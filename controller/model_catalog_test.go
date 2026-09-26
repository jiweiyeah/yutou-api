package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/relay/channel/kitedelayed"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexCatalogEntryCarriesEveryRequiredModelInfoField 钉住「必填字段齐全」。
//
// Codex 的 ModelInfo 里没有 #[serde(default)] 的字段都是必填
// （codex-rs/protocol/src/openai_models.rs），少写一个会让客户端**整个目录解析失败** ——
// 那比现在的 400 更糟（所有模型都加载不出来）。所以这条测试按字段清单逐个断言。
func TestCodexCatalogEntryCarriesEveryRequiredModelInfoField(t *testing.T) {
	required := []string{
		"slug",
		"display_name",
		"description",
		"supported_reasoning_levels",
		"shell_type",
		"visibility",
		"supported_in_api",
		"priority",
		"availability_nux",
		"upgrade",
		"support_verbosity",
		"default_verbosity",
		"apply_patch_tool_type",
		"truncation_policy",
		"experimental_supported_tools",
	}

	entry := codexCatalogEntry("openai/gpt-6-astra", kitedelayed.CodexModelMetadata{})
	for _, field := range required {
		_, exists := entry[field]
		assert.True(t, exists, "必填字段缺失会让 Codex 整个目录解析失败: %s", field)
	}

	// 网关算不出阈值时不下发该字段，且 context_window 与 Codex 的 fallback 一致（272000），
	// 保证"没算出阈值"的模型行为与今天完全相同。
	assert.NotContains(t, entry, "auto_compact_token_limit")
	assert.Equal(t, 272000, entry["context_window"])
	assert.Equal(t, 272000, entry["max_context_window"])
	assert.Equal(t, "unified_exec", entry["shell_type"])
	assert.Equal(t, "list", entry["visibility"])
	assert.Equal(t, gin.H{"mode": "bytes", "limit": 10000}, entry["truncation_policy"])
}

func TestCodexCatalogEntryOverridesContextAndCompactLimit(t *testing.T) {
	entry := codexCatalogEntry("openai/gpt-6-astra", kitedelayed.CodexModelMetadata{
		AutoCompactTokenLimit: 64693,
		ContextWindow:         1050000,
	})
	assert.Equal(t, 64693, entry["auto_compact_token_limit"])
	assert.Equal(t, 1050000, entry["context_window"])
	assert.Equal(t, 1050000, entry["max_context_window"])

	// Codex 侧会把它夹到 0.9 × context_window，所以阈值必须落在夹取范围内才不会被削。
	limit, ok := entry["auto_compact_token_limit"].(int)
	require.True(t, ok)
	contextWindow, ok := entry["context_window"].(int)
	require.True(t, ok)
	assert.Less(t, limit, contextWindow*9/10, "阈值必须低于 Codex 的夹取上限，否则会被削小")
}
