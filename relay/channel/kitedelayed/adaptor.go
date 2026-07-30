package kitedelayed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const (
	ChannelName        = "kite_delayed"
	submitPath         = "/v1/delayed/chat/completions"
	maximumRequestSize = 32 << 20
	maximumBodySize    = 64 << 20
	pollInterval       = 500 * time.Millisecond
	pollTimeout        = 2 * time.Minute
)

var ModelList = []string{
	"kimi-k3",
	"deepseek-v4-pro",
	"glm-5.2",
	"qwen3.6-35b-a3b",
	"nemotron-3-ultra",
}

type Adaptor struct {
	openai.Adaptor
}

type jobResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  any    `json:"error"`
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
	a.Adaptor.Init(info)
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("relay info is nil")
	}
	if info.RelayFormat == types.RelayFormatOpenAI && info.RelayMode != relayconstant.RelayModeChatCompletions {
		return "", fmt.Errorf("Kite Delayed only supports /v1/chat/completions")
	}
	return strings.TrimRight(info.ChannelBaseUrl, "/") + submitPath, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	requestJSON, err := readLimited(requestBody, maximumRequestSize)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("read Kite Delayed request body failed: %w", err),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	var payload map[string]any
	if err := common.Unmarshal(requestJSON, &payload); err != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("decode Kite Delayed request body failed: %w", err),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if len(payload) == 0 {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("Kite Delayed request body is empty"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	// This adaptor exposes a synchronous OpenAI-compatible contract while the
	// upstream is asynchronous. Always use the immediate queue and request a
	// buffered result; streaming is synthesized after the result is available.
	requestJSON, err = sjson.SetBytes(requestJSON, "completion_window", "now")
	if err != nil {
		return nil, types.NewError(
			fmt.Errorf("set Kite Delayed completion window failed: %w", err),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	requestJSON, err = sjson.SetBytes(requestJSON, "stream", false)
	if err != nil {
		return nil, types.NewError(
			fmt.Errorf("disable Kite Delayed upstream streaming failed: %w", err),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	requestJSON, err = sjson.DeleteBytes(requestJSON, "stream_options")
	if err != nil {
		return nil, types.NewError(
			fmt.Errorf("remove Kite Delayed stream options failed: %w", err),
			types.ErrorCodeConvertRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}

	submitURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	clientRequestedStream := info.IsStream
	info.IsStream = false
	defer func() {
		info.IsStream = clientRequestedStream
	}()

	info.UpstreamRequestBodySize = int64(len(requestJSON))
	submitResp, err := a.doJSONRequest(c, info, c.Request.Context(), http.MethodPost, submitURL, bytes.NewReader(requestJSON))
	if err != nil {
		return nil, kiteRequestError("submit", err, http.StatusBadGateway)
	}
	submitBody, err := readAndCloseResponse(submitResp, maximumBodySize)
	if err != nil {
		return nil, kiteRequestError("read submit response", err, http.StatusBadGateway)
	}
	if submitResp.StatusCode < http.StatusOK || submitResp.StatusCode >= http.StatusMultipleChoices {
		return nil, kiteHTTPError("submit", submitResp.StatusCode, submitBody)
	}

	var job jobResponse
	if err := common.Unmarshal(submitBody, &job); err != nil {
		return nil, kiteRequestError("decode submit response", err, http.StatusBadGateway)
	}
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" {
		return nil, kiteRequestError("submit", fmt.Errorf("upstream response did not contain a job id"), http.StatusBadGateway)
	}

	pollCtx, cancel := context.WithTimeout(c.Request.Context(), pollTimeout)
	defer cancel()

	baseURL := strings.TrimRight(info.ChannelBaseUrl, "/")
	jobID := url.PathEscape(job.ID)
	statusURL := baseURL + "/v1/delayed/jobs/" + jobID
	resultURL := statusURL + "/result"

	for {
		statusResp, err := a.doJSONRequest(c, info, pollCtx, http.MethodGet, statusURL, nil)
		if err != nil {
			if pollCtx.Err() != nil {
				return nil, kiteRequestError("poll", pollCtx.Err(), http.StatusGatewayTimeout)
			}
			return nil, kiteRequestError("poll", err, http.StatusBadGateway)
		}
		statusBody, err := readAndCloseResponse(statusResp, maximumBodySize)
		if err != nil {
			return nil, kiteRequestError("read poll response", err, http.StatusBadGateway)
		}
		if statusResp.StatusCode < http.StatusOK || statusResp.StatusCode >= http.StatusMultipleChoices {
			return nil, kiteHTTPError("poll", statusResp.StatusCode, statusBody)
		}

		job = jobResponse{}
		if err := common.Unmarshal(statusBody, &job); err != nil {
			return nil, kiteRequestError("decode poll response", err, http.StatusBadGateway)
		}

		switch strings.ToLower(strings.TrimSpace(job.Status)) {
		case "succeeded":
			resultResp, err := a.doJSONRequest(c, info, pollCtx, http.MethodGet, resultURL, nil)
			if err != nil {
				return nil, kiteRequestError("fetch result", err, http.StatusBadGateway)
			}
			if resultResp.StatusCode < http.StatusOK || resultResp.StatusCode >= http.StatusMultipleChoices {
				resultBody, readErr := readAndCloseResponse(resultResp, maximumBodySize)
				if readErr != nil {
					return nil, kiteRequestError("read result response", readErr, http.StatusBadGateway)
				}
				return nil, kiteHTTPError("fetch result", resultResp.StatusCode, resultBody)
			}
			return resultResp, nil
		case "queued", "running", "pending", "processing", "in_progress":
			// Keep polling below.
		case "failed", "cancelled", "canceled", "expired":
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("Kite Delayed job %s: %s", job.Status, jobErrorMessage(job.Error)),
				types.ErrorCodeDoRequestFailed,
				http.StatusBadGateway,
				types.ErrOptionWithSkipRetry(),
			)
		default:
			return nil, kiteRequestError(
				"poll",
				fmt.Errorf("unexpected job status %q", job.Status),
				http.StatusBadGateway,
			)
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-pollCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, kiteRequestError("poll", pollCtx.Err(), http.StatusGatewayTimeout)
		case <-timer.C:
		}
	}
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if !info.IsStream {
		return a.Adaptor.DoResponse(c, resp, info)
	}

	resultBody, err := readAndCloseResponse(resp, maximumBodySize)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	streamBody, err := buildChatCompletionStream(resultBody)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
	}

	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(streamBody)),
	}
	streamResp.Header.Set("Content-Type", "text/event-stream")
	helper.SetEventStreamHeaders(c)
	return a.Adaptor.DoResponse(c, streamResp, info)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func (a *Adaptor) doJSONRequest(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	ctx context.Context,
	method string,
	requestURL string,
	body io.Reader,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		req.Body = http.NoBody
	}
	if body != nil && info.UpstreamRequestBodySize > 0 {
		req.ContentLength = info.UpstreamRequestBodySize
	}

	headers := req.Header
	if err := a.SetupRequestHeader(c, &headers, info); err != nil {
		return nil, err
	}
	if method == http.MethodGet {
		req.Header.Del("Content-Type")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	headerOverride, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range headerOverride {
		req.Header.Set(key, value)
		if strings.EqualFold(key, "Host") {
			req.Host = value
		}
	}

	return channel.DoRequest(c, req, info)
}

func buildChatCompletionStream(resultBody []byte) ([]byte, error) {
	var result map[string]any
	if err := common.Unmarshal(resultBody, &result); err != nil {
		return nil, fmt.Errorf("decode Kite Delayed result failed: %w", err)
	}

	var parsed dto.OpenAITextResponse
	if err := common.Unmarshal(resultBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode Kite Delayed chat completion failed: %w", err)
	}
	if upstreamError := parsed.GetOpenAIError(); upstreamError != nil && upstreamError.Type != "" {
		return nil, fmt.Errorf("Kite Delayed result error: %s", upstreamError.Message)
	}

	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("Kite Delayed result did not contain choices")
	}

	baseChunk := map[string]any{
		"id":      result["id"],
		"object":  "chat.completion.chunk",
		"created": result["created"],
		"model":   result["model"],
	}
	for _, key := range []string{"system_fingerprint", "service_tier"} {
		if value, exists := result[key]; exists {
			baseChunk[key] = value
		}
	}

	contentChunks := make([]map[string]any, 0, len(choices))
	finishChoices := make([]any, 0, len(choices))
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Kite Delayed result contained an invalid choice")
		}
		delta, ok := choice["message"].(map[string]any)
		if !ok {
			delta = map[string]any{}
		}

		deltas := []map[string]any{delta}
		reasoning := ""
		if value, exists := delta["reasoning_content"].(string); exists {
			reasoning = value
		} else if value, exists := delta["reasoning"].(string); exists {
			reasoning = value
		}
		content, hasContent := delta["content"]
		if reasoning != "" && hasContent && content != nil && content != "" {
			reasoningDelta := make(map[string]any, len(delta))
			for key, value := range delta {
				if key != "content" {
					reasoningDelta[key] = value
				}
			}
			deltas = []map[string]any{
				reasoningDelta,
				{"content": content},
			}
		}

		for deltaIndex, messageDelta := range deltas {
			contentChoice := map[string]any{
				"index":         choice["index"],
				"delta":         messageDelta,
				"finish_reason": nil,
			}
			if logprobs, exists := choice["logprobs"]; exists && deltaIndex == 0 {
				contentChoice["logprobs"] = logprobs
			}
			contentChunk := cloneChunk(baseChunk)
			contentChunk["choices"] = []any{contentChoice}
			contentChunks = append(contentChunks, contentChunk)
		}

		finishChoice := map[string]any{
			"index":         choice["index"],
			"delta":         map[string]any{},
			"finish_reason": choice["finish_reason"],
		}
		if nativeFinishReason, exists := choice["native_finish_reason"]; exists {
			finishChoice["native_finish_reason"] = nativeFinishReason
		}
		finishChoices = append(finishChoices, finishChoice)
	}

	chunks := make([]map[string]any, 0, len(contentChunks)+2)
	chunks = append(chunks, contentChunks...)

	finishChunk := cloneChunk(baseChunk)
	finishChunk["choices"] = finishChoices
	chunks = append(chunks, finishChunk)

	if usage, exists := result["usage"]; exists && usage != nil {
		usageChunk := cloneChunk(baseChunk)
		usageChunk["choices"] = []any{}
		usageChunk["usage"] = usage
		chunks = append(chunks, usageChunk)
	}

	var stream bytes.Buffer
	for _, chunk := range chunks {
		data, err := common.Marshal(chunk)
		if err != nil {
			return nil, fmt.Errorf("encode Kite Delayed stream chunk failed: %w", err)
		}
		stream.WriteString("data: ")
		stream.Write(data)
		stream.WriteString("\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	return stream.Bytes(), nil
}

func cloneChunk(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+2)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("body is nil")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return body, nil
}

func readAndCloseResponse(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	defer service.CloseResponseBodyGracefully(resp)
	return readLimited(resp.Body, limit)
}

func kiteRequestError(operation string, err error, statusCode int) *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		fmt.Errorf("Kite Delayed %s failed: %w", operation, err),
		types.ErrorCodeDoRequestFailed,
		statusCode,
		types.ErrOptionWithSkipRetry(),
	)
}

func kiteHTTPError(operation string, statusCode int, body []byte) *types.NewAPIError {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return kiteRequestError(operation, fmt.Errorf("upstream returned HTTP %d: %s", statusCode, message), statusCode)
}

func jobErrorMessage(value any) string {
	if value == nil {
		return "upstream job failed without an error message"
	}
	if message, ok := value.(string); ok && strings.TrimSpace(message) != "" {
		return message
	}
	encoded, err := common.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

var _ channel.Adaptor = (*Adaptor)(nil)
