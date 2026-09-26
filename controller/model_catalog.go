package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/kitedelayed"

	"github.com/gin-gonic/gin"
)

// ModelCatalog 下发 Codex 可用的模型目录（客户端把 provider 的
// `model_catalog_url` 指向本端点即可）。
//
// 存在的唯一理由：Codex 只在自己不认识模型名时用 fallback 元数据，而那份 fallback 里
// `auto_compact_token_limit` 是 **None** —— 它要等到 `context_window × 95% ≈ 25.8 万 token`
// 才压缩，比 Kite Router 的字节制安全线（astra 258,773 字节 ≈ 6.5 万 token 口径）
// **晚了约 4 倍**。结果撞线时它不会自动压缩，而是把超限请求发出来被网关闸门拒掉。
// 把阈值下发给它，客户端就会在撞线之前自己压缩。
//
// ⚠️ 字段集**逐字段镜像** Codex 自己的 fallback 元数据
// （`codex-rs/models-manager/src/model_info.rs` 的 `model_info_from_slug`）：
// `ModelInfo` 里没有 `#[serde(default)]` 的字段都是**必填**，少写一个会让客户端
// **整个目录解析失败**（比现在更糟）。所以这里不做任何"精简"，
// 默认值也与 fallback 保持一致 —— 算不出阈值的模型行为与今天完全相同。
func ModelCatalog(c *gin.Context) {
	groups, err := getModelListGroups(c)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "get user group failed"})
		return
	}
	group := ""
	if len(groups.ownerGroups) > 0 {
		group = groups.ownerGroups[0]
	}

	models := model.GetGroupEnabledModels(group)
	metadata := kitedelayed.CodexCatalogMetadata(c, group)

	entries := make([]gin.H, 0, len(models))
	for _, name := range models {
		entries = append(entries, codexCatalogEntry(name, metadata[name]))
	}
	c.JSON(http.StatusOK, gin.H{"models": entries})
}

// codexCatalogEntry 组装一条 ModelInfo。除 slug 与两个上下文字段外，取值与 Codex 的
// fallback 元数据一致（`shell_type=unified_exec`、`truncation_policy=bytes/10000`、
// `visibility=list` 等），这样"网关没算出阈值"的模型不会因为本端点而改变行为。
func codexCatalogEntry(slug string, meta kitedelayed.CodexModelMetadata) gin.H {
	const fallbackContextWindow = 272000
	contextWindow := meta.ContextWindow
	if contextWindow <= 0 {
		contextWindow = fallbackContextWindow
	}
	entry := gin.H{
		"slug":                         slug,
		"display_name":                 slug,
		"description":                  nil,
		"supported_reasoning_levels":   []any{},
		"shell_type":                   "unified_exec",
		"visibility":                   "list",
		"supported_in_api":             true,
		"priority":                     1,
		"availability_nux":             nil,
		"upgrade":                      nil,
		"support_verbosity":            false,
		"default_verbosity":            nil,
		"apply_patch_tool_type":        nil,
		"truncation_policy":            gin.H{"mode": "bytes", "limit": 10000},
		"experimental_supported_tools": []any{},
		"context_window":               contextWindow,
		"max_context_window":           contextWindow,
	}
	if meta.AutoCompactTokenLimit > 0 {
		// Codex 侧会把它夹到 0.9 × context_window（见 ModelInfo::auto_compact_token_limit），
		// 所以 context_window 取上游真实窗口时这个值不会被夹掉。
		entry["auto_compact_token_limit"] = meta.AutoCompactTokenLimit
	}
	return entry
}
