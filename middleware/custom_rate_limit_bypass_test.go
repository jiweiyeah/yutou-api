package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 受信任调用方的限流豁免契约：
//   - 未配置 RATE_LIMIT_BYPASS_TOKEN 时，请求头被完全忽略；
//   - 配置后，只有与之相等的头才跳过 IP 维度限流；
//   - 错误的头与没有头一样受限；
//   - 豁免的请求不占桶。

func withRateLimitBypassToken(t *testing.T, token string) {
	t.Helper()
	previous := common.RateLimitBypassToken
	common.RateLimitBypassToken = token
	t.Cleanup(func() { common.RateLimitBypassToken = previous })
}

func withInMemoryRateLimiter(t *testing.T) {
	t.Helper()
	previous := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previous })
}

// 桶容量 1、窗口 60 秒：同一 IP 的第二个未豁免请求必然 429。
// mark 每个用例独立，避免共享的 inMemoryRateLimiter 键互相污染。
func newRateLimitedRouter(t *testing.T, mark string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/limited", rateLimitFactory(1, 60, mark), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return router
}

func performLimitedRequest(router *gin.Engine, header string) int {
	req := httptest.NewRequest(http.MethodGet, "/limited", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	if header != "" {
		req.Header.Set(RateLimitBypassHeader, header)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func TestRateLimitBypassGating(t *testing.T) {
	withInMemoryRateLimiter(t)

	tests := []struct {
		name       string
		configured string
		header     string
		wantSecond int
	}{
		{name: "未配置时请求头被忽略", configured: "", header: "secret", wantSecond: http.StatusTooManyRequests},
		{name: "正确的头跳过限流", configured: "secret", header: "secret", wantSecond: http.StatusOK},
		{name: "错误的头照常受限", configured: "secret", header: "wrong", wantSecond: http.StatusTooManyRequests},
		{name: "没有头照常受限", configured: "secret", header: "", wantSecond: http.StatusTooManyRequests},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withRateLimitBypassToken(t, tt.configured)
			router := newRateLimitedRouter(t, fmt.Sprintf("bypass-gating-%d-", i))
			require.Equal(t, http.StatusOK, performLimitedRequest(router, tt.header))
			assert.Equal(t, tt.wantSecond, performLimitedRequest(router, tt.header))
		})
	}
}

func TestRateLimitBypassDoesNotConsumeBucket(t *testing.T) {
	// 豁免不是「先放行再计数」：豁免请求之后，同 IP 的普通请求仍有完整的桶。
	withInMemoryRateLimiter(t)
	withRateLimitBypassToken(t, "secret")
	router := newRateLimitedRouter(t, "bypass-bucket-")

	require.Equal(t, http.StatusOK, performLimitedRequest(router, "secret"))
	require.Equal(t, http.StatusOK, performLimitedRequest(router, "secret"))
	assert.Equal(t, http.StatusOK, performLimitedRequest(router, ""))
	assert.Equal(t, http.StatusTooManyRequests, performLimitedRequest(router, ""))
}
