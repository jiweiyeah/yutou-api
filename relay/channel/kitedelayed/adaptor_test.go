package kitedelayed

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	service.InitHttpClient()
	constant.StreamingTimeout = 300
	os.Exit(m.Run())
}

func TestAdaptorDoRequestPollsAndNormalizesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	receivedPayload := make(chan map[string]any, 1)
	var statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == submitPath:
			var payload map[string]any
			if err := common.DecodeJson(r.Body, &payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			receivedPayload <- payload
			writeJSONResponse(t, w, http.StatusAccepted, map[string]any{
				"id":     "job_test",
				"status": "queued",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/delayed/jobs/job_test":
			if statusCalls.Add(1) == 1 {
				writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "job_test", "status": "running"})
				return
			}
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "job_test", "status": "succeeded"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/delayed/jobs/job_test/result":
			writeJSONResponse(t, w, http.StatusOK, chatCompletionResult())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL, true)
	adaptor := &Adaptor{}
	adaptor.Init(info)

	response, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{
		"model":"glm-5.2",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true,
		"stream_options":{"include_usage":true},
		"completion_window":"later"
	}`))
	require.NoError(t, err)
	require.IsType(t, &http.Response{}, response)
	defer response.(*http.Response).Body.Close()

	payload := <-receivedPayload
	assert.Equal(t, "now", payload["completion_window"])
	assert.Equal(t, false, payload["stream"])
	assert.NotContains(t, payload, "stream_options")
	assert.Equal(t, int32(2), statusCalls.Load())
	assert.True(t, info.IsStream)
}

func TestAdaptorDoRequestMarksFailedJobAsNonRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case submitPath:
			writeJSONResponse(t, w, http.StatusAccepted, map[string]any{"id": "job_failed", "status": "queued"})
		case "/v1/delayed/jobs/job_failed":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":     "job_failed",
				"status": "failed",
				"error":  map[string]any{"message": "provider rejected the request"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL, false)
	adaptor := &Adaptor{}
	adaptor.Init(info)

	_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
	require.Error(t, err)

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.True(t, types.IsSkipRetryError(apiErr), "a submitted paid job must not be duplicated by relay retry")
	assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "provider rejected the request")
}

func TestAdaptorDoResponseSynthesizesOpenAIStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	info := testRelayInfo("https://example.com", true)
	info.ShouldIncludeUsage = true
	adaptor := &Adaptor{}
	adaptor.Init(info)

	resultBody, err := common.Marshal(chatCompletionResult())
	require.NoError(t, err)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(resultBody))),
	}

	usageValue, apiErr := adaptor.DoResponse(ctx, response, info)
	require.Nil(t, apiErr)
	usage, ok := usageValue.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 13, usage.PromptTokens)
	assert.Equal(t, 8, usage.CompletionTokens)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))

	body := recorder.Body.String()
	assert.Contains(t, body, `"object":"chat.completion.chunk"`)
	assert.Contains(t, body, `"content":"你好"`)
	assert.Contains(t, body, `"reasoning_details"`)
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.Contains(t, body, `"prompt_tokens":13`)
	assert.Contains(t, body, "data: [DONE]")
}

func TestAdaptorDoResponseConvertsReasoningAndTextToClaudeStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	info := testRelayInfo("https://example.com", true)
	info.RelayFormat = types.RelayFormatClaude
	info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{
		LastMessagesType: relaycommon.LastMessageTypeNone,
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	resultBody, err := common.Marshal(chatCompletionResult())
	require.NoError(t, err)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(resultBody))),
	}

	_, apiErr := adaptor.DoResponse(ctx, response, info)
	require.Nil(t, apiErr)

	body := recorder.Body.String()
	assert.Contains(t, body, `"type":"thinking_delta"`)
	assert.Contains(t, body, `"thinking":"greeting analysis"`)
	assert.Contains(t, body, `"type":"text_delta"`)
	assert.Contains(t, body, `"text":"你好"`)
	assert.Contains(t, body, "event: message_stop")
}

func TestAdaptorDoResponseConvertsToolCallToClaudeStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	info := testRelayInfo("https://example.com", true)
	info.RelayFormat = types.RelayFormatClaude
	info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{
		LastMessagesType: relaycommon.LastMessageTypeNone,
	}
	adaptor := &Adaptor{}
	adaptor.Init(info)

	resultBody, err := common.Marshal(toolCallCompletionResult())
	require.NoError(t, err)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(resultBody))),
	}

	usageValue, apiErr := adaptor.DoResponse(ctx, response, info)
	require.Nil(t, apiErr)
	usage, ok := usageValue.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 166, usage.PromptTokens)
	assert.Equal(t, 28, usage.CompletionTokens)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))

	body := recorder.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"name":"get_weather"`)
	assert.Contains(t, body, `"partial_json":"{\"city\":\"北京\"}"`)
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
	assert.Contains(t, body, "event: message_stop")
}

func testRelayInfo(baseURL string, stream bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		IsStream:    stream,
		RelayMode:   relayconstant.RelayModeChatCompletions,
		RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeKiteDelayed,
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-key",
			UpstreamModelName: "glm-5.2",
		},
	}
}

func chatCompletionResult() map[string]any {
	return map[string]any{
		"id":      "chatcmpl_test",
		"object":  "chat.completion",
		"created": 1785418437,
		"model":   "GLM 5.2",
		"choices": []any{
			map[string]any{
				"index":                0,
				"finish_reason":        "stop",
				"native_finish_reason": "stop",
				"message": map[string]any{
					"role":      "assistant",
					"content":   "你好",
					"reasoning": "greeting analysis",
					"reasoning_details": []any{
						map[string]any{"type": "reasoning.text", "text": "greeting analysis"},
					},
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     13,
			"completion_tokens": 8,
			"total_tokens":      21,
		},
	}
}

func toolCallCompletionResult() map[string]any {
	return map[string]any{
		"id":      "chatcmpl_tool_test",
		"object":  "chat.completion",
		"created": 1785425796,
		"model":   "GLM 5.2",
		"choices": []any{
			map[string]any{
				"index":                0,
				"finish_reason":        "tool_calls",
				"native_finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"type":  "function",
							"index": 0,
							"id":    "call_weather",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city":"北京"}`,
							},
						},
					},
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     166,
			"completion_tokens": 28,
			"total_tokens":      194,
		},
	}
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, statusCode int, value any) {
	t.Helper()
	body, err := common.Marshal(value)
	if err != nil {
		t.Errorf("marshal test response: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, err := w.Write(body); err != nil {
		t.Errorf("write test response: %v", err)
	}
}
