package middleware

import (
	"crypto/subtle"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// RateLimitBypassHeader 携带共享密钥的请求头。值与 common.RateLimitBypassToken
// 常量时间相等的请求跳过 IP 维度限流（rateLimitFactory 生成的 GW / GA / CT /
// DW / UP）。用户维度（userRateLimitFactory）、模型维度限流以及其余中间件一概
// 不受影响；豁免的请求也不占用桶，不会挤掉同 IP 上其他调用方的名额。
//
// 见 docs/custom/rate-limit-bypass.md。
const RateLimitBypassHeader = "X-RateLimit-Bypass-Token"

func isRateLimitBypassed(c *gin.Context) bool {
	secret := common.RateLimitBypassToken
	if secret == "" {
		return false
	}
	provided := c.GetHeader(RateLimitBypassHeader)
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) == 1
}
