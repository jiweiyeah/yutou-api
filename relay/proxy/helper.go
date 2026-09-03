package proxy

import (
	"context"

	"github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// ResolveProxyURL 返回渠道在当前降级状态下应使用的代理地址，空字符串表示直连。
// 处于代理模式时会先应用排队延迟，再返回代理地址。
//
// 返回值直接传给 service.GetHttpClientWithProxy，由项目统一的客户端工厂负责
// 超时、重定向校验、TLS 与连接池配置，这里不自行构造 http.Client。
func ResolveProxyURL(c *gin.Context, info *common.RelayInfo) string {
	if info == nil || info.ChannelId == 0 {
		return ""
	}

	manager := GetDegradedManager()
	if !manager.IsEnabled(info.ChannelId) {
		return ""
	}

	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	manager.ApplyQueueDelay(ctx, info.ChannelId)

	return manager.ResolveProxyURL(info.ChannelId)
}

// RecordResponseForProxy 记录响应，用于驱动降级/恢复状态机
func RecordResponseForProxy(info *common.RelayInfo, statusCode int, isSuccess bool) {
	if info == nil || info.ChannelId == 0 {
		return
	}
	GetDegradedManager().RecordResponse(info.ChannelId, statusCode, isSuccess)
}

// GetProxyStats 获取渠道代理统计信息
func GetProxyStats(channelID int) map[string]interface{} {
	return GetDegradedManager().GetStats(channelID)
}

// ResetProxyState 重置渠道代理状态
func ResetProxyState(channelID int) {
	GetDegradedManager().ResetState(channelID)
}
