package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func callCountClaudeTokens(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	CountClaudeTokens(ctx)

	var payload map[string]any
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	return recorder.Code, payload
}

// Anthropic 语义：只返回 input_tokens，不生成、不计费。
func TestCountClaudeTokens_ReturnsOnlyInputTokens(t *testing.T) {
	code, payload := callCountClaudeTokens(t,
		`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello world"}]}`)

	assert.Equal(t, http.StatusOK, code)
	tokens, ok := payload["input_tokens"].(float64)
	require.True(t, ok, "response should carry a numeric input_tokens")
	assert.Greater(t, tokens, float64(0))
	assert.Len(t, payload, 1)
}

func TestCountClaudeTokens_CountsToolsAndSystem(t *testing.T) {
	plainCode, plain := callCountClaudeTokens(t,
		`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello world"}]}`)
	richCode, rich := callCountClaudeTokens(t,
		`{"model":"claude-sonnet-4","system":"You are concise.",`+
			`"messages":[{"role":"user","content":"hello world"}],`+
			`"tools":[{"name":"get_weather","description":"Get weather for a city",`+
			`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)

	require.Equal(t, http.StatusOK, plainCode)
	require.Equal(t, http.StatusOK, richCode)
	assert.Greater(t, rich["input_tokens"].(float64), plain["input_tokens"].(float64))
}

// 校验失败要返回 400 与 Anthropic 认得的 error.type，而不是内部错误类型。
func TestCountClaudeTokens_RejectsInvalidBody(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing messages", `{"model":"claude-sonnet-4"}`},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"broken json", `{"model":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, payload := callCountClaudeTokens(t, tc.body)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "error", payload["type"])

			errObj, ok := payload["error"].(map[string]any)
			require.True(t, ok, "error body should be an object")
			assert.Equal(t, "invalid_request_error", errObj["type"])
			assert.NotEmpty(t, errObj["message"])
		})
	}
}
