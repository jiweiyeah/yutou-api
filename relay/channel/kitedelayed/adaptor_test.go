package kitedelayed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestAdaptorDoRequestAutoDisablesOnlyMarathonOnInsufficientCredits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalAutomaticDisable := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = false
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = originalAutomaticDisable
	})

	tests := []struct {
		name        string
		channelName string
		statusCode  int
		response    map[string]any
		wantDisable bool
	}{
		{
			name:        "marathon insufficient credits",
			channelName: "marathon",
			statusCode:  http.StatusPaymentRequired,
			response:    map[string]any{"detail": "insufficient credits"},
			wantDisable: true,
		},
		{
			name:        "other channel insufficient credits",
			channelName: "another-kite-channel",
			statusCode:  http.StatusPaymentRequired,
			response:    map[string]any{"detail": "insufficient credits"},
			wantDisable: false,
		},
		{
			name:        "marathon other payment error",
			channelName: "marathon",
			statusCode:  http.StatusPaymentRequired,
			response:    map[string]any{"detail": "payment method required"},
			wantDisable: false,
		},
		{
			name:        "marathon insufficient credits with other status",
			channelName: "marathon",
			statusCode:  http.StatusForbidden,
			response:    map[string]any{"detail": "insufficient credits"},
			wantDisable: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSONResponse(t, w, test.statusCode, test.response)
			}))
			t.Cleanup(server.Close)

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			common.SetContextKey(ctx, constant.ContextKeyChannelName, test.channelName)
			info := testRelayInfo(server.URL, false)
			adaptor := &Adaptor{}
			adaptor.Init(info)

			_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
			require.Error(t, err)

			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, test.wantDisable, types.IsChannelAutoDisableError(apiErr))
			assert.Equal(t, test.wantDisable, service.ShouldDisableChannel(apiErr))
			assert.True(t, types.IsSkipRetryError(apiErr))
		})
	}
}

func TestAdaptorDoRequestMapsOnlyMarathonReservationFailuresToTemporaryErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalAutomaticDisable := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = originalAutomaticDisable
	})

	tests := []struct {
		name           string
		channelName    string
		response       map[string]any
		wantStatus     int
		wantRetryAfter int
		wantDisable    bool
	}{
		{
			name:           "marathon reservation failure",
			channelName:    "marathon",
			response:       map[string]any{"detail": "insufficient credits for concurrent job reservation"},
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: reservationRetryAfterSeconds,
		},
		{
			name:           "other channel reservation failure",
			channelName:    "another-kite-channel",
			response:       map[string]any{"detail": "insufficient credits for concurrent job reservation"},
			wantStatus:     http.StatusPaymentRequired,
			wantRetryAfter: 0,
		},
		{
			name:           "marathon exhausted credits remains non-retryable",
			channelName:    "marathon",
			response:       map[string]any{"detail": "insufficient credits"},
			wantStatus:     http.StatusPaymentRequired,
			wantRetryAfter: 0,
			wantDisable:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSONResponse(t, w, http.StatusPaymentRequired, test.response)
			}))
			t.Cleanup(server.Close)

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			common.SetContextKey(ctx, constant.ContextKeyChannelName, test.channelName)
			info := testRelayInfo(server.URL, false)
			adaptor := &Adaptor{}
			adaptor.Init(info)

			_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
			require.Error(t, err)

			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.True(t, types.IsSkipRetryError(apiErr), "the adaptor owns reservation retries and must not trigger an outer relay retry")
			assert.Equal(t, test.wantStatus, apiErr.StatusCode)
			assert.Equal(t, test.wantRetryAfter, types.GetRetryAfterSeconds(apiErr))
			assert.Equal(t, test.wantDisable, service.ShouldDisableChannel(apiErr))
		})
	}
}

func TestAdaptorDoRequestRotatesMarathonKeyBeforeSubmitIsAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))

	oldDB := model.DB
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	oldRetryTimes := common.RetryTimes
	t.Cleanup(func() {
		model.DB = oldDB
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
		common.RetryTimes = oldRetryTimes
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RetryTimes = 2

	var submitKeys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == submitPath {
			submitKeys = append(submitKeys, r.Header.Get("Authorization"))
			if r.Header.Get("Authorization") == "Bearer key-a" {
				writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{"detail": "insufficient credits for concurrent job reservation"})
				return
			}
			writeJSONResponse(t, w, http.StatusAccepted, map[string]any{"id": "job_rotated", "status": "queued"})
			return
		}
		switch r.URL.Path {
		case "/v1/delayed/jobs/job_rotated":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "job_rotated", "status": "succeeded"})
		case "/v1/delayed/jobs/job_rotated/result":
			writeJSONResponse(t, w, http.StatusOK, chatCompletionResult())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	autoBan := 1
	channel := &model.Channel{
		Id:      10821,
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "marathon",
		Key:     "key-a\nkey-b\nkey-c",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         3,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, db.Create(channel).Error)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	common.SetContextKey(ctx, constant.ContextKeyChannelName, "marathon")
	common.SetContextKey(ctx, constant.ContextKeyChannelKey, "key-a")
	common.SetContextKey(ctx, constant.ContextKeyChannelMultiKeyIndex, 0)
	info := testRelayInfo(server.URL, false)
	info.ChannelId = channel.Id
	info.ChannelIsMultiKey = true
	info.ChannelMultiKeyIndex = 0
	info.ApiKey = "key-a"
	adaptor := &Adaptor{}
	adaptor.Init(info)

	response, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.IsType(t, &http.Response{}, response)
	response.(*http.Response).Body.Close()
	assert.Equal(t, []string{"Bearer key-a", "Bearer key-b"}, submitKeys)
	assert.Equal(t, "key-b", info.ApiKey)
	assert.Equal(t, 1, info.ChannelMultiKeyIndex)
	assert.Equal(t, "key-b", common.GetContextKeyString(ctx, constant.ContextKeyChannelKey))
	assert.Equal(t, 1, common.GetContextKeyInt(ctx, constant.ContextKeyChannelMultiKeyIndex))
}

