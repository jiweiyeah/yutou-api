package controller

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestShouldRetryHonoursForcedRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	forced504 := types.NewErrorWithStatusCode(
		errors.New("Kite Delayed router failed: upstream returned HTTP 504"),
		types.ErrorCodeDoRequestFailed, 504,
		types.ErrOptionWithSkipRetry(), types.ErrOptionWithForceRetry(),
	)
	skip504 := types.NewErrorWithStatusCode(
		errors.New("Kite Delayed poll failed: upstream returned HTTP 504"),
		types.ErrorCodeDoRequestFailed, 504,
		types.ErrOptionWithSkipRetry(),
	)
	plain502 := types.NewErrorWithStatusCode(
		errors.New("upstream returned HTTP 502"),
		types.ErrorCodeDoRequestFailed, 502,
	)

	tests := []struct {
		name       string
		err        *types.NewAPIError
		retryTimes int
		setup      func(c *gin.Context)
		expected   bool
	}{
		{
			name:       "forced retry survives the 504 always-skip status list",
			err:        forced504,
			retryTimes: 5,
			expected:   true,
		},
		{
			name:       "skip retry without force still blocks 504",
			err:        skip504,
			retryTimes: 5,
			expected:   false,
		},
		{
			name:       "forced retry still needs a retry budget",
			err:        forced504,
			retryTimes: 0,
			expected:   false,
		},
		{
			name:       "forced retry never moves a pinned channel",
			err:        forced504,
			retryTimes: 5,
			setup:      func(c *gin.Context) { c.Set("specific_channel_id", 10867) },
			expected:   false,
		},
		{
			name:       "forced retry never overrides channel affinity",
			err:        forced504,
			retryTimes: 5,
			setup:      func(c *gin.Context) { c.Set("channel_affinity_skip_retry_on_failure", true) },
			expected:   false,
		},
		{
			name:       "status code table still applies without force",
			err:        plain502,
			retryTimes: 5,
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tt.setup != nil {
				tt.setup(c)
			}
			assert.Equal(t, tt.expected, shouldRetry(c, tt.err, tt.retryTimes))
		})
	}
}
