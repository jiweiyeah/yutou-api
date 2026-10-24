package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelRedisRateLimitUsesUTCRegardlessOfLocalTimezone(t *testing.T) {
	redisServer, redisClient := useRateLimitMiniRedis(t)
	previousLocation := time.Local
	time.Local = time.FixedZone("test-utc-plus-eight", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })

	ctx := context.Background()
	recordKey := "rateLimit:model-utc-record"
	recordRedisRequest(ctx, redisClient, recordKey, 2)
	recorded, err := redisClient.LIndex(ctx, recordKey, 0).Result()
	require.NoError(t, err)
	recordedAt, err := time.Parse(modelRateLimitTimeFormat, recorded)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), recordedAt, 2*time.Second)

	checkKey := "rateLimit:model-utc-check"
	withinWindow := time.Now().UTC().Add(-30 * time.Second).Format(modelRateLimitTimeFormat)
	_, err = redisServer.Push(checkKey, withinWindow, withinWindow)
	require.NoError(t, err)
	allowed, err := checkRedisRateLimit(ctx, redisClient, checkKey, 2, 60)
	require.NoError(t, err)
	assert.False(t, allowed, "an existing UTC timestamp inside the window must remain limited on a non-UTC host")
}

var modelRateLimitTestUsers atomic.Int64

func TestModelRateLimitStreamFailuresDoNotConsumeSuccessLimit(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		for _, totalLimit := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/total=%d", backend, totalLimit), func(t *testing.T) {
				userID := 7200000 + int(modelRateLimitTestUsers.Add(1))
				handler := memoryRateLimitHandler(60, totalLimit, 1)
				if backend == "redis" {
					useRateLimitMiniRedis(t)
					handler = redisRateLimitHandler(60, totalLimit, 1)
				}
				router := gin.New()
				router.GET("/:outcome", func(c *gin.Context) { c.Set("id", userID) }, handler, func(c *gin.Context) {
					status := relaycommon.NewStreamStatus()
					if c.Param("outcome") == "failed" {
						status.MarkFailed("server_error", "", 0)
					} else {
						status.MarkCompleted()
					}
					common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, status)
					c.Status(http.StatusOK)
				})
				assert.Equal(t, http.StatusOK, performRateLimitRequest(router, "/failed", "127.0.0.1:1000").Code)
				if totalLimit > 0 {
					assert.Equal(t, http.StatusOK, performRateLimitRequest(router, "/failed", "127.0.0.1:1000").Code)
				} else {
					assert.Equal(t, http.StatusOK, performRateLimitRequest(router, "/completed", "127.0.0.1:1000").Code)
				}
				assert.Equal(t, http.StatusTooManyRequests, performRateLimitRequest(router, "/completed", "127.0.0.1:1000").Code)
			})
		}
	}
}

func TestModelMemoryRateLimitReservesConcurrentSuccessAdmission(t *testing.T) {
	userID := 7200000 + int(modelRateLimitTestUsers.Add(1))
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	router := gin.New()
	router.GET("/:outcome", func(c *gin.Context) { c.Set("id", userID) }, memoryRateLimitHandler(60, 0, 1), func(c *gin.Context) {
		if c.Param("outcome") == "failed" {
			close(entered)
			<-release
			status := relaycommon.NewStreamStatus()
			status.MarkFailed("server_error", "", 0)
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, status)
		}
		c.Status(http.StatusOK)
	})
	go func() {
		defer close(finished)
		assert.Equal(t, http.StatusOK, performRateLimitRequest(router, "/failed", "127.0.0.1:1000").Code)
	}()
	<-entered
	assert.Equal(t, http.StatusTooManyRequests, performRateLimitRequest(router, "/completed", "127.0.0.1:1000").Code)
	close(release)
	<-finished
	assert.Equal(t, http.StatusOK, performRateLimitRequest(router, "/completed", "127.0.0.1:1000").Code)
	assert.Equal(t, http.StatusTooManyRequests, performRateLimitRequest(router, "/completed", "127.0.0.1:1000").Code)
}

