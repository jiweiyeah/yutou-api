package kitedelayed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

const (
	ChannelName = "kite_delayed"
	// 上游 gokite 有两条互不相干的线，共用同一个渠道类型，靠 ChannelBaseUrl
	// 的后缀（routerMarker）区分：
	//   Marathon  （异步 job）: POST /v1/delayed/chat/completions
	//   Kite Router（同步）    : POST /v1/chat/completions
	submitPath = "/v1/delayed/chat/completions"
	// Kite Router 线的 base_url 标记。base_url 填成
	// `https://delayed-inference.prod.gokite.ai/kite-router` 即走 Router 线；
	// 不带这个后缀则仍是 Marathon 线（既有渠道行为完全不变）。
	//
	// 用 base_url 而不是模型名来分流：kimi-k3 / deepseek-v4-pro 两条线都有，
	// 按模型名没法表达「新渠道走 Router、老渠道继续走 Marathon」这种灰度。
	routerMarker = "/kite-router"
	// Kite Router 上游只提供 chat completions，三种入口（chat / responses /
	// messages）都打到这个路径，格式转换由 relayconvert 在进出上游前后完成。
	routerPath                   = "/v1/chat/completions"
	maximumRequestSize           = 32 << 20
	maximumBodySize              = 64 << 20
	pollTimeout                  = 5 * time.Minute
	clientClosedStatus           = 499
	reservationRetryAfterSeconds = 2
)

var (
	pollInterval          = 500 * time.Millisecond
	pollHeartbeatInterval = 15 * time.Second
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

// kiteRouterBaseURL 判断这个渠道是不是 Kite Router 线，并返回剥掉标记后的 base URL。
func kiteRouterBaseURL(channelBaseURL string) (string, bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(channelBaseURL), "/")
	if !strings.HasSuffix(strings.ToLower(trimmed), routerMarker) {
		return "", false
	}
	return strings.TrimRight(trimmed[:len(trimmed)-len(routerMarker)], "/"), true
}

func isKiteRouterChannel(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	_, ok := kiteRouterBaseURL(info.ChannelBaseUrl)
	return ok
}

