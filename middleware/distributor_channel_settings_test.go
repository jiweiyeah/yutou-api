package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ===== CUSTOM START: 渠道级 error_rewrite 在重试路径上的上下文刷新 =====
//
// 重试会重新选渠道（controller.getChannel -> service.CacheGetRandomSatisfiedChannel），
// 因此每次尝试都必须用「本次选中的渠道」覆盖 context 里的渠道级配置。
// 否则第 N 次尝试仍然带着首次选中渠道的 settings，渠道级 error_rewrite
// 会在换渠道之后失效。
func TestSetupContextForSelectedChannelRefreshesPerAttemptChannelSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	first := &model.Channel{Id: 11, Name: "first", Type: constant.ChannelTypeOpenAI, Key: "sk-first"}
	first.SetOtherSettings(dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{Match: "first-only", StatusCode: http.StatusBadGateway}},
	})
	require.Nil(t, SetupContextForSelectedChannel(c, first, "gpt-4o"))
	firstSettings, ok := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	require.True(t, ok)
	require.Len(t, firstSettings.ErrorRewrite, 1)
	require.Equal(t, 11, common.GetContextKeyInt(c, constant.ContextKeyChannelId))

	// The retry picks a different channel; its settings must replace the stale ones.
	second := &model.Channel{Id: 12, Name: "second", Type: constant.ChannelTypeOpenAI, Key: "sk-second"}
	second.SetOtherSettings(dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{Match: "second-only", StatusCode: http.StatusServiceUnavailable}},
	})
	require.Nil(t, SetupContextForSelectedChannel(c, second, "gpt-4o"))

	secondSettings, ok := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	require.True(t, ok)
	require.Len(t, secondSettings.ErrorRewrite, 1)
	require.Equal(t, "second-only", secondSettings.ErrorRewrite[0].Match,
		"a retried attempt must see the newly selected channel's error_rewrite rules")
	require.Equal(t, 12, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
}

// A channel without rules must clear the rules of the previous attempt rather
// than leave them in place: the rewrite is per selected channel, not sticky.
func TestSetupContextForSelectedChannelClearsRewriteRulesFromPreviousAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	withRules := &model.Channel{Id: 21, Name: "with-rules", Type: constant.ChannelTypeOpenAI, Key: "sk-a"}
	withRules.SetOtherSettings(dto.ChannelOtherSettings{
		ErrorRewrite: []dto.ChannelErrorRewriteRule{{Match: "sticky", StatusCode: http.StatusBadGateway}},
	})
	require.Nil(t, SetupContextForSelectedChannel(c, withRules, "gpt-4o"))

	withoutRules := &model.Channel{Id: 22, Name: "no-rules", Type: constant.ChannelTypeOpenAI, Key: "sk-b"}
	require.Nil(t, SetupContextForSelectedChannel(c, withoutRules, "gpt-4o"))

	settings, ok := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	require.True(t, ok)
	require.Empty(t, settings.ErrorRewrite)
}

// ===== CUSTOM END =====
