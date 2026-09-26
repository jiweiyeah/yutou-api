package kitedelayed

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Kite Router 线的 key 池余额缓存与选 key（设计文档 §5.2）。
//
// 该渠道的 key 池由上千个「一次性 $5 账号」组成，random 模式下每次请求随机抽一个。
// 池子里约 3% 的 key 已被耗尽，中等规模的请求会随机撞上它们 → 偶发 402 → 重试再撞。
//
// 这里用上游免费的 GET /v1/credits 缓存每个 key 的余额，在发请求之前按估算的
// required 过滤候选 key。任何一步失败（取不到余额、池子不是多 key、超出时间预算）
// 都退回 middleware 已选定的 key —— 也就是今天的行为，绝不因此阻塞或拒绝请求。
const (
	kiteRouterCreditsPath = "/v1/credits"

	kiteRouterBalanceTTL            = 5 * time.Minute
	kiteRouterBalanceFetchTimeout   = 3 * time.Second
	kiteRouterBalanceProbeLimit     = 8
	kiteRouterBalanceProbeBudget    = 2 * time.Second
	kiteRouterDefaultFilterFloorUSD = 0.50
	// kiteRouterBalanceCacheMaxEntries 只是防止缓存无限增长（渠道删改 key 会留下
	// 陈旧条目）。超过后整表重建，代价仅是重新探测一次。
	kiteRouterBalanceCacheMaxEntries = 200000
)

// kiteRouterKeyPick 描述选 key 的结果。
type kiteRouterKeyPick int

const (
	// kiteRouterKeyPickUnchanged 表示不做过滤，沿用 middleware 已选定的 key。
	kiteRouterKeyPickUnchanged kiteRouterKeyPick = iota
	// kiteRouterKeyPickSelected 表示已按余额挑到一个够用的 key。
	kiteRouterKeyPickSelected
	// kiteRouterKeyPickNoneAffordable 表示连续探测的候选 key 余额都不够。
	kiteRouterKeyPickNoneAffordable
)

type kiteRouterBalanceEntry struct {
	balance   float64
	fetchedAt time.Time
}

var (
	kiteRouterBalanceMu        sync.Mutex
	kiteRouterBalanceByChannel = make(map[int]map[int]kiteRouterBalanceEntry)
)

func kiteRouterCachedBalance(channelID, keyIndex int) (float64, bool) {
	if channelID <= 0 || keyIndex < 0 {
		return 0, false
	}
	kiteRouterBalanceMu.Lock()
	defer kiteRouterBalanceMu.Unlock()
	entries, ok := kiteRouterBalanceByChannel[channelID]
	if !ok {
		return 0, false
	}
	entry, ok := entries[keyIndex]
	if !ok || time.Since(entry.fetchedAt) >= kiteRouterBalanceTTL {
		return 0, false
	}
	return entry.balance, true
}

func kiteRouterStoreBalance(channelID, keyIndex int, balance float64) {
	if channelID <= 0 || keyIndex < 0 {
		return
	}
	kiteRouterBalanceMu.Lock()
	defer kiteRouterBalanceMu.Unlock()
	total := 0
	for _, entries := range kiteRouterBalanceByChannel {
		total += len(entries)
	}
	if total >= kiteRouterBalanceCacheMaxEntries {
		kiteRouterBalanceByChannel = make(map[int]map[int]kiteRouterBalanceEntry)
	}
	entries, ok := kiteRouterBalanceByChannel[channelID]
	if !ok {
		entries = make(map[int]kiteRouterBalanceEntry)
		kiteRouterBalanceByChannel[channelID] = entries
	}
	entries[keyIndex] = kiteRouterBalanceEntry{balance: balance, fetchedAt: time.Now()}
}

// kiteRouterMarkKeyDrained 记录某个 key 的余额已不足以服务本轮请求。
// 上游 402 只告诉我们「余额 < required」，所以这里直接记为 0：在本轮及之后
// TTL 窗口内不再选它，到期后会重新探测（那时它可能已经不适合、也可能仍然可用）。
func kiteRouterMarkKeyDrained(channelID, keyIndex int) {
	kiteRouterStoreBalance(channelID, keyIndex, 0)
}

func kiteRouterBalanceFilterEnabled(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	if configured := kiteRouterOtherSettings(info).KiteRouterBalanceFilter; configured != nil {
		return *configured
	}
	return true
}

func kiteRouterBalanceFilterFloorUSD() float64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("KITE_ROUTER_BALANCE_FILTER_FLOOR_USD", ""))
	if raw == "" {
		return kiteRouterDefaultFilterFloorUSD
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		logger.LogWarn(nil, fmt.Sprintf("invalid KITE_ROUTER_BALANCE_FILTER_FLOOR_USD=%q, using %.2f", raw, kiteRouterDefaultFilterFloorUSD))
		return kiteRouterDefaultFilterFloorUSD
	}
	return value
}