// ConvertOpenAIResponsesRequest 在 Router 线上把 responses 请求转成 chat completions
// 请求 —— 上游 Kite Router 只认 chat，直接透传 responses body 会因缺 `messages`
// 字段被回 422 Field required。Marathon 线保持原有行为（原样透传）。
//
// 这里同时是预算闸门（§5.1）的挂载点：转换完成、发上游之前先判断这次请求是否
// 注定超过上游 $5 额度上限，注定失败就直接返回可执行的明确错误。
func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	if !isKiteRouterChannel(info) {
		return a.Adaptor.ConvertOpenAIResponsesRequest(c, info, request)
	}
	// 上一轮压缩产生的 compaction 块要先还原成普通消息，否则会被转换层当成
	// 未知类型 → 空内容消息，历史整段丢失。
	if _, err := expandKiteRouterCompactionItems(&request); err != nil {
		return nil, types.NewErrorWithStatusCode(
			err,
			types.ErrorCode("compaction_block_invalid"),
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	// Codex 的远程压缩 v2 就是一次普通的 /v1/responses，只是 input 末尾多一条
	// compaction_trigger。命中即改写为「调便宜模型做摘要」。
	compactionRequested, err := stripKiteRouterCompactionTrigger(&request)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			err, types.ErrorCodeConvertRequestFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	// Codex 的 Responses Lite 把工具藏在 input 的 additional_tools 项里，先提升成
	// chat 能表达的形状；否则转换层会把整段 tools 丢掉（详见 codex_lite.go）。
	toolSpecs, err := hoistCodexLiteTools(&request)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			err,
			types.ErrorCodeConvertRequestFailed,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if len(toolSpecs) > 0 {
		c.Set(codexLiteContextKey, toolSpecs)
	}
	if info.RelayMode == relayconstant.RelayModeResponsesCompact || compactionRequested {
		// 注意不要把 *types.NewAPIError 直接当 error 返回：typed nil 会变成非 nil
		// 接口，调用方看到的是一次「空消息」的失败。
		compactionBody, apiErr := a.buildKiteRouterCompactionRequest(c, info, request, compactionRequested)
		if apiErr != nil {
			return nil, apiErr
		}
		return compactionBody, nil
	}
	converted, err := relayconvert.ResponsesRequestToChatCompletionsRequest(&request)
	if err != nil {
		return nil, err
	}
	if apiErr := a.applyKiteRouterBudgetGate(
		c,
		info,
		info.UpstreamModelName,
		kiteRouterPromptBytes(converted),
		kiteRouterRequestedOutputTokens(converted),
	); apiErr != nil {
		return nil, apiErr
	}
	return converted, nil
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("relay info is nil")
	}
	if base, ok := kiteRouterBaseURL(info.ChannelBaseUrl); ok {
		// Router 线上游只有 chat completions；responses / messages 入口在进来
		// 之前已被转成 chat 请求，所以这里三种 RelayMode 都打同一个路径。
		return base + routerPath, nil
	}
	if info.RelayFormat == types.RelayFormatOpenAI && info.RelayMode != relayconstant.RelayModeChatCompletions {
		return "", fmt.Errorf("Kite Delayed only supports /v1/chat/completions")
	}
	return strings.TrimRight(info.ChannelBaseUrl, "/") + submitPath, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	if isKiteRouterChannel(info) {
		return a.doKiteRouterRequest(c, info, requestBody)
	}
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
	var submitBody []byte
	attemptedKeys := map[string]struct{}{info.ApiKey: {}}
	for submitAttempt := 0; ; submitAttempt++ {
		submitResp, err := a.doJSONRequest(c, info, c.Request.Context(), http.MethodPost, submitURL, bytes.NewReader(requestJSON))
		if err != nil {
			return nil, kiteRequestError("submit", err, http.StatusBadGateway)
		}
		submitBody, err = readAndCloseResponse(submitResp, maximumBodySize)
		if err != nil {
			return nil, kiteRequestError("read submit response", err, http.StatusBadGateway)
		}
		if submitResp.StatusCode >= http.StatusOK && submitResp.StatusCode < http.StatusMultipleChoices {
			break
		}

		apiErr := kiteHTTPError("submit", submitResp.StatusCode, submitBody)
		isMarathon := strings.EqualFold(strings.TrimSpace(common.GetContextKeyString(c, constant.ContextKeyChannelName)), "marathon")
		if submitResp.StatusCode != http.StatusPaymentRequired || !isMarathon {
			return nil, apiErr
		}

		var upstreamError struct {
			Detail string `json:"detail"`
		}
		if common.Unmarshal(submitBody, &upstreamError) != nil {
			return nil, apiErr
		}

		switch strings.ToLower(strings.TrimSpace(upstreamError.Detail)) {
		case "insufficient credits":
			// This is a definitive key-level credit failure. Preserve the existing
			// automatic-disable behavior for Marathon only.
			types.ErrOptionWithChannelAutoDisable()(apiErr)
			return nil, apiErr
		case "insufficient credits for concurrent job reservation":
			// The submit was rejected before a job was created. Rotate only within
			// this Marathon multi-key channel; submitted jobs remain non-retryable.
			apiErr.StatusCode = http.StatusServiceUnavailable
			types.ErrOptionWithRetryAfter(reservationRetryAfterSeconds)(apiErr)
			if submitAttempt >= common.RetryTimes || !info.ChannelIsMultiKey {
				return nil, apiErr
			}

			channel, channelErr := model.CacheGetChannel(info.ChannelId)
			if channelErr != nil {
				logger.LogWarn(c, fmt.Sprintf("Kite Delayed Marathon reservation retry could not load channel %d: %v", info.ChannelId, channelErr))
				return nil, apiErr
			}
			var nextKey string
			var nextIndex int
			var nextErr *types.NewAPIError
			for range channel.GetKeys() {
				nextKey, nextIndex, nextErr = channel.GetNextEnabledKey()
				if nextErr != nil {
					break
				}
				if _, alreadyAttempted := attemptedKeys[nextKey]; !alreadyAttempted {
					break
				}
				nextKey = ""
			}
			if nextErr != nil || strings.TrimSpace(nextKey) == "" {
				if nextErr != nil {
					logger.LogWarn(c, fmt.Sprintf("Kite Delayed Marathon reservation retry could not select next key for channel %d: %v", info.ChannelId, nextErr))
				}
				return nil, apiErr
			}

			logger.LogWarn(c, fmt.Sprintf("Kite Delayed Marathon reservation unavailable on key index %d; retrying with key index %d (%d/%d)", info.ChannelMultiKeyIndex, nextIndex, submitAttempt+1, common.RetryTimes))
			attemptedKeys[nextKey] = struct{}{}
			info.ApiKey = nextKey
			info.ChannelMultiKeyIndex = nextIndex
			common.SetContextKey(c, constant.ContextKeyChannelKey, nextKey)
			common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, nextIndex)
			continue
		default:
			return nil, apiErr
		}
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

	var stopHeartbeat context.CancelFunc
	var heartbeatDone <-chan struct{}
	if clientRequestedStream {
		heartbeatCtx, stop := context.WithCancel(pollCtx)
		stopHeartbeat = stop
		done := make(chan struct{})
		heartbeatDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(pollHeartbeatInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					helper.SetEventStreamHeaders(c)
					helper.ExtendWriteDeadline(c)
					if err := helper.PingData(c); err != nil {
						cancel()
						return
					}
				case <-heartbeatCtx.Done():
					return
				}
			}
		}()
		defer func() {
			stopHeartbeat()
			<-heartbeatDone
		}()
	}

	baseURL := strings.TrimRight(info.ChannelBaseUrl, "/")
	jobID := url.PathEscape(job.ID)
	statusURL := baseURL + "/v1/delayed/jobs/" + jobID
	resultURL := statusURL + "/result"

	for {
		statusResp, err := a.doJSONRequest(c, info, pollCtx, http.MethodGet, statusURL, nil)
		if err != nil {
			if pollCtx.Err() != nil {
				return nil, kitePollContextError(pollCtx.Err())
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
			resultBody, readErr := readAndCloseResponse(resultResp, maximumBodySize)
			if readErr != nil {
				return nil, kiteRequestError("read result response", readErr, http.StatusBadGateway)
			}
			if resultResp.StatusCode < http.StatusOK || resultResp.StatusCode >= http.StatusMultipleChoices {
				return nil, kiteHTTPError("fetch result", resultResp.StatusCode, resultBody)
			}

			// The result request uses pollCtx. Buffer it before DoRequest returns so
			// the deferred cancel cannot invalidate the body consumed by DoResponse.
			resultResp.Body = io.NopCloser(bytes.NewReader(resultBody))
			resultResp.ContentLength = int64(len(resultBody))
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
			return nil, kitePollContextError(pollCtx.Err())
		case <-timer.C:
		}
	}
}

