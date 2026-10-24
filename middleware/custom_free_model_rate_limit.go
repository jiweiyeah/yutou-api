package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/reasoning"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hostreasoning "github.com/QuantumNous/new-api/setting/reasoning"

	"github.com/gin-gonic/gin"
)

// freeModelRateLimitNamespace 与 ModelRequestRateLimit 的键空间分开，两套计数
// 互不影响：免费模型额度用完不影响付费模型。
const freeModelRateLimitNamespace = "rateLimit:freeModel"

// freeModelInFlight 记录每个用户在途的免费模型请求数。只在进程内计数：生产是
// 单实例部署；多实例下每个实例各自计数，实际总并发上限是配置值乘以实例数。
var freeModelInFlight sync.Map

// FreeModelRateLimit 限制指定分组对**免费模型**的调用次数与并发数。
//
// 它独立于 ModelRequestRateLimit：后者按分组配置（y-api / y-api-boost），
// y-api-paid 不在其中，于是付费用户对免费模型完全没有约束。本中间件只对免费
// 模型生效，付费模型请求原样放行，因此不影响付费用户的付费调用。
//
// 计数口径是「请求数」而非「成功数」：失败的请求同样占额度，避免客户端用重试
// 把上游打穿。配置全部来自环境变量（common/custom_free_model_rate_limit.go），
// 默认关闭。
func FreeModelRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !common.FreeModelRateLimitEnabled {
			c.Next()
			return
		}
		countLimit := common.FreeModelRateLimitCount
		concurrencyLimit := common.FreeModelRateLimitConcurrency
		// 两个上限都没配时直接放行，不为判定免费模型去解析请求体。
		if countLimit <= 0 && concurrencyLimit <= 0 {
			c.Next()
			return
		}
		group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUserGroup)
		}
		if !freeModelRateLimitAppliesTo(group) {
			c.Next()
			return
		}
		// 分组过滤先做，只有受限分组才承担提前解析请求体的代价。
		model := freeModelRequestModel(c)
		if model == "" || !isFreeModel(model) {
			c.Next()
			return
		}
		userId := c.GetInt("id")
		if userId <= 0 {
			c.Next()
			return
		}

		if concurrencyLimit > 0 {
			if !freeModelAcquire(userId, concurrencyLimit) {
				abortWithOpenAiMessage(c, http.StatusTooManyRequests,
					i18n.T(c, i18n.MsgFreeModelConcurrencyReached, map[string]any{"Max": concurrencyLimit}))
				return
			}
			defer freeModelRelease(userId)
		}
		if countLimit > 0 && !freeModelCheckCount(c, userId, countLimit) {
			return
		}
		c.Next()
	}
}

// freeModelCheckCount 检查窗口内的免费模型请求数，超限时写入 429 响应并返回
// false。
func freeModelCheckCount(c *gin.Context, userId int, limit int) bool {
	windowSeconds := int64(common.FreeModelRateLimitWindowMin) * 60
	if windowSeconds <= 0 {
		windowSeconds = 24 * 60 * 60
	}
	key := freeModelRateLimitNamespace + ":" + strconv.Itoa(userId)
	var (
		allowed bool
		ttl     int64
		err     error
	)
	if common.RedisEnabled && common.RDB != nil {
		allowed, _, ttl, err = redisFixedWindowTake(c.Request.Context(), key, limit, windowSeconds)
	} else {
		// 无 Redis 时的兜底：进程内滑动窗口，但条目按「最后活跃时间」淘汰，
		// 空闲超过 RateLimitKeyExpirationDuration 的用户会被重置窗口。24 小时
		// 窗口要精确计数必须有 Redis。
		inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
		allowed = inMemoryRateLimiter.Request(key, limit, windowSeconds)
		if !allowed {
			ttl = windowSeconds
		}
	}
	if err != nil {
		// 限流器不可用时放行：Redis 抖动不该把整站免费模型请求打死。
		common.SysError(common.LogText("free model rate limit check failed: %s", err.Error()))
		return true
	}
	if allowed {
		return true
	}
	if ttl > 0 {
		c.Header("Retry-After", strconv.FormatInt(ttl, 10))
	}
	abortWithOpenAiMessage(c, http.StatusTooManyRequests, i18n.T(c, i18n.MsgFreeModelRateLimitReached, map[string]any{
		"Minutes": common.FreeModelRateLimitWindowMin,
		"Max":     limit,
	}))
	return false
}