// kiteRouterPickKey 在候选 key 里挑一个余额 ≥ requiredUSD 的。
// 返回值：(key, keyIndex, 结果)。结果为 Unchanged 时调用方保持原有 key 不变。
func (a *Adaptor) kiteRouterPickKey(c *gin.Context, info *relaycommon.RelayInfo, requiredUSD float64) (string, int, kiteRouterKeyPick) {
	if info == nil || !kiteRouterBalanceFilterEnabled(info) {
		return "", 0, kiteRouterKeyPickUnchanged
	}
	if requiredUSD <= kiteRouterBalanceFilterFloorUSD() {
		// 小请求几乎所有 key 都撑得住，不值得为它付一次 credits 往返。
		return "", 0, kiteRouterKeyPickUnchanged
	}
	channel, err := model.CacheGetChannel(info.ChannelId)
	if err != nil || channel == nil || !channel.ChannelInfo.IsMultiKey {
		return "", 0, kiteRouterKeyPickUnchanged
	}
	if len(channel.GetKeys()) == 0 {
		return "", 0, kiteRouterKeyPickUnchanged
	}

	// 先确认 middleware 已选定的 key 够不够用：够用就原样保留，省一次重选。
	if balance, ok := a.kiteRouterBalanceFor(c, info, info.ApiKey, info.ChannelMultiKeyIndex); ok {
		if balance >= requiredUSD {
			return "", 0, kiteRouterKeyPickUnchanged
		}
	} else {
		return "", 0, kiteRouterKeyPickUnchanged
	}

	deadline := time.Now().Add(kiteRouterBalanceProbeBudget)
	probed := map[string]struct{}{strings.TrimSpace(info.ApiKey): {}}
	for attempt := 0; attempt < kiteRouterBalanceProbeLimit; attempt++ {
		if time.Now().After(deadline) {
			return "", 0, kiteRouterKeyPickUnchanged
		}
		key, index, pickErr := channel.GetNextEnabledKey()
		if pickErr != nil {
			return "", 0, kiteRouterKeyPickUnchanged
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return "", 0, kiteRouterKeyPickUnchanged
		}
		if _, duplicated := probed[key]; duplicated {
			continue
		}
		probed[key] = struct{}{}

		balance, ok := a.kiteRouterBalanceFor(c, info, key, index)
		if !ok {
			return "", 0, kiteRouterKeyPickUnchanged
		}
		if balance >= requiredUSD {
			return key, index, kiteRouterKeyPickSelected
		}
	}
	return "", 0, kiteRouterKeyPickNoneAffordable
}

// kiteRouterBalanceFor 取某个 key 的余额：命中缓存直接用，否则发一次 /v1/credits。
// 第二个返回值为 false 表示「拿不到可信余额」，调用方应放弃过滤。
func (a *Adaptor) kiteRouterBalanceFor(c *gin.Context, info *relaycommon.RelayInfo, key string, keyIndex int) (float64, bool) {
	if balance, ok := kiteRouterCachedBalance(info.ChannelId, keyIndex); ok {
		return balance, true
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, false
	}
	balance, err := a.fetchKiteRouterBalance(c, info, key)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("Kite Router credits check failed for channel %d key index %d: %v", info.ChannelId, keyIndex, err))
		return 0, false
	}
	kiteRouterStoreBalance(info.ChannelId, keyIndex, balance)
	return balance, true
}

func (a *Adaptor) fetchKiteRouterBalance(c *gin.Context, info *relaycommon.RelayInfo, key string) (float64, error) {
	base, ok := kiteRouterBaseURL(info.ChannelBaseUrl)
	if !ok {
		return 0, fmt.Errorf("channel %d is not on the Kite Router line", info.ChannelId)
	}
	statusCode, body, err := kiteRouterUpstreamGet(c, info, base+kiteRouterCreditsPath, key, kiteRouterBalanceFetchTimeout)
	if err != nil {
		return 0, err
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("upstream returned HTTP %d", statusCode)
	}

	var payload struct {
		Currency string `json:"currency"`
		Balance  any    `json:"balance"`
	}
	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode credits response: %w", err)
	}
	if payload.Currency != "" && !strings.EqualFold(strings.TrimSpace(payload.Currency), "USD") {
		return 0, fmt.Errorf("unexpected credits currency %q", payload.Currency)
	}
	return kiteRouterParseBalance(payload.Balance)
}

