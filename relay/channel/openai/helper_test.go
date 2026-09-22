package openai

import (
	"strings"
	"testing"

	relayconstant "github.com/QuantumNous/new-api/relay/constant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RelayModeUnknown 是 Path2RelayMode 对未映射文本端点的兜底值，目前只有 /v1/messages 命中。
// 这类请求在 OpenAI 兼容上游被转成 chat completions 形态，响应也是 chat SSE，因此必须被
// 计入 responseTextBuilder；否则上游未回 usage 时 completion_tokens 会退化成 0（输出 token 不计费）。
func TestProcessTokenData_UnknownModeCountsChatStreamText(t *testing.T) {
	const chunk = `{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"PONG"},"finish_reason":null}]}`

	for _, relayMode := range []int{relayconstant.RelayModeChatCompletions, relayconstant.RelayModeUnknown} {
		var builder strings.Builder
		toolCount := 0
		err := processTokenData(relayMode, chunk, &builder, &toolCount)
		require.NoError(t, err)
		assert.Equal(t, "PONG", builder.String(), "relayMode=%d", relayMode)
	}
}

func TestProcessTokenData_UnknownModeCountsReasoningAndToolCalls(t *testing.T) {
	const chunk = `{"choices":[{"index":0,"delta":{"reasoning_content":"think ","tool_calls":[{"index":0,"function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`

	var builder strings.Builder
	toolCount := 0
	require.NoError(t, processTokenData(relayconstant.RelayModeUnknown, chunk, &builder, &toolCount))
	assert.Equal(t, `think f{"a":1}`, builder.String())
	assert.Equal(t, 1, toolCount)
}

func TestProcessTokenData_CompletionsModeUnchanged(t *testing.T) {
	const chunk = `{"choices":[{"index":0,"text":"hello"}]}`

	var builder strings.Builder
	toolCount := 0
	require.NoError(t, processTokenData(relayconstant.RelayModeCompletions, chunk, &builder, &toolCount))
	assert.Equal(t, "hello", builder.String())
}

func TestProcessTokenData_UnknownModePropagatesBadJSON(t *testing.T) {
	var builder strings.Builder
	toolCount := 0
	assert.Error(t, processTokenData(relayconstant.RelayModeUnknown, "not-json", &builder, &toolCount))
	assert.Empty(t, builder.String())
}
