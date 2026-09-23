package relay

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplySystemPromptToResponsesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newInfo := func(prompt string, override bool) *relaycommon.RelayInfo {
		return &relaycommon.RelayInfo{
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelSetting: dto.ChannelSettings{
					SystemPrompt:         prompt,
					SystemPromptOverride: override,
				},
			},
		}
	}

	// Adapters hand back dto.OpenAIResponsesRequest by value, so the value case
	// is the one that actually runs in production.
	instructionsOf := func(t *testing.T, converted any) string {
		t.Helper()
		request, ok := converted.(dto.OpenAIResponsesRequest)
		require.True(t, ok, "expected dto.OpenAIResponsesRequest, got %T", converted)
		return string(request.Instructions)
	}

	t.Run("empty instructions get the channel prompt", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", false), dto.OpenAIResponsesRequest{})
		assert.Equal(t, `"GUARD"`, instructionsOf(t, out))
		assert.False(t, common.GetContextKeyBool(c, constant.ContextKeySystemPromptOverride))
	})

	t.Run("existing instructions are kept without override", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		in := dto.OpenAIResponsesRequest{Instructions: json.RawMessage(`"client"`)}
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", false), in)
		assert.Equal(t, `"client"`, instructionsOf(t, out))
	})

	t.Run("override prepends to existing instructions", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		in := dto.OpenAIResponsesRequest{Instructions: json.RawMessage(`"client"`)}
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", true), in)
		assert.Equal(t, `"GUARD\nclient"`, instructionsOf(t, out))
		assert.True(t, common.GetContextKeyBool(c, constant.ContextKeySystemPromptOverride))
	})

	t.Run("override replaces blank instructions", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		in := dto.OpenAIResponsesRequest{Instructions: json.RawMessage(`"   "`)}
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", true), in)
		assert.Equal(t, `"GUARD"`, instructionsOf(t, out))
	})

	t.Run("pointer requests are handled too", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", false), &dto.OpenAIResponsesRequest{})
		request, ok := out.(*dto.OpenAIResponsesRequest)
		require.True(t, ok, "expected *dto.OpenAIResponsesRequest, got %T", out)
		assert.Equal(t, `"GUARD"`, string(request.Instructions))
	})

	t.Run("no channel prompt is a no-op", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		in := dto.OpenAIResponsesRequest{Instructions: json.RawMessage(`"client"`)}
		out := applySystemPromptToResponsesRequest(c, newInfo("", true), in)
		assert.Equal(t, `"client"`, instructionsOf(t, out))
	})

	t.Run("chat converted requests fall back to the chat rules", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		in := &dto.GeneralOpenAIRequest{Messages: []dto.Message{{Role: "user", Content: "hi"}}}
		out := applySystemPromptToResponsesRequest(c, newInfo("GUARD", true), in)
		request, ok := out.(*dto.GeneralOpenAIRequest)
		require.True(t, ok)
		require.Len(t, request.Messages, 2)
		assert.Equal(t, "system", request.Messages[0].Role)
		assert.Equal(t, "GUARD", request.Messages[0].StringContent())
	})
}

func TestIsResponsesEventStreamContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "plain", contentType: "text/event-stream", want: true},
		{name: "mixed case with charset", contentType: "Text/Event-Stream; charset=utf-8", want: true},
		{name: "json", contentType: "application/json", want: false},
		{name: "empty", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isResponsesEventStreamContentType(tt.contentType))
		})
	}
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}
