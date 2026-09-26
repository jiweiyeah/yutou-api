package kitedelayed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Kite Router 线的额度预算模型。
//
// 上游 Kite Router 的每个 API key 归属一个独立账号，账号只有一次性 welcome
// credit（$5），**无法充值**。上游在转发前做额度预检，不够就直接拒绝：
//
//	402 {"detail":{"code":"insufficient_router_balance",
//	      "message":"insufficient Kite Router allowance",
//	      "required_microusd":5278695}}
//
// 预检金额实测标定（2026-09-26，38 个数据点，误差 < 0.4%）：
//
//	required_USD ≈ 请求体字节数 × 输入价($/M) / 1e6
//	             + 输出预留 token 数 × 输出价($/M) / 1e6
//	             + $0.004
//
// 两个容易搞错的点（都已实测排除）：
//  1. 提示项按**字节**计价，不是 token —— 同一 30k token 的提示，随机串（53KB）
//     要 $0.889、英文散文（155KB）要 $2.416、中文（122KB UTF-8）要 $1.911，
//     三条都精确落在「字节 × 输入价」上，按 token 则完全对不上。
//  2. 输出预留按 `max_tokens` 取，不传时按上游目录的 `max_output_tokens`（当前全为
//     8192）。实测 1024→2048→8192 三档差额恰好等于 1024×输出价。
//
// 因此一个 $5 账号能服务的**字节上限**是：
//
//	threshold_bytes(model) = (safe_budget - max_output × 输出价/1e6 - 0.004) / (输入价/1e6)
//
// 例如 gpt-6-astra（$15/M 入、$75/M 出）在 $4.5 安全线下是 258,773 字节。
//
// 作用域：以下所有逻辑只挂在 isKiteRouterChannel 之下，且只对
// RelayModeResponses / RelayModeResponsesCompact 生效。其他渠道、其他 type、
// 其他 relay mode 的代码路径逐字节不变。
const (
	kiteRouterCatalogPath    = "/v1/catalog"
	kiteRouterCatalogTTL     = 30 * time.Minute
	kiteRouterCatalogTimeout = 15 * time.Second

	// kiteRouterDefaultSafeBudgetUSD 是压缩/拒绝的触发线。距 $5 硬上限留 10%
	// 余量：required 是上游的预检值，与实收存在偏差，且字节估算本身有误差。
	kiteRouterDefaultSafeBudgetUSD = 4.50
	// kiteRouterHardCapUSD 是上游账号的硬上限，仅用于兜底判断与配置校验。
	kiteRouterHardCapUSD = 5.00
	// kiteRouterReserveBaseUSD 是实测的常数项。
	kiteRouterReserveBaseUSD = 0.004

	// kiteRouterBytesPerTokenForDisplay 只用于把字节阈值换算成人话里的 token 数。
	// 取 4（偏保守：真实 token 数通常不小于 字节/4）以免提示里给出过大的 token 上限。
	kiteRouterBytesPerTokenForDisplay = 4

	kiteRouterBudgetContextKey = "kite_router_budget"
)

// kiteRouterModelPrice 是上游 /v1/catalog 里一个模型的计价与容量信息。
type kiteRouterModelPrice struct {
	InputPerMillion  float64
	OutputPerMillion float64
	ContextLength    int
	MaxOutputTokens  int
}

func (p kiteRouterModelPrice) usable() bool {
	return p.InputPerMillion > 0 && p.OutputPerMillion > 0
}

// reserveOutputTokens 返回上游做预检时会预留的输出 token 数：显式 max_tokens
// 优先（但不超过目录上限），缺省时用目录的 max_output_tokens。
func (p kiteRouterModelPrice) reserveOutputTokens(maxTokens int) int {
	if maxTokens > 0 && (p.MaxOutputTokens <= 0 || maxTokens < p.MaxOutputTokens) {
		return maxTokens
	}
	return p.MaxOutputTokens
}

