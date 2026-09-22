package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// CountClaudeTokens 实现 Anthropic 的 POST /v1/messages/count_tokens：只统计输入 token，
// 不选渠道、不生成、不计费，响应形如 {"input_tokens": 123}。
//
// 数值是本地估算（见 service.CountClaudeMessagesInputTokens），不是上游精确计数。
// 该接口此前缺失，Claude 客户端调用会 404 后自行降级为本地估算；补上之后估算挪到网关侧，
// 用与计费同源的 tokenizer，口径更贴近日志里的 prompt_tokens。
func CountClaudeTokens(c *gin.Context) {
	request, err := helper.GetAndValidateClaudeRequest(c)
	if err != nil {
		// 用 Claude 原生错误类型：Anthropic 客户端会按 error.type 分支处理，
		// 默认的 new_api_error 不在其已知类型里。
		newAPIError := types.WithClaudeError(types.ClaudeError{
			Type:    "invalid_request_error",
			Message: err.Error(),
		}, http.StatusBadRequest)
		c.JSON(newAPIError.StatusCode, gin.H{
			"type":  "error",
			"error": newAPIError.ToClaudeError(),
		})
		return
	}

	tokens := service.CountClaudeMessagesInputTokens(request)
	logger.LogDebug(c, "count tokens: model=%s messages=%d tools=%d input_tokens=%d",
		request.Model, len(request.Messages), len(request.GetTools()), tokens)

	c.JSON(http.StatusOK, gin.H{
		"input_tokens": tokens,
	})
}
