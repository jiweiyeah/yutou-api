package service

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnnotateDeepSeekFreeTierLimitParsesResetTime(t *testing.T) {
	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
	message := fmt.Sprintf("You've used this period's free allowance. Your next rolling 7-day period starts at %s. Use the paid model to keep going.", resetAt.Format(time.RFC3339Nano))
	apiErr := types.WithOpenAIError(types.OpenAIError{
		Message: message,
		Type:    string(DeepSeekFreeTierLimitCode),
		Code:    string(DeepSeekFreeTierLimitCode),
	}, http.StatusTooManyRequests)

	matched := AnnotateDeepSeekFreeTierLimit(constant.ChannelTypeDeepSeek, apiErr)

	require.True(t, matched)
	assert.True(t, types.IsChannelAutoDisableError(apiErr))
	assert.Equal(t, resetAt.Unix(), types.GetChannelAutoDisableUntil(apiErr))
}

func TestAnnotateDeepSeekFreeTierLimitIgnoresOtherRateLimits(t *testing.T) {
	apiErr := types.WithOpenAIError(types.OpenAIError{
		Message: "rate limit exceeded",
		Type:    "rate_limit_error",
		Code:    "rate_limit_error",
	}, http.StatusTooManyRequests)

	matched := AnnotateDeepSeekFreeTierLimit(constant.ChannelTypeDeepSeek, apiErr)

	assert.False(t, matched)
	assert.False(t, types.IsChannelAutoDisableError(apiErr))
	assert.Zero(t, types.GetChannelAutoDisableUntil(apiErr))
}