// doKiteRouterRequest 处理 Kite Router 线：同步请求，且上游不支持流式。
//
// 上游对 stream=true 直接回 400（`Kite Router Phase 1 does not support stream=true`），
// 所以这里对上游一律按非流式发；客户端要流式时 DoResponse 会把完整结果用
// buildChatCompletionStream 合成为 SSE —— 和 Marathon 线复用同一套合成逻辑。
func (a *Adaptor) doKiteRouterRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	requestJSON, err := readLimited(requestBody, maximumRequestSize)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("read Kite Router request body failed: %w", err),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if len(requestJSON) == 0 {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("Kite Router request body is empty"),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	clientRequestedStream := info.IsStream
	info.IsStream = false
	defer func() {
		info.IsStream = clientRequestedStream
	}()

	// 上游 Kite Router 是同步接口、且不支持流式，所以从发出请求到拿到完整结果
	// 这一段（长输出实测要 690 秒）对客户端是**完全静默**的 —— 会被 Cloudflare
	// 的 ~100s 源站超时掐断并回 524。Marathon 线靠轮询循环里的心跳撑住，
	// 这里必须补上同样的事，否则流式长输出必挂。
	upstreamCtx := c.Request.Context()
	if clientRequestedStream {
		heartbeatCtx, cancel := context.WithCancel(upstreamCtx)
		upstreamCtx = heartbeatCtx
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(pollHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					helper.SetEventStreamHeaders(c)
					helper.ExtendWriteDeadline(c)
					if err := helper.PingData(c); err != nil {
						// 客户端已断开，连带中止上游请求
						cancel()
						return
					}
				case <-heartbeatCtx.Done():
					return
				}
			}
		}()
		defer func() {
			cancel()
			<-done
		}()
	}

	requestJSON, err = sjson.SetBytes(requestJSON, "stream", false)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("set Kite Router stream flag failed: %w", err),
			types.ErrorCodeConvertRequestFailed,
			http.StatusInternalServerError,
			types.ErrOptionWithSkipRetry(),
		)
	}
	requestJSON, err = sjson.DeleteBytes(requestJSON, "stream_options")
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("remove Kite Router stream options failed: %w", err),
			types.ErrorCodeConvertRequestFailed,
			http.StatusInternalServerError,
			types.ErrOptionWithSkipRetry(),
		)
	}

	requestURL, err := a.GetRequestURL(info)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	info.UpstreamRequestBodySize = int64(len(requestJSON))

	// 按估算的预检金额挑 key：池子里约 3% 的 key 已被耗尽，随机抽签会偶发 402。
	// 拿不到可信余额时这里什么都不做（保持随机选 key 的既有行为）。
	if budget := kiteRouterBudgetFrom(c); budget != nil {
		key, index, pick := a.kiteRouterPickKey(c, info, budget.RequiredUSD)
		switch pick {
		case kiteRouterKeyPickSelected:
			logger.LogInfo(c, fmt.Sprintf("Kite Router budget $%.4f requires a healthier key on channel %d: key index %d -> %d",
				budget.RequiredUSD, info.ChannelId, info.ChannelMultiKeyIndex, index))
			info.ApiKey = key
			info.ChannelMultiKeyIndex = index
			common.SetContextKey(c, constant.ContextKeyChannelKey, key)
			common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, index)
		case kiteRouterKeyPickNoneAffordable:
			return nil, a.kiteRouterNoAffordableKeyError(c, info, budget)
		}
	}

	resp, err := a.doJSONRequest(c, info, upstreamCtx, http.MethodPost, requestURL, bytes.NewReader(requestJSON))
	if err != nil {
		return nil, kiteRequestError("router", err, http.StatusBadGateway)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, readErr := readAndCloseResponse(resp, maximumBodySize)
		if readErr != nil {
			return nil, kiteRequestError("read router response", readErr, http.StatusBadGateway)
		}
		// 402 是上游的额度预检拒绝：记下这个 key 已不够用，并把上游的原始错误
		// 换成可执行的人话（设计文档 §7 明确要求绝不透传）。
		if resp.StatusCode == http.StatusPaymentRequired && kiteRouterIsInsufficientBalance(body) {
			kiteRouterMarkKeyDrained(info.ChannelId, info.ChannelMultiKeyIndex)
			return nil, a.kiteRouterUpstreamBalanceError(c, info, body)
		}
		return nil, kiteHTTPError("router", resp.StatusCode, body)
	}
	return resp, nil
}