// requiredUSD 估算上游对该请求的预检金额。
func (p kiteRouterModelPrice) requiredUSD(promptBytes, maxTokens int) float64 {
	return float64(promptBytes)*p.InputPerMillion/1e6 +
		float64(p.reserveOutputTokens(maxTokens))*p.OutputPerMillion/1e6 +
		kiteRouterReserveBaseUSD
}

// thresholdBytes 返回给定安全线下该模型能服务的最大提示字节数。
func (p kiteRouterModelPrice) thresholdBytes(safeBudgetUSD float64) int {
	if !p.usable() {
		return 0
	}
	remaining := safeBudgetUSD -
		float64(p.reserveOutputTokens(0))*p.OutputPerMillion/1e6 -
		kiteRouterReserveBaseUSD
	if remaining <= 0 {
		return 0
	}
	bytes := remaining / (p.InputPerMillion / 1e6)
	if bytes >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(bytes)
}

// canTriggerBudget 判断该模型是否存在「提示本身就把额度吃超」的可能。
// 只用于挑选压缩专用模型：我们假定真实文本的字节/token 不超过 4，
// 于是目录上下文窗口对应的最大字节数是 context_length × 4。
func (p kiteRouterModelPrice) canTriggerBudget(safeBudgetUSD float64) bool {
	if !p.usable() || p.ContextLength <= 0 {
		return true
	}
	return p.thresholdBytes(safeBudgetUSD) < p.ContextLength*kiteRouterBytesPerTokenForDisplay
}

// kiteRouterBudget 是单次请求的额度估算结果。
type kiteRouterBudget struct {
	Model       string
	PromptBytes int
	MaxTokens   int
	RequiredUSD float64
	SafeUSD     float64
	Price       kiteRouterModelPrice
}

func (b *kiteRouterBudget) exceeded() bool {
	return b != nil && b.RequiredUSD > b.SafeUSD
}

// kiteRouterCatalog 是某个上游（按 router base URL 区分）的模型价格表快照。
type kiteRouterCatalog struct {
	fetchedAt time.Time
	models    map[string]kiteRouterModelPrice
}

