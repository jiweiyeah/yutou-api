package kitedelayed

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// Codex 模型目录（`model_catalog_url`）所需的下发数据。
//
// 背景：Codex 只在自己不认识模型名时用 fallback 元数据，而那份 fallback 里
// `auto_compact_token_limit` 是 **None**（见 `codex-rs/models-manager/src/model_info.rs`
// 的 `model_info_from_slug`），于是它要等到 `context_window × 95% ≈ 25.8 万 token`
// 才触发压缩 —— 比 Kite Router 的字节制安全线（astra 258,773 字节 ≈ 6.5 万 token 口径）
// **晚了约 4 倍**。结果就是撞线时它不会自动压缩，而是把超限请求发出来被闸门拒掉。
//
// 这里把「按 §4.2 字节阈值换算出的保守 token 上限」算出来，交给 Codex 模型目录下发，
// 让客户端在撞线之前自己压缩。
//
// 保守口径：字节阈值 / 4。真实 token 数不会小于 字节/4，所以这个值只会让压缩**提前**发生。
const kiteRouterAutoCompactCacheTTL = 5 * time.Minute

// CodexModelMetadata 是网关能下发给 Codex 的、与 Kite Router 线相关的模型元数据。
type CodexModelMetadata struct {
	// AutoCompactTokenLimit 是保守的自动压缩阈值；0 表示不下发（客户端沿用 fallback）。
	AutoCompactTokenLimit int
	// ContextWindow 是上游目录里的上下文窗口（token）；0 表示未知。
	ContextWindow int
}

type kiteRouterAutoCompactEntry struct {
	fetchedAt time.Time
	metadata  map[string]CodexModelMetadata
}

var (
	kiteRouterAutoCompactMu    sync.Mutex
	kiteRouterAutoCompactCache = make(map[string]kiteRouterAutoCompactEntry)
)

// CodexCatalogMetadata 返回「渠道在售模型名 → 元数据」，覆盖所有 Router 线渠道在售、
// 且属于 group 的模型。返回空 map 表示没有任何可下发的阈值（调用方应保持 fallback 语义）。
//
// 只在 Router 线（`base_url` 以 `/kite-router` 结尾）的渠道上算，其他渠道的模型不进表 ——
// 它们的上下文约束不由我们定义，贸然下发只会更糟。
func CodexCatalogMetadata(c *gin.Context, group string) map[string]CodexModelMetadata {
	now := time.Now()
	kiteRouterAutoCompactMu.Lock()
	entry, cached := kiteRouterAutoCompactCache[group]
	kiteRouterAutoCompactMu.Unlock()
	if cached && now.Sub(entry.fetchedAt) < kiteRouterAutoCompactCacheTTL {
		return entry.metadata
	}

	metadata := make(map[string]CodexModelMetadata)
	channels, err := model.GetEnabledChannelsWithKeysByType(constant.ChannelTypeKiteDelayed)
	if err == nil {
		for _, channel := range channels {
			if channel == nil {
				continue
			}
			base, isRouter := kiteRouterBaseURL(channel.GetBaseURL())
			if !isRouter {
				continue
			}
			if group != "" && !kiteRouterChannelServesGroup(channel, group) {
				continue
			}
			safeBudget := kiteRouterSafeBudgetUSDFor(channel.GetOtherSettings())
			// 上游目录是免鉴权的（带无效 key 也回 200），这里不需要传 key。
			catalog := kiteRouterCatalogForBase(c, base, channel.GetSetting().Proxy, "")
			for _, declared := range channel.GetModels() {
				declared = strings.TrimSpace(declared)
				if declared == "" {
					continue
				}
				upstream := kiteRouterUpstreamModelName(channel, catalog, declared)
				price, found := kiteRouterLookupPrice(catalog, upstream)
				if !found || !price.usable() {
					continue
				}
				tokens := price.thresholdBytes(safeBudget) / kiteRouterBytesPerTokenForDisplay
				if tokens <= 0 {
					continue
				}
				// 同一模型可能被多个 Router 渠道服务：取**最大**阈值（最宽松、最少误压）。
				current := metadata[declared]
				if tokens > current.AutoCompactTokenLimit {
					current.AutoCompactTokenLimit = tokens
				}
				if price.ContextLength > current.ContextWindow {
					current.ContextWindow = price.ContextLength
				}
				metadata[declared] = current
			}
		}
	}

	kiteRouterAutoCompactMu.Lock()
	kiteRouterAutoCompactCache[group] = kiteRouterAutoCompactEntry{fetchedAt: now, metadata: metadata}
	kiteRouterAutoCompactMu.Unlock()
	return metadata
}

func kiteRouterChannelServesGroup(channel *model.Channel, group string) bool {
	for _, candidate := range channel.GetGroups() {
		if strings.TrimSpace(candidate) == group {
			return true
		}
	}
	return false
}