// doKiteRouterResponses 处理 Router 线的 /v1/responses 入口（Codex 等客户端）。
//
// 上游只提供 chat completions，所以这里把拿到的 chat 响应（或合成的 chat SSE）
// 用 relayconvert 转成 responses 格式再输出 —— 与 gemini 渠道
// （relay/channel/gemini/relay_responses.go）的做法一致。
func (a *Adaptor) doKiteRouterResponses(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	resultBody, err := readAndCloseResponse(resp, maximumBodySize)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(resultBody, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if upstreamError := chatResp.GetOpenAIError(); upstreamError != nil && upstreamError.Type != "" {
		return nil, types.WithOpenAIError(*upstreamError, resp.StatusCode)
	}

	if !info.IsStream {
		convertResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		responsesResp, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
		if !ok {
			return nil, types.NewOpenAIError(
				fmt.Errorf("expected OpenAI responses response, got %T", convertResult.Value),
				types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		// Codex Lite 的 custom 工具（exec）要按 custom_tool_call 回给客户端。
		applyCodexLiteOutput(responsesResp.Output, codexLiteTools(c))
		responseBody, err := common.Marshal(responsesResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
		}
		service.IOCopyBytesGracefully(c, resp, responseBody)
		// 必须回指针：responses_handler.go 用裸断言 usage.(*dto.Usage) 取值，
		// 返回值类型会 panic（interface conversion: dto.Usage, not *dto.Usage）。
		return &chatResp.Usage, nil
	}

	// 流式：先把完整结果合成为 chat SSE，再逐 chunk 转成 responses 事件流。
	streamBody, err := buildChatCompletionStream(resultBody)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	state, err := relayconvert.NewResponseStreamState(
		types.RelayFormatOpenAI,
		types.RelayFormatOpenAIResponses,
		relayconvert.ResponseStreamOptions{
			ID:      helper.GetResponseID(c),
			Model:   info.UpstreamModelName,
			Created: common.GetTimestamp(),
		},
	)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	helper.SetEventStreamHeaders(c)
	liteRewriter := newCodexLiteStreamRewriter(codexLiteTools(c))
	sendEvents := func(results []relayconvert.ResponseResult) *types.NewAPIError {
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				continue
			}
			outboundEvent := event.Payload
			outboundEvent.Type = event.Type
			for _, outbound := range liteRewriter.rewrite(outboundEvent) {
				payload, err := common.Marshal(outbound)
				if err != nil {
					return types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
				}
				helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: outbound.Type}, string(payload))
			}
		}
		return nil
	}

	var usage *dto.Usage
	for _, chunk := range parseChatCompletionStreamChunks(streamBody) {
		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if apiErr := sendEvents(results); apiErr != nil {
			return nil, apiErr
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage != nil {
		state.SetUsage(usage)
	}
	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if apiErr := sendEvents(finalResults); apiErr != nil {
		return nil, apiErr
	}
	return usage, nil
}

// parseChatCompletionStreamChunks 把 buildChatCompletionStream 产出的 SSE 文本
// 还原成 chunk 序列（跳过非 data 行与 [DONE]）。
func parseChatCompletionStreamChunks(streamBody []byte) []dto.ChatCompletionsStreamResponse {
	chunks := make([]dto.ChatCompletionsStreamResponse, 0, 8)
	for _, line := range strings.Split(string(streamBody), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(payload, &chunk); err != nil {
			continue
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if isKiteRouterChannel(info) {
		switch info.RelayMode {
		case relayconstant.RelayModeResponses:
			if plan := kiteRouterCompactionPlanFrom(c); plan != nil && plan.V2 {
				return a.doKiteRouterCompactionV2Stream(c, resp, info)
			}
			return a.doKiteRouterResponses(c, resp, info)
		case relayconstant.RelayModeResponsesCompact:
			return a.doKiteRouterCompaction(c, resp, info)
		}
	}
	if !info.IsStream {
		usage, apiErr := a.Adaptor.DoResponse(c, resp, info)
		return usage, kitePostSubmitError(apiErr)
	}

	resultBody, err := readAndCloseResponse(resp, maximumBodySize)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}
	streamBody, err := buildChatCompletionStream(resultBody)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}

	streamResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(streamBody)),
	}
	streamResp.Header.Set("Content-Type", "text/event-stream")
	helper.SetEventStreamHeaders(c)
	usage, apiErr := a.Adaptor.DoResponse(c, streamResp, info)
	return usage, kitePostSubmitError(apiErr)
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

func kitePollContextError(err error) *types.NewAPIError {
	if errors.Is(err, context.Canceled) {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("Kite Delayed poll canceled by client: %w", err),
			types.ErrorCodeDoRequestFailed,
			clientClosedStatus,
			types.ErrOptionWithSkipRetry(),
		)
	}
	return kiteRequestError("poll", err, http.StatusGatewayTimeout)
}

func kitePostSubmitError(err *types.NewAPIError) *types.NewAPIError {
	if err == nil {
		return nil
	}
	types.ErrOptionWithSkipRetry()(err)
	return err
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