type kiteRouterCatalogResponse struct {
	Models []struct {
		ID              string `json:"id"`
		ContextLength   int    `json:"context_length"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Pricing         struct {
			Unit   string `json:"unit"`
			Input  string `json:"input"`
			Output string `json:"output"`
		} `json:"pricing"`
	} `json:"models"`
}

// kiteRouterFallbackPrices 是 /v1/catalog 取不到时的兜底价格表（实测值）。
// 兜底表里没有的模型一律跳过闸门（放行），绝不因为取价失败而拒绝请求。
var kiteRouterFallbackPrices = map[string]kiteRouterModelPrice{
	"gpt-6-astra":         {InputPerMillion: 15, OutputPerMillion: 75, ContextLength: 1050000, MaxOutputTokens: 8192},
	"gpt-5.6-sol":         {InputPerMillion: 6, OutputPerMillion: 30, ContextLength: 1050000, MaxOutputTokens: 8192},
	"gpt-5.6-luna":        {InputPerMillion: 0.30, OutputPerMillion: 1.80, ContextLength: 1050000, MaxOutputTokens: 8192},
	"claude-opus-5":       {InputPerMillion: 7.5, OutputPerMillion: 37.5, ContextLength: 1000000, MaxOutputTokens: 8192},
	"claude-sonnet-5":     {InputPerMillion: 3, OutputPerMillion: 15, ContextLength: 1000000, MaxOutputTokens: 8192},
	"solar-pro4":          {InputPerMillion: 0.135, OutputPerMillion: 0.54, ContextLength: 524288, MaxOutputTokens: 8192},
	"kimi-k3":             {InputPerMillion: 3.15, OutputPerMillion: 16.425, ContextLength: 1048576, MaxOutputTokens: 8192},
	"deepseek-v4-pro":     {InputPerMillion: 1.419579, OutputPerMillion: 2.839158, ContextLength: 1024000, MaxOutputTokens: 8192},
	"deepseek-v4.1-flash": {InputPerMillion: 0.10, OutputPerMillion: 0.40, ContextLength: 1024000, MaxOutputTokens: 8192},
}

var (
	kiteRouterCatalogMu      sync.RWMutex
	kiteRouterCatalogCache   = make(map[string]*kiteRouterCatalog)
	kiteRouterCatalogFetchMu sync.Mutex
)

// kiteRouterLookupPrice 按「完整名 → 去掉 vendor 前缀 → 最后一段」的顺序查价，
// 因为渠道的 models 用 `openai/gpt-6-astra` 这种带 vendor 的名字，而上游目录用裸 id。
func kiteRouterLookupPrice(models map[string]kiteRouterModelPrice, model string) (kiteRouterModelPrice, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	if trimmed == "" || len(models) == 0 {
		return kiteRouterModelPrice{}, false
	}
	if price, ok := models[trimmed]; ok {
		return price, true
	}
	if _, rest, found := strings.Cut(trimmed, "/"); found {
		if price, ok := models[rest]; ok {
			return price, true
		}
		if _, tail, found := strings.Cut(rest, "/"); found {
			if price, ok := models[tail]; ok {
				return price, true
			}
		}
	}
	return kiteRouterModelPrice{}, false
}

// kiteRouterCatalogFor 返回该渠道上游的模型价格表。命中缓存（TTL 30min）时零网络开销；
// 取价失败时退回上一次成功的快照，再退回内置兜底表。
func (a *Adaptor) kiteRouterCatalogFor(c *gin.Context, info *relaycommon.RelayInfo) map[string]kiteRouterModelPrice {
	base, ok := kiteRouterBaseURL(info.ChannelBaseUrl)
	if !ok {
		return kiteRouterFallbackPrices
	}
	proxy, apiKey := "", ""
	if info != nil && info.ChannelMeta != nil {
		proxy = info.ChannelSetting.Proxy
		apiKey = info.ApiKey
	}
	return kiteRouterCatalogForBase(c, base, proxy, apiKey)
}

// kiteRouterCatalogForBase 是不依赖 RelayInfo 的取价入口（模型目录端点等旁路场景用）。
func kiteRouterCatalogForBase(c *gin.Context, base, proxy, apiKey string) map[string]kiteRouterModelPrice {

	kiteRouterCatalogMu.RLock()
	cached := kiteRouterCatalogCache[base]
	kiteRouterCatalogMu.RUnlock()
	if cached != nil && time.Since(cached.fetchedAt) < kiteRouterCatalogTTL {
		return cached.models
	}

	kiteRouterCatalogFetchMu.Lock()
	defer kiteRouterCatalogFetchMu.Unlock()
	// 等锁期间可能已被别的请求刷新过。
	kiteRouterCatalogMu.RLock()
	cached = kiteRouterCatalogCache[base]
	kiteRouterCatalogMu.RUnlock()
	if cached != nil && time.Since(cached.fetchedAt) < kiteRouterCatalogTTL {
		return cached.models
	}

	models, err := kiteRouterFetchCatalog(c, base, proxy, apiKey)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("Kite Router catalog fetch failed, falling back to cached/builtin prices: %v", err))
		if cached != nil {
			return cached.models
		}
		return kiteRouterFallbackPrices
	}

	kiteRouterCatalogMu.Lock()
	kiteRouterCatalogCache[base] = &kiteRouterCatalog{fetchedAt: time.Now(), models: models}
	kiteRouterCatalogMu.Unlock()
	return models
}

func kiteRouterFetchCatalog(c *gin.Context, base, proxy, apiKey string) (map[string]kiteRouterModelPrice, error) {
	statusCode, body, err := kiteRouterUpstreamGet(c, proxy, base+kiteRouterCatalogPath, apiKey, kiteRouterCatalogTimeout)
	if err != nil {
		return nil, err
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream returned HTTP %d", statusCode)
	}

	var payload kiteRouterCatalogResponse
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode catalog response: %w", err)
	}
	models := make(map[string]kiteRouterModelPrice, len(payload.Models))
	for _, entry := range payload.Models {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" {
			continue
		}
		price := kiteRouterModelPrice{
			InputPerMillion:  kiteRouterParsePrice(entry.Pricing.Input),
			OutputPerMillion: kiteRouterParsePrice(entry.Pricing.Output),
			ContextLength:    entry.ContextLength,
			MaxOutputTokens:  entry.MaxOutputTokens,
		}
		if !price.usable() {
			continue
		}
		models[id] = price
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("catalog response contained no usable model prices")
	}
	return models, nil
}

func kiteRouterParsePrice(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

// kiteRouterUpstreamGet 是预算相关旁路请求（catalog / credits）专用的 GET。
// 它刻意不走 adaptor.doJSONRequest：那条路径在 info.IsStream 为真时会提前写出
// SSE 头并启动心跳，而预算检查发生在流式请求的正文之前，会污染客户端响应。
func kiteRouterUpstreamGet(c *gin.Context, proxy, requestURL, apiKey string, timeout time.Duration) (int, []byte, error) {
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		_ = response.Body.Close()
	}()
	body, err := readLimited(response.Body, 1<<20)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

// kiteRouterOtherSettings 安全读取渠道级扩展设置。ChannelMeta 是嵌入指针，
// 未初始化的 RelayInfo（单测、旁路调用）上直接取字段会 panic。
func kiteRouterOtherSettings(info *relaycommon.RelayInfo) dto.ChannelOtherSettings {
	if info == nil || info.ChannelMeta == nil {
		return dto.ChannelOtherSettings{}
	}
	return info.ChannelOtherSettings
}

// kiteRouterSafeBudgetUSD 解析生效的安全线：渠道设置优先，其次环境变量，最后默认 4.5。
func kiteRouterSafeBudgetUSD(info *relaycommon.RelayInfo) float64 {
	return kiteRouterSafeBudgetUSDFor(kiteRouterOtherSettings(info))
}

// kiteRouterSafeBudgetUSDFor 是不依赖 RelayInfo 的安全线解析（模型目录端点用）。
func kiteRouterSafeBudgetUSDFor(settings dto.ChannelOtherSettings) float64 {
	if configured := settings.KiteRouterSafeBudgetUSD; configured != nil {
		if *configured > 0 && *configured <= kiteRouterHardCapUSD {
			return *configured
		}
		logger.LogWarn(nil, fmt.Sprintf("Kite Router safe budget %.4f out of range (0, %.2f], using default %.2f",
			*configured, kiteRouterHardCapUSD, kiteRouterDefaultSafeBudgetUSD))
	}
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("KITE_ROUTER_SAFE_BUDGET_USD", ""))
	if raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err == nil && value > 0 && value <= kiteRouterHardCapUSD {
			return value
		}
		logger.LogWarn(nil, fmt.Sprintf("invalid KITE_ROUTER_SAFE_BUDGET_USD=%q, using %.2f", raw, kiteRouterDefaultSafeBudgetUSD))
	}
	return kiteRouterDefaultSafeBudgetUSD
}

// kiteRouterBudgetGateDisabled 是渠道级逃生阀：闸门误伤时可以按渠道直接关掉，
// 行为退回本次改动之前（超限请求照发上游，由上游回 402），不必等发版。
func kiteRouterBudgetGateDisabled(info *relaycommon.RelayInfo) bool {
	disabled := kiteRouterOtherSettings(info).KiteRouterBudgetGateDisabled
	return disabled != nil && *disabled
}

// kiteRouterEstimateBudget 计算一次请求的额度占用。价格取不到（模型不在目录也不在
// 兜底表）时返回 nil，调用方据此跳过闸门 —— 不因为取价失败而拒绝请求。
func (a *Adaptor) kiteRouterEstimateBudget(c *gin.Context, info *relaycommon.RelayInfo, modelName string, promptBytes, maxTokens int) *kiteRouterBudget {
	if promptBytes <= 0 {
		return nil
	}
	price, ok := kiteRouterLookupPrice(a.kiteRouterCatalogFor(c, info), modelName)
	if !ok {
		return nil
	}
	safeBudget := kiteRouterSafeBudgetUSD(info)
	return &kiteRouterBudget{
		Model:       modelName,
		PromptBytes: promptBytes,
		MaxTokens:   price.reserveOutputTokens(maxTokens),
		RequiredUSD: price.requiredUSD(promptBytes, maxTokens),
		SafeUSD:     safeBudget,
		Price:       price,
	}
}

// applyKiteRouterBudgetGate 是 §5.1 的预算闸门：在把请求发给上游**之前**判断
// 它是否注定 402，注定失败就返回可执行的明确错误，绝不白跑一次往返。
// 通过时把估算结果挂到 gin context，供 doKiteRouterRequest 选 key 复用。
func (a *Adaptor) applyKiteRouterBudgetGate(c *gin.Context, info *relaycommon.RelayInfo, modelName string, promptBytes, maxTokens int) *types.NewAPIError {
	// 渠道级逃生阀：闸门误伤时运营可以直接关掉它（≤60s 生效），不必等发版。
	if kiteRouterBudgetGateDisabled(info) {
		return nil
	}
	budget := a.kiteRouterEstimateBudget(c, info, modelName, promptBytes, maxTokens)
	if budget == nil {
		return nil
	}
	c.Set(kiteRouterBudgetContextKey, budget)
	kiteRouterSetBudgetHeader(c, budget)
	if !budget.exceeded() {
		return nil
	}
	return a.kiteRouterBudgetError(c, info, budget)
}

// kiteRouterBudgetHeaderRatio 是「接近安全线」的下发门槛：超过安全线的 80%
// 才在响应头里提示客户端该压缩了，避免给正常请求塞无用的头。
const kiteRouterBudgetHeaderRatio = 0.8

// kiteRouterBudgetHeader 是提示客户端何时该压缩的响应头名。
const kiteRouterBudgetHeader = "X-Kite-Context-Budget"

// kiteRouterSetBudgetHeader 在接近安全线时下发阈值提示（设计文档 §5.4）。
// 服务端无法主动发起压缩（HTTP 请求-响应模型），能做的是「被调用时执行」+「下发建议阈值」。
func kiteRouterSetBudgetHeader(c *gin.Context, budget *kiteRouterBudget) {
	if c == nil || budget == nil || budget.RequiredUSD < budget.SafeUSD*kiteRouterBudgetHeaderRatio {
		return
	}
	thresholdBytes := budget.Price.thresholdBytes(budget.SafeUSD)
	c.Header(kiteRouterBudgetHeader, fmt.Sprintf(
		"used=%.4f/%.2f model=%s prompt_bytes=%d threshold_bytes=%d threshold_tokens=%d max_tokens=%d",
		budget.RequiredUSD, budget.SafeUSD, budget.Model, budget.PromptBytes,
		thresholdBytes, thresholdBytes/kiteRouterBytesPerTokenForDisplay, budget.MaxTokens))
}

// kiteRouterBudgetFrom 取出本轮预算估算（没有则返回 nil）。
func kiteRouterBudgetFrom(c *gin.Context) *kiteRouterBudget {
	if c == nil {
		return nil
	}
	value, exists := c.Get(kiteRouterBudgetContextKey)
	if !exists {
		return nil
	}
	budget, _ := value.(*kiteRouterBudget)
	return budget
}

func (a *Adaptor) kiteRouterBudgetError(c *gin.Context, info *relaycommon.RelayInfo, budget *kiteRouterBudget) *types.NewAPIError {
	thresholdBytes := budget.Price.thresholdBytes(budget.SafeUSD)
	// 用户可见文案刻意保持**英文、精简、不带金额**：只说明"撞到上下文上限"和可选出路。
	// 金额/字节/token 等内部细节全部只放在 metadata 里给程序处理。
	// 两个 token 数必须用**同一口径**（字节/4）换算 —— 曾出现过「约 258773 字节（约 64693
	// token），当前约 292185 字节（约 12498 token）」这种 23 字节/token 的自相矛盾，
	// 后者来自网关对 responses 请求的 token 估算（只统计部分字段，对工具调用密集的
	// Codex 请求严重低估）。判据是字节，换算也只用字节。
	promptTokens := budget.PromptBytes / kiteRouterBytesPerTokenForDisplay

	alternative := a.kiteRouterCheapestAlternative(c, info, budget)
	if alternative == "" {
		alternative = "a model with a larger limit"
	}
	message := fmt.Sprintf(
		"Context limit reached for model %s: this request is ~%d bytes, above the ~%d-byte limit for this channel. Compact the conversation, switch to %s, or start a new session.",
		budget.Model, budget.PromptBytes, thresholdBytes, alternative)

	metadata, _ := common.Marshal(map[string]any{
		"code":                 "context_budget_exceeded",
		"model":                budget.Model,
		"required_usd":         budget.RequiredUSD,
		"safe_budget_usd":      budget.SafeUSD,
		"threshold_bytes":      thresholdBytes,
		"threshold_tokens":     thresholdBytes / kiteRouterBytesPerTokenForDisplay,
		"prompt_bytes":         budget.PromptBytes,
		"prompt_tokens":        promptTokens,
		"bytes_per_token_used": kiteRouterBytesPerTokenForDisplay,
		"max_tokens":           budget.MaxTokens,
	})

	return types.NewErrorWithStatusCode(
		errors.New(message),
		types.ErrorCode("context_budget_exceeded"),
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithMetadata(metadata),
	)
}

// kiteRouterCheapestAlternative 找一个「同样这份上下文、但不会超限」的更便宜模型，
// 用于把错误信息变成可执行建议。它只是锦上添花，任何失败都返回空串 —— 绝不能因为
// 查不到建议而把一次明确的 400 变成 panic。
func (a *Adaptor) kiteRouterCheapestAlternative(c *gin.Context, info *relaycommon.RelayInfo, budget *kiteRouterBudget) string {
	if info == nil || model.DB == nil {
		return ""
	}
	channel, err := model.CacheGetChannel(info.ChannelId)
	if err != nil || channel == nil {
		return ""
	}
	catalog := a.kiteRouterCatalogFor(c, info)
	bestName := ""
	bestRequired := math.MaxFloat64
	for _, candidate := range channel.GetModels() {
		price, ok := kiteRouterLookupPrice(catalog, candidate)
		if !ok || !price.usable() {
			continue
		}
		if kiteRouterLookupPriceSame(price, budget.Price) {
			continue
		}
		required := price.requiredUSD(budget.PromptBytes, budget.MaxTokens)
		if required > budget.SafeUSD || required >= bestRequired {
			continue
		}
		bestName = candidate
		bestRequired = required
	}
	if bestName == "" {
		return ""
	}
	return fmt.Sprintf("改用 %s（同上下文约 $%.2f）", bestName, bestRequired)
}

func kiteRouterLookupPriceSame(left, right kiteRouterModelPrice) bool {
	return left.InputPerMillion == right.InputPerMillion && left.OutputPerMillion == right.OutputPerMillion
}

// kiteRouterPromptBytes 用实际会发往上游的请求体长度作为「提示字节数」。
// 后续的 RemoveDisabledFields / param_override 只会让请求体变小，所以这个值是上界。
func kiteRouterPromptBytes(payload any) int {
	if payload == nil {
		return 0
	}
	encoded, err := common.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// kiteRouterRequestedOutputTokens 取请求里的输出上限；为 0 时由价格表回退到
// 上游目录的 max_output_tokens（上游就是这么预留的）。
func kiteRouterRequestedOutputTokens(request *dto.GeneralOpenAIRequest) int {
	if request == nil {
		return 0
	}
	if request.MaxCompletionTokens != nil {
		return int(*request.MaxCompletionTokens)
	}
	if request.MaxTokens != nil {
		return int(*request.MaxTokens)
	}
	return 0
}
