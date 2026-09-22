package service

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countClaudeInputTokens(t *testing.T, body string) int {
	t.Helper()
	var request dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &request))
	return CountClaudeMessagesInputTokens(&request)
}

func TestCountClaudeMessagesInputTokens_NilRequest(t *testing.T) {
	assert.Equal(t, 0, CountClaudeMessagesInputTokens(nil))
}

// 即便正文极短，也应保留请求头部与每条消息的结构开销，否则客户端会低估上下文。
func TestCountClaudeMessagesInputTokens_IncludesStructuralOverhead(t *testing.T) {
	got := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	assert.Greater(t, got, 3, "should include request header and per-message overhead")
}

func TestCountClaudeMessagesInputTokens_GrowsWithContent(t *testing.T) {
	short := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	long := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"`+
		strings.Repeat("hello world ", 500)+`"}]}`)
	assert.Greater(t, long, short)
}

func TestCountClaudeMessagesInputTokens_CountsSystem(t *testing.T) {
	plain := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	withSystem := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","system":"`+
		strings.Repeat("be brief ", 200)+`","messages":[{"role":"user","content":"hi"}]}`)
	assert.Greater(t, withSystem, plain)
}

// 请求体经 JSON 反序列化后 Tools 里是 map[string]any，会被 dto.ProcessTools 跳过，
// 所以工具必须在本函数里单独计入，否则带工具的请求会被明显低估。
func TestCountClaudeMessagesInputTokens_CountsTools(t *testing.T) {
	const tool = `{"name":"get_weather","description":"Get the weather for a city",` +
		`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}`

	plain := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	withTools := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],`+
		`"tools":[`+tool+`]}`)

	assert.Greater(t, withTools, plain)

	// 工具越多，估算越大
	more := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],`+
		`"tools":[`+tool+`,`+tool+`,`+tool+`]}`)
	assert.Greater(t, more, withTools)
}

func TestCountClaudeMessagesInputTokens_CountsImages(t *testing.T) {
	withoutImage := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"look"}]}`)
	withImage := countClaudeInputTokens(t, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png",`+
		`"data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}}]}]}`)
	assert.Greater(t, withImage, withoutImage)
}
