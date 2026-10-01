package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const atriaUpstreamMessage = "Atria-Dawn-Preview is not supported by TokenPlan"

func rewriteTestContext(t *testing.T, settings dto.ChannelOtherSettings, model string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if model != "" {
		c.Set("original_model", model)
	}
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, settings)
	return c
}

// atriaUpstreamError reproduces what channel 10864 gets back today: an OpenAI
// shaped error whose message misattributes an oversized body to a model/plan
// problem, carried on HTTP 404.
func atriaUpstreamError() *types.NewAPIError {
	return types.WithOpenAIError(types.OpenAIError{
		Message: atriaUpstreamMessage,
		Type:    "model_not_available",
		Code:    "model_not_available",
	}, http.StatusNotFound)
}

func atriaRewriteSettings() dto.ChannelOtherSettings {
	return dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{
			{
				Match:      "is not supported by TokenPlan",
				StatusCode: http.StatusRequestEntityTooLarge,
				Message:    "请求体过大：本次 {body_kb} KB（{body_bytes} 字节），超出上游 {model} 的上限。请压缩上下文或新开会话后重试。",
				Code:       "request_too_large",
				SkipRetry:  true,
			},
		},
	}
}

func TestApplyChannelErrorRewriteReplacesMessageStatusAndCode(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, atriaRewriteSettings(), "deepseek/deepseek-v4-flash")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 1075320)

	require.Equal(t, http.StatusRequestEntityTooLarge, got.StatusCode)
	require.Equal(t, types.ErrorCode("request_too_large"), got.GetErrorCode())
	require.True(t, types.IsSkipRetryError(got), "rewritten rule must stop the retry loop")

	payload := got.ToOpenAIError()
	require.Equal(t, "request_too_large", payload.Code)
	require.Equal(t, "request_too_large", payload.Type)
	require.Contains(t, payload.Message, "1051 KB")
	require.Contains(t, payload.Message, "1075320")
	require.Contains(t, payload.Message, "deepseek/deepseek-v4-flash")
	require.NotContains(t, payload.Message, atriaUpstreamMessage)
}

func TestApplyChannelErrorRewriteMatchesCaseInsensitively(t *testing.T) {
	t.Parallel()

	settings := atriaRewriteSettings()
	settings.ErrorRewrite[0].Match = "NOT SUPPORTED BY tokenplan"
	c := rewriteTestContext(t, settings, "")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 1)

	require.Equal(t, http.StatusRequestEntityTooLarge, got.StatusCode)
}

func TestApplyChannelErrorRewriteLeavesUnrelatedErrorsAlone(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, atriaRewriteSettings(), "")
	err := types.WithOpenAIError(types.OpenAIError{
		Message: "Cluster RPM rate limit exceeded.",
		Type:    "rate_limit_error",
		Code:    "rate_limit_exceeded",
	}, http.StatusTooManyRequests)

	got := ApplyChannelErrorRewrite(c, err, 4096)

	require.Equal(t, http.StatusTooManyRequests, got.StatusCode)
	require.Equal(t, types.ErrorCode("rate_limit_exceeded"), got.GetErrorCode())
	require.False(t, types.IsSkipRetryError(got))
	require.Equal(t, "Cluster RPM rate limit exceeded.", got.ToOpenAIError().Message)
}

// A rule must never be able to mask an error the gateway itself produced: those
// messages are already accurate and carry no upstream response.
func TestApplyChannelErrorRewriteSkipsGatewayGeneratedErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "new_api_error",
			err:  types.NewErrorWithStatusCode(errTestSentinel{msg: atriaUpstreamMessage}, types.ErrorCodeBadRequestBody, http.StatusBadRequest),
		},
		{
			name: "channel error",
			err:  types.NewErrorWithStatusCode(errTestSentinel{msg: atriaUpstreamMessage}, types.ErrorCodeChannelNoAvailableKey, http.StatusInternalServerError),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := rewriteTestContext(t, atriaRewriteSettings(), "")
			got := ApplyChannelErrorRewrite(c, tc.err, 1024)

			require.Equal(t, tc.err.StatusCode, got.StatusCode)
			require.Equal(t, tc.err.GetErrorCode(), got.GetErrorCode())
			require.False(t, types.IsSkipRetryError(got))
		})
	}
}

func TestApplyChannelErrorRewriteIsNoOpWithoutRules(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, dto.ChannelOtherSettings{}, "")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 1024)

	require.Equal(t, http.StatusNotFound, got.StatusCode)
	require.Equal(t, atriaUpstreamMessage, got.ToOpenAIError().Message)
}

// An empty match would rewrite every upstream failure on the channel, so it is
// rejected at config time and ignored at runtime.
func TestApplyChannelErrorRewriteIgnoresEmptyMatch(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{Match: "", StatusCode: http.StatusRequestEntityTooLarge}},
	}, "")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 1024)

	require.Equal(t, http.StatusNotFound, got.StatusCode)
}

func TestApplyChannelErrorRewriteKeepsUpstreamMessageWhenRuleHasNone(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{
			Match:      "TokenPlan",
			StatusCode: http.StatusRequestEntityTooLarge,
		}},
	}, "")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 1024)

	require.Equal(t, http.StatusRequestEntityTooLarge, got.StatusCode)
	require.Equal(t, atriaUpstreamMessage, got.ToOpenAIError().Message)
}

func TestApplyChannelErrorRewriteExpandsUpstreamPlaceholder(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{
			Match:   "TokenPlan",
			Message: "upstream said: {upstream_message}",
		}},
	}, "")
	err := atriaUpstreamError()

	got := ApplyChannelErrorRewrite(c, err, 0)

	require.Equal(t, "upstream said: "+atriaUpstreamMessage, got.ToOpenAIError().Message)
}

func TestApplyChannelErrorRewriteRewritesClaudePayload(t *testing.T) {
	t.Parallel()

	c := rewriteTestContext(t, atriaRewriteSettings(), "tencent/hy3")
	err := types.WithClaudeError(types.ClaudeError{
		Type:    "invalid_request_error",
		Message: atriaUpstreamMessage,
	}, http.StatusNotFound)

	got := ApplyChannelErrorRewrite(c, err, 2048)

	require.Equal(t, http.StatusRequestEntityTooLarge, got.StatusCode)
	claude := got.ToClaudeError()
	require.Equal(t, "request_too_large", claude.Type)
	require.NotContains(t, claude.Message, atriaUpstreamMessage)
}

type errTestSentinel struct{ msg string }

func (e errTestSentinel) Error() string { return e.msg }