func TestAdaptorDoRequestLimitsMarathonReservationRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))

	oldDB := model.DB
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	oldRetryTimes := common.RetryTimes
	t.Cleanup(func() {
		model.DB = oldDB
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
		common.RetryTimes = oldRetryTimes
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RetryTimes = 1

	var submitKeys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == submitPath {
			submitKeys = append(submitKeys, r.Header.Get("Authorization"))
			writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{"detail": "insufficient credits for concurrent job reservation"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	autoBan := 1
	channel := &model.Channel{
		Id:      10822,
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "marathon",
		Key:     "key-a\nkey-b\nkey-c",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         3,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, db.Create(channel).Error)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	common.SetContextKey(ctx, constant.ContextKeyChannelName, "marathon")
	common.SetContextKey(ctx, constant.ContextKeyChannelKey, "key-a")
	info := testRelayInfo(server.URL, false)
	info.ChannelId = channel.Id
	info.ChannelIsMultiKey = true
	info.ChannelMultiKeyIndex = 0
	info.ApiKey = "key-a"
	adaptor := &Adaptor{}
	adaptor.Init(info)

	_, err = adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, []string{"Bearer key-a", "Bearer key-b"}, submitKeys)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	assert.Equal(t, reservationRetryAfterSeconds, types.GetRetryAfterSeconds(apiErr))
	assert.True(t, types.IsSkipRetryError(apiErr))
}

func TestAdaptorDoRequestBuffersResultBeforePollContextCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resultBody, err := common.Marshal(chatCompletionResult())
	require.NoError(t, err)

	client := service.GetHttpClient()
	require.NotNil(t, client)
	originalTransport := client.Transport
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		switch req.URL.Path {
		case submitPath:
			body = []byte(`{"id":"job_buffered","status":"queued"}`)
		case "/v1/delayed/jobs/job_buffered":
			body = []byte(`{"id":"job_buffered","status":"succeeded"}`)
		case "/v1/delayed/jobs/job_buffered/result":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: &contextReadCloser{
					ctx:    req.Context(),
					reader: bytes.NewReader(resultBody),
				},
				Request: req,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request path %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() {
		client.Transport = originalTransport
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo("https://example.com", false)
	adaptor := &Adaptor{}
	adaptor.Init(info)

	response, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	httpResp := response.(*http.Response)

	usageValue, apiErr := adaptor.DoResponse(ctx, httpResp, info)
	require.Nil(t, apiErr)
	usage, ok := usageValue.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 13, usage.PromptTokens)
	assert.Equal(t, 8, usage.CompletionTokens)
}

func TestAdaptorDoResponseMarksPostSubmitErrorsAsNonRetryable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			info := testRelayInfo("https://example.com", stream)
			adaptor := &Adaptor{}
			adaptor.Init(info)

			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       &failingReadCloser{err: context.Canceled},
			}

			_, apiErr := adaptor.DoResponse(ctx, response, info)
			require.NotNil(t, apiErr)
			assert.True(t, types.IsSkipRetryError(apiErr))
			assert.Contains(t, apiErr.Error(), context.Canceled.Error())
		})
	}
}

func TestAdaptorDoRequestReportsClientCancellationWithoutGatewayTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	requestCtx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case submitPath:
			writeJSONResponse(t, w, http.StatusAccepted, map[string]any{"id": "job_canceled", "status": "queued"})
		case "/v1/delayed/jobs/job_canceled":
			cancel()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL, false)
	adaptor := &Adaptor{}
	adaptor.Init(info)

	_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`))
	require.Error(t, err)

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, clientClosedStatus, apiErr.StatusCode)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Contains(t, apiErr.Error(), "canceled by client")
}

func TestAdaptorDoRequestKeepsSlowStreamAliveUntilRequestTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.Equal(t, 5*time.Minute, pollTimeout)

	originalPollInterval := pollInterval
	originalHeartbeatInterval := pollHeartbeatInterval
	pollInterval = 5 * time.Millisecond
	pollHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		pollInterval = originalPollInterval
		pollHeartbeatInterval = originalHeartbeatInterval
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case submitPath:
			writeJSONResponse(t, w, http.StatusAccepted, map[string]any{"id": "job_slow", "status": "queued"})
		case "/v1/delayed/jobs/job_slow":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "job_slow", "status": "running"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL, true)
	adaptor := &Adaptor{}
	adaptor.Init(info)

	_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	require.Error(t, err)

	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	assert.Contains(t, recorder.Body.String(), ": PING\n\n")
	assert.True(t, info.IsStream)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type contextReadCloser struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}

func (r *contextReadCloser) Close() error {
	return nil
}

type failingReadCloser struct {
	err error
}

func (r *failingReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *failingReadCloser) Close() error {
	return nil
}
