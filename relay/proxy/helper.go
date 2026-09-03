package proxy

import (
	"context"
	"net/http"

	"github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

// WrapHTTPClientWithProxy 为特定渠道包装 HTTP 客户端，支持动态代理切换
func WrapHTTPClientWithProxy(c *gin.Context, info *common.RelayInfo, baseClient *http.Client) *http.Client {
	if info == nil || info.ChannelId == 0 {
		return baseClient
	}

	manager := GetDegradedManager()
	if !manager.IsEnabled(info.ChannelId) {
		return baseClient
	}

	// 应用排队延迟（代理模式下）
	manager.ApplyQueueDelay(c.Request.Context(), info.ChannelId)

	// 返回适合当前模式的 HTTP 客户端
	return manager.GetHTTPClient(info.ChannelId)
}

// RecordResponseForProxy 记录响应，用于动态调整代理模式
func RecordResponseForProxy(info *common.RelayInfo, statusCode int, isSuccess bool) {
	if info == nil || info.ChannelId == 0 {
		return
	}

	manager := GetDegradedManager()
	if !manager.IsEnabled(info.ChannelId) {
		return
	}

	manager.RecordResponse(info.ChannelId, statusCode, isSuccess)
}

// GetProxyStats 获取渠道代理统计信息
func GetProxyStats(channelID int) map[string]interface{} {
	manager := GetDegradedManager()
	return manager.GetStats(channelID)
}

// ResetProxyState 重置渠道代理状态
func ResetProxyState(channelID int) {
	manager := GetDegradedManager()
	manager.ResetState(channelID)
}

// WithProxyContext 在 context 中标记是否使用代理
func WithProxyContext(ctx context.Context, useProxy bool) context.Context {
	return context.WithValue(ctx, "use_proxy", useProxy)
}

// IsUsingProxy 检查 context 是否使用代理
func IsUsingProxy(ctx context.Context) bool {
	val := ctx.Value("use_proxy")
	if val == nil {
		return false
	}
	useProxy, ok := val.(bool)
	return ok && useProxy
}
