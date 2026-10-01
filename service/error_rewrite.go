package service

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Placeholders understood inside ChannelErrorRewriteRule.Message.
const (
	errorRewritePlaceholderBodyBytes = "{body_bytes}"
	errorRewritePlaceholderBodyKB    = "{body_kb}"
	errorRewritePlaceholderModel     = "{model}"
	errorRewritePlaceholderUpstream  = "{upstream_message}"
)

// upstreamShapedErrorTypes lists the error types that carry an upstream
// response. Errors raised by the gateway itself (billing, auth, request
// parsing) are deliberately excluded: their message is already accurate, and a
// channel-level rule must not be able to mask them.
var upstreamShapedErrorTypes = map[types.ErrorType]struct{}{
	types.ErrorTypeOpenAIError:   {},
	types.ErrorTypeClaudeError:   {},
	types.ErrorTypeGeminiError:   {},
	types.ErrorTypeRerankError:   {},
	types.ErrorTypeUpstreamError: {},
}

// ApplyChannelErrorRewrite rewrites a channel's upstream error into the wording
// configured on that channel, so the downstream client is not shown an upstream
// message that misattributes the cause.
//
// It is a no-op unless the channel carries at least one error_rewrite rule and
// the error actually came back from an upstream. requestBodyBytes is the size of
// the request body the relay sent (0 when unknown); it feeds the {body_bytes}
// and {body_kb} placeholders.
//
// The caller is expected to invoke this before the retry decision: a rule that
// lowers the status code (or sets skip_retry) is what stops the relay loop from
// re-sending a request that is guaranteed to fail the same way.
func ApplyChannelErrorRewrite(c *gin.Context, err *types.NewAPIError, requestBodyBytes int64) *types.NewAPIError {
	if c == nil || err == nil {
		return err
	}
	settings, ok := common.GetContextKeyType[dto.ChannelOtherSettings](c, constant.ContextKeyChannelOtherSetting)
	if !ok || len(settings.ErrorRewrite) == 0 {
		return err
	}
	if !isRewriteableUpstreamError(err) {
		return err
	}

	rawMessage := err.Error()
	lowered := strings.ToLower(rawMessage)
	for i, rule := range settings.ErrorRewrite {
		needle := strings.ToLower(strings.TrimSpace(rule.Match))
		if needle == "" || !strings.Contains(lowered, needle) {
			continue
		}
		applyErrorRewriteRule(c, err, rule, rawMessage, requestBodyBytes)
		logger.LogInfo(c, fmt.Sprintf(
			"upstream error rewritten by channel error_rewrite[%d]: %q -> %q (status %d)",
			i, common.LocalLogPreview(rawMessage), common.LocalLogPreview(err.Error()), err.StatusCode))
		return err
	}
	return err
}

func isRewriteableUpstreamError(err *types.NewAPIError) bool {
	if err.StatusCode < 400 || err.StatusCode > 599 {
		return false
	}
	// "channel:" errors are produced by the gateway while talking to the
	// channel (no available key, bad override, ...); they never carry an
	// upstream response and must stay verbatim.
	if types.IsChannelError(err) {
		return false
	}
	_, ok := upstreamShapedErrorTypes[err.GetErrorType()]
	return ok
}

func applyErrorRewriteRule(c *gin.Context, err *types.NewAPIError, rule dto.ChannelErrorRewriteRule, rawMessage string, requestBodyBytes int64) {
	if rule.StatusCode != 0 {
		err.StatusCode = rule.StatusCode
	}
	if rule.SkipRetry {
		types.ErrOptionWithSkipRetry()(err)
	}
	if code := strings.TrimSpace(rule.Code); code != "" {
		types.ErrOptionWithErrorCode(types.ErrorCode(code))(err)
	}

	message := strings.TrimSpace(rule.Message)
	if message == "" {
		// Status/code-only rewrite: the client still gets a readable error only
		// if the code changed, so leave the upstream wording alone.
		if strings.TrimSpace(rule.Code) != "" {
			patchRelayErrorPayload(err, "", strings.TrimSpace(rule.Code))
		}
		return
	}
	message = expandErrorRewritePlaceholders(message, c, rawMessage, requestBodyBytes)
	err.SetMessage(message)
	patchRelayErrorPayload(err, message, strings.TrimSpace(rule.Code))
}

// patchRelayErrorPayload keeps the serialized payload in sync with Err.
// ToOpenAIError / ToClaudeError read the message back out of RelayError for
// upstream-shaped errors, so SetMessage alone would not reach the client.
func patchRelayErrorPayload(err *types.NewAPIError, message string, code string) {
	switch payload := err.RelayError.(type) {
	case types.OpenAIError:
		if message != "" {
			payload.Message = message
		}
		if code != "" {
			payload.Code = code
			payload.Type = code
		}
		err.RelayError = payload
	case *types.OpenAIError:
		if payload == nil {
			return
		}
		if message != "" {
			payload.Message = message
		}
		if code != "" {
			payload.Code = code
			payload.Type = code
		}
	case types.ClaudeError:
		if message != "" {
			payload.Message = message
		}
		if code != "" {
			payload.Type = code
		}
		err.RelayError = payload
	}
}

func expandErrorRewritePlaceholders(template string, c *gin.Context, upstreamMessage string, requestBodyBytes int64) string {
	bodyBytes := "unknown"
	bodyKB := "unknown"
	if requestBodyBytes > 0 {
		bodyBytes = strconv.FormatInt(requestBodyBytes, 10)
		bodyKB = strconv.FormatInt((requestBodyBytes+1023)/1024, 10)
	}
	model := ""
	if c != nil {
		model = c.GetString("original_model")
	}
	return strings.NewReplacer(
		errorRewritePlaceholderBodyBytes, bodyBytes,
		errorRewritePlaceholderBodyKB, bodyKB,
		errorRewritePlaceholderModel, model,
		errorRewritePlaceholderUpstream, upstreamMessage,
	).Replace(template)
}