// configureFreeModelRateLimit 把免费模型限流切到测试配置，并在用例结束时把
// 全局变量与倍率表还原。
func configureFreeModelRateLimit(t *testing.T, groups string, count int, concurrency int) {
	t.Helper()
	previousEnabled := common.FreeModelRateLimitEnabled
	previousGroups := common.FreeModelRateLimitGroups
	previousCount := common.FreeModelRateLimitCount
	previousWindow := common.FreeModelRateLimitWindowMin
	previousConcurrency := common.FreeModelRateLimitConcurrency
	previousRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() {
		common.FreeModelRateLimitEnabled = previousEnabled
		common.FreeModelRateLimitGroups = previousGroups
		common.FreeModelRateLimitCount = previousCount
		common.FreeModelRateLimitWindowMin = previousWindow
		common.FreeModelRateLimitConcurrency = previousConcurrency
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
	})

	require.NoError(t, i18n.Init())
	common.FreeModelRateLimitEnabled = true
	common.FreeModelRateLimitGroups = groups
	common.FreeModelRateLimitCount = count
	common.FreeModelRateLimitWindowMin = 60
	common.FreeModelRateLimitConcurrency = concurrency
	// 价格表里没有这两个模型，免费与否完全由倍率决定。
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"free-model-ratelimit-test":0,"paid-model-ratelimit-test":1}`))
}

func newFreeModelRateLimitTestRouter(userID int, group string) *gin.Engine {
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("id", userID)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
	}, FreeModelRateLimit(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return router
}

func postFreeModelRateLimit(router http.Handler, model string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestFreeModelRateLimitAppliesToFreeModelsInConfiguredGroupsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRateLimitMiniRedis(t)
	configureFreeModelRateLimit(t, "y-api-paid", 2, 0)

	userID := 7300000 + int(modelRateLimitTestUsers.Add(1))
	otherGroupRouter := newFreeModelRateLimitTestRouter(userID, "y-api")
	for range 3 {
		assert.Equal(t, http.StatusOK, postFreeModelRateLimit(otherGroupRouter, "free-model-ratelimit-test").Code,
			"groups outside FREE_MODEL_RATE_LIMIT_GROUPS stay unlimited")
	}

	limitedRouter := newFreeModelRateLimitTestRouter(userID, "y-api-paid")
	for range 3 {
		assert.Equal(t, http.StatusOK, postFreeModelRateLimit(limitedRouter, "paid-model-ratelimit-test").Code,
			"paid models must not consume the free model allowance")
	}
	assert.Equal(t, http.StatusOK, postFreeModelRateLimit(limitedRouter, "free-model-ratelimit-test").Code)
	assert.Equal(t, http.StatusOK, postFreeModelRateLimit(limitedRouter, "free-model-ratelimit-test").Code)
	limited := postFreeModelRateLimit(limitedRouter, "free-model-ratelimit-test")
	assert.Equal(t, http.StatusTooManyRequests, limited.Code)
	assert.Equal(t, "3600", limited.Header().Get("Retry-After"))
}

func TestFreeModelRateLimitCapsConcurrentFreeModelRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRateLimitMiniRedis(t)
	configureFreeModelRateLimit(t, "", 0, 1)

	userID := 7300000 + int(modelRateLimitTestUsers.Add(1))
	entered, release := make(chan struct{}), make(chan struct{})
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("id", userID)
	}, FreeModelRateLimit(), func(c *gin.Context) {
		if c.GetHeader("X-Test-Block") != "" {
			close(entered)
			<-release
		}
		c.Status(http.StatusOK)
	})

	finished := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"free-model-ratelimit-test"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Test-Block", "1")
		router.ServeHTTP(recorder, request)
		finished <- recorder.Code
	}()
	<-entered
	assert.Equal(t, http.StatusTooManyRequests, postFreeModelRateLimit(router, "free-model-ratelimit-test").Code)
	assert.Equal(t, http.StatusOK, postFreeModelRateLimit(router, "paid-model-ratelimit-test").Code,
		"concurrency cap only covers free models")
	close(release)
	assert.Equal(t, http.StatusOK, <-finished)
	assert.Equal(t, http.StatusOK, postFreeModelRateLimit(router, "free-model-ratelimit-test").Code,
		"a finished request must release its concurrency slot")
}

func TestFreeModelRateLimitDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configureFreeModelRateLimit(t, "", 1, 1)
	common.FreeModelRateLimitEnabled = false

	router := newFreeModelRateLimitTestRouter(7300000+int(modelRateLimitTestUsers.Add(1)), "y-api-paid")
	for range 3 {
		assert.Equal(t, http.StatusOK, postFreeModelRateLimit(router, "free-model-ratelimit-test").Code)
	}
}

// 计费侧会先把 @thinking:on 之类的修饰符解析成基础模型名再查价，限流若直接
// 拿原始名查表就会被修饰符绕过。
func TestFreeModelRateLimitResolvesModelModifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRateLimitMiniRedis(t)
	configureFreeModelRateLimit(t, "", 1, 0)

	router := newFreeModelRateLimitTestRouter(7300000+int(modelRateLimitTestUsers.Add(1)), "y-api-paid")
	assert.Equal(t, http.StatusOK, postFreeModelRateLimit(router, "free-model-ratelimit-test").Code)
	for _, variant := range []string{
		"free-model-ratelimit-test@thinking:on",
		"free-model-ratelimit-test@thinking:2048",
	} {
		assert.Equal(t, http.StatusTooManyRequests, postFreeModelRateLimit(router, variant).Code,
			"%s must count against the base model's free allowance", variant)
	}
}