func kiteRouterParseBalance(value any) (float64, error) {
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, fmt.Errorf("invalid non-finite credits balance")
		}
		return typed, nil
	case nil:
		return 0, fmt.Errorf("credits response balance is missing")
	default:
		text = fmt.Sprint(typed)
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid credits balance %q: %w", text, err)
	}
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return 0, fmt.Errorf("invalid non-finite credits balance")
	}
	return balance, nil
}

// kiteRouterNoAffordableKeyError 在「探测到的候选 key 余额都不够」时返回。
// 这不是上下文问题，而是池子里没有能服务本轮预检金额的账号。
func (a *Adaptor) kiteRouterNoAffordableKeyError(c *gin.Context, info *relaycommon.RelayInfo, budget *kiteRouterBudget) *types.NewAPIError {
	options := []string{"压缩上下文（Codex 在接近上限时会自行压缩，也可手动触发）"}
	if alternative := a.kiteRouterCheapestAlternative(c, info, budget); alternative != "" {
		options = append(options, alternative)
	}
	options = append(options, "稍后重试（池子里仍有余额充足的 key，重试会重新抽签）")

	var message strings.Builder
	fmt.Fprintf(&message,
		"上游 Kite Router 预检需要 $%.4f，但连续探测的候选 key 余额都不足（该池由一次性 $5 账号组成，无法充值）。请任选其一：",
		budget.RequiredUSD)
	for index, option := range options {
		fmt.Fprintf(&message, "%s%s；", kiteRouterOptionMarker(index), option)
	}

	metadata, _ := common.Marshal(map[string]any{
		"code":            "insufficient_router_balance",
		"model":           budget.Model,
		"required_usd":    budget.RequiredUSD,
		"safe_budget_usd": budget.SafeUSD,
		"prompt_bytes":    budget.PromptBytes,
		"max_tokens":      budget.MaxTokens,
	})

	return types.NewErrorWithStatusCode(
		fmt.Errorf("%s", message.String()),
		types.ErrorCode("insufficient_router_balance"),
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithMetadata(metadata),
	)
}

// kiteRouterIsInsufficientBalance 判断 402 响应体是不是上游的额度预检拒绝
// （`{"detail":{"code":"insufficient_router_balance", ...}}`）。其他 402 语义
// 不同，保持原有透传路径。
func kiteRouterIsInsufficientBalance(body []byte) bool {
	var payload struct {
		Detail struct {
			Code string `json:"code"`
		} `json:"detail"`
	}
	if err := common.Unmarshal(body, &payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Detail.Code) == "insufficient_router_balance"
}

// kiteRouterUpstreamBalanceError 把上游原样返回的 402 insufficient_router_balance
// 翻译成人话。设计文档 §7 明确要求绝不透传上游原始错误。
func (a *Adaptor) kiteRouterUpstreamBalanceError(c *gin.Context, info *relaycommon.RelayInfo, body []byte) *types.NewAPIError {
	requiredMicrousd := 0
	var payload struct {
		Detail struct {
			Code             string `json:"code"`
			Message          string `json:"message"`
			RequiredMicrousd int64  `json:"required_microusd"`
		} `json:"detail"`
	}
	if err := common.Unmarshal(body, &payload); err == nil {
		requiredMicrousd = int(payload.Detail.RequiredMicrousd)
	}

	modelName := ""
	channelID, keyIndex := 0, 0
	if info != nil {
		modelName = info.UpstreamModelName
		if info.ChannelMeta != nil {
			channelID = info.ChannelId
			keyIndex = info.ChannelMultiKeyIndex
		}
	}
	var message strings.Builder
	fmt.Fprintf(&message,
		"上游 Kite Router 判定该请求超出账号额度（模型 %s，预检需要 $%.4f），且当前 key 余额不足。请任选其一：",
		modelName, float64(requiredMicrousd)/1e6)
	options := []string{"压缩上下文（Codex 在接近上限时会自行压缩，也可手动触发）", "改用更便宜的模型", "稍后重试（重试会换一个 key）"}
	for index, option := range options {
		fmt.Fprintf(&message, "%s%s；", kiteRouterOptionMarker(index), option)
	}

	metadata, _ := common.Marshal(map[string]any{
		"code":            "insufficient_router_balance",
		"model":           modelName,
		"required_usd":    float64(requiredMicrousd) / 1e6,
		"channel_id":      channelID,
		"multi_key_index": keyIndex,
	})

	return types.NewErrorWithStatusCode(
		fmt.Errorf("%s", message.String()),
		types.ErrorCode("insufficient_router_balance"),
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithMetadata(metadata),
	)
}