// freeModelRateLimitAppliesTo 报告 group 是否在 FREE_MODEL_RATE_LIMIT_GROUPS 里。
// 配置为空表示所有分组都受限。
func freeModelRateLimitAppliesTo(group string) bool {
	configured := strings.TrimSpace(common.FreeModelRateLimitGroups)
	if configured == "" {
		return true
	}
	for name := range strings.SplitSeq(configured, ",") {
		if strings.TrimSpace(name) == group {
			return true
		}
	}
	return false
}

// isFreeModel 判定模型是否免费，口径与 relay/helper/price.go 的计费判定一致：
// 价格或倍率为 0 即免费。查不到配置的模型不算免费——它会走默认倍率计费，不该
// 占用免费额度。
//
// 必须先按计费侧的顺序解析模型名：直接拿原始名查表会被 `@thinking`、
// `-thinking` 这类后缀绕过（原始名没有配置项，于是判定为「非免费」直接放行）。
func isFreeModel(model string) bool {
	name, ok := configuredBillingModelName(model)
	if !ok {
		return false
	}
	if price, ok := ratio_setting.GetModelPrice(name, false); ok {
		return price == 0
	}
	ratio, ok, _ := ratio_setting.GetModelRatio(name)
	return ok && ratio == 0
}

// configuredBillingModelName 复刻 relay/helper/price.go resolveBillingModelName
// 的候选顺序，返回第一个有价格或倍率配置的名字。
//
// 与计费侧唯一的差异是不认 tiered_expr 计费模式：那类模型按表达式计价，价格
// 与倍率表里都没有条目，在这里会落到下一个候选或被判为非免费——方向上是少放
// 行、多计数，不会让人绕过额度。
func configuredBillingModelName(model string) (string, bool) {
	var candidates []string
	if !reasoning.ParseModelModifiers(model).HasModifiers() {
		candidates = append(candidates, model)
	}
	candidates = append(candidates, hostreasoning.CanonicalBillingModelNames(model)...)
	candidates = append(candidates, hostreasoning.BaseModelName(model))

	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		formatted := ratio_setting.FormatMatchingModelName(name)
		if _, ok := ratio_setting.GetModelPrice(formatted, false); ok {
			return formatted, true
		}
		if ratio_setting.HasConfiguredModelRatio(formatted) {
			return formatted, true
		}
	}
	return "", false
}

// freeModelRequestModel 取出请求要调用的模型名。限流发生在 Distribute 之前，
// 上下文里还没有 original_model，只能自己解析。
// getModelFromRequest 读的是可重复读的 body storage，读完会复位，不影响后续。
// 只解析 JSON 请求体：multipart / form 的模型名要么走查询参数（音频端点有默认
// 模型），要么一定不是免费模型，不值得为它们在 Distribute 之前把上传内容再解
// 一遍。
func freeModelRequestModel(c *gin.Context) string {
	if model := c.GetString("original_model"); model != "" {
		return model
	}
	if model := c.GetString("resolved_task_model"); model != "" {
		return model
	}
	if model := c.Query("model"); model != "" {
		return model
	}
	// Gemini 把模型名放在路径里：/v1beta/models/{model}:{action}。没有冒号的
	// 路径不是 Gemini 动作端点，交给请求体解析。
	path := c.Request.URL.Path
	if strings.Contains(path, "/models/") && strings.Contains(path, ":") {
		return extractModelNameFromGeminiPath(path)
	}
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		return ""
	}
	modelRequest, err := getModelFromRequest(c)
	if err != nil || modelRequest == nil {
		return ""
	}
	return modelRequest.Model
}

// freeModelAcquire 占用一个在途名额，返回是否成功。
func freeModelAcquire(userId int, limit int) bool {
	value, _ := freeModelInFlight.LoadOrStore(strconv.Itoa(userId), new(atomic.Int64))
	counter := value.(*atomic.Int64)
	for {
		current := counter.Load()
		if current >= int64(limit) {
			return false
		}
		if counter.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func freeModelRelease(userId int) {
	if value, ok := freeModelInFlight.Load(strconv.Itoa(userId)); ok {
		value.(*atomic.Int64).Add(-1)
	}
}
