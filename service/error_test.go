package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	// ===== CUSTOM START: 自动禁用关键词/上游 body 相关测试依赖 =====
	"github.com/QuantumNous/new-api/setting/operation_setting"
	// ===== CUSTOM END =====
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResetStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		statusCode       int
		statusCodeConfig string
		expectedCode     int
	}{
		{
			name:             "map string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"503"}`,
			expectedCode:     503,
		},
		{
			name:             "map int value",
			statusCode:       429,
			statusCodeConfig: `{"429":503}`,
			expectedCode:     503,
		},
		{
			name:             "skip invalid string value",
			statusCode:       429,
			statusCodeConfig: `{"429":"bad-code"}`,
			expectedCode:     429,
		},
		{
			name:             "skip status code 200",
			statusCode:       200,
			statusCodeConfig: `{"200":503}`,
			expectedCode:     200,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newAPIError := &types.NewAPIError{
				StatusCode: tc.statusCode,
			}
			ResetStatusCode(newAPIError, tc.statusCodeConfig)
			require.Equal(t, tc.expectedCode, newAPIError.StatusCode)
		})
	}
}

func TestRelayErrorHandlerTruncatesInvalidJSONBodyInLog(t *testing.T) {
	withDebugEnabled(t, false)

	body := strings.Repeat("b", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, "bad response status code 500", newAPIError.Error())
	require.Contains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), fmt.Sprintf("original_length=%d", len(body)))
	require.NotContains(t, logBuffer.String(), strings.Repeat("b", common.LocalLogContentLimit+1))
}

func TestRelayErrorHandlerKeepsStructuredErrorMessage(t *testing.T) {
	message := strings.Repeat("c", common.LocalLogContentLimit+256)
	body := `{"message":"` + message + `"}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

func TestRelayErrorHandlerKeepsOpenAIErrorMessage(t *testing.T) {
	message := strings.Repeat("d", common.LocalLogContentLimit+256)
	body := `{"error":{"message":"` + message + `","type":"server_error","code":"server_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, message, newAPIError.Error())
}

// ===== CUSTOM START: 顶层 message/type/code 形态的上游错误识别测试 =====
// 有些上游不把错误嵌在 "error" 对象里，而是把 message/type/code 平铺在顶层。
// 这里锁定该形态仍能被解析成 OpenAI 形状的错误，并保留上游的 error code。
func TestRelayErrorHandlerKeepsTopLevelOpenAIErrorCode(t *testing.T) {
	body := `{"message":"upstream rejected the request","type":"invalid_request_error","code":"invalid_request_error"}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.Equal(t, types.ErrorCode("invalid_request_error"), newAPIError.GetErrorCode())
	require.Equal(t, "upstream rejected the request", newAPIError.Error())
}

// ===== CUSTOM END =====

func TestRelayErrorHandlerKeepsInvalidJSONBodyInDebugLog(t *testing.T) {
	withDebugEnabled(t, true)

	body := strings.Repeat("e", common.LocalLogContentLimit+256)
	var logBuffer bytes.Buffer

	common.LogWriterMu.Lock()
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logBuffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = oldWriter
		common.LogWriterMu.Unlock()
	})

	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	require.NotContains(t, logBuffer.String(), "[truncated")
	require.Contains(t, logBuffer.String(), body)
}

func withDebugEnabled(t *testing.T, enabled bool) {
	t.Helper()

	oldDebug := common.DebugEnabled
	common.DebugEnabled = enabled
	t.Cleanup(func() {
		common.DebugEnabled = oldDebug
	})
}

// ===== CUSTOM START: 上游 body 保留与自动禁用关键词匹配测试 =====
func TestRelayErrorHandlerRetainsUpstreamBodyForNonJSON(t *testing.T) {
	body := "Braintrust gateway error: Braintrust model 'glm-5.3-flash' unauthorized, insufficient credit balance"
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.NotNil(t, newAPIError)
	// The client facing message stays generic and must not leak the upstream body.
	require.Equal(t, "bad response status code 403", newAPIError.Error())
	// The body is still retained so auto-disable keyword matching can read it.
	require.Equal(t, body, newAPIError.UpstreamBody())
}

func TestRelayErrorHandlerTruncatesLongUpstreamBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", upstreamBodyKeywordLimit+100))),
	}

	newAPIError := RelayErrorHandler(context.Background(), resp, false)

	require.Len(t, newAPIError.UpstreamBody(), upstreamBodyKeywordLimit)
}

func TestShouldDisableChannelMatchesKeywordsInUpstreamBody(t *testing.T) {
	oldKeywords := operation_setting.AutomaticDisableKeywords
	oldEnabled := common.AutomaticDisableChannelEnabled
	t.Cleanup(func() {
		operation_setting.AutomaticDisableKeywords = oldKeywords
		common.AutomaticDisableChannelEnabled = oldEnabled
	})
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableKeywords = []string{"INSUFFICIENT"}

	t.Run("plain text body still disables channel", func(t *testing.T) {
		apiErr := types.NewError(errors.New("bad response status code 403"),
			types.ErrorCodeBadResponseStatusCode,
			types.ErrOptionWithStatusCode(http.StatusForbidden),
			types.ErrOptionWithUpstreamBody("unauthorized, insufficient credit balance"))
		require.True(t, ShouldDisableChannel(apiErr))
	})

	t.Run("generic message alone keeps channel enabled", func(t *testing.T) {
		apiErr := types.NewError(errors.New("bad response status code 403"),
			types.ErrorCodeBadResponseStatusCode,
			types.ErrOptionWithStatusCode(http.StatusForbidden))
		require.False(t, ShouldDisableChannel(apiErr))
	})

	t.Run("body without keyword keeps channel enabled", func(t *testing.T) {
		apiErr := types.NewError(errors.New("bad response status code 403"),
			types.ErrorCodeBadResponseStatusCode,
			types.ErrOptionWithStatusCode(http.StatusForbidden),
			types.ErrOptionWithUpstreamBody("temporarily unavailable"))
		require.False(t, ShouldDisableChannel(apiErr))
	})
}

// ===== CUSTOM END =====
