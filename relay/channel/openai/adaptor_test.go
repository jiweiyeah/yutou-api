package openai

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveEmptyAssistantMessages(t *testing.T) {
	reasoning := "thinking"
	toolMessage := dto.Message{Role: "assistant"}
	toolMessage.SetToolCalls([]dto.ToolCallRequest{{
		ID:   "call_1",
		Type: "function",
		Function: dto.FunctionRequest{
			Name:      "lookup",
			Arguments: `{}`,
		},
	}})
	request := &dto.GeneralOpenAIRequest{
		Messages: []dto.Message{
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: nil},
			{Role: "assistant", Content: "  \n"},
			{Role: "assistant", Content: []any{}},
			{Role: "assistant", Content: []any{map[string]any{"type": dto.ContentTypeImageURL}}},
			{Role: "assistant", Content: "answer"},
			{Role: "assistant", ReasoningContent: &reasoning},
			toolMessage,
		},
	}

	request.Messages = removeEmptyAssistantMessages(request.Messages)

	require.Len(t, request.Messages, 4)
	assert.Equal(t, "user", request.Messages[0].Role)
	assert.Equal(t, "answer", request.Messages[1].StringContent())
	assert.Equal(t, "thinking", request.Messages[2].GetReasoningContent())
	assert.Equal(t, "assistant", request.Messages[3].Role)
}

func TestConvertClaudeRequestMirrorsKimiReasoningForCustomChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	thought := "previous thought"
	request := &dto.ClaudeRequest{
		Model: "moonshotai/kimi-k3-free",
		Messages: []dto.ClaudeMessage{{
			Role: "assistant",
			Content: []dto.ClaudeMediaMessage{{
				Type:     "thinking",
				Thinking: &thought,
			}},
		}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeCustom,
			ChannelBaseUrl:    "https://api.tokenrouter.com/v1/chat/completions",
			UpstreamModelName: "moonshotai/kimi-k3-free",
		},
	}

	converted, err := (&Adaptor{}).ConvertClaudeRequest(c, info, request)

	require.NoError(t, err)
	openAIRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Len(t, openAIRequest.Messages, 1)
	require.NotNil(t, openAIRequest.Messages[0].ReasoningContent)
	require.NotNil(t, openAIRequest.Messages[0].Reasoning)
	assert.Equal(t, "previous thought", *openAIRequest.Messages[0].ReasoningContent)
	assert.Equal(t, "previous thought", *openAIRequest.Messages[0].Reasoning)
}

func TestConvertClaudeRequestNormalizesTokenRouterAssistantTextBlocks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	firstText := "Earlier "
	secondText := "answer"
	request := &dto.ClaudeRequest{
		Model: "moonshotai/kimi-k3-free",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "Question"},
			{Role: "assistant", Content: []dto.ClaudeMediaMessage{
				{Type: "text", Text: &firstText},
				{Type: "text", Text: &secondText},
			}},
			{Role: "user", Content: "Continue"},
		},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeCustom,
			ChannelBaseUrl:    "https://api.tokenrouter.com/v1/chat/completions",
			UpstreamModelName: "moonshotai/kimi-k3-free",
		},
	}

	converted, err := (&Adaptor{}).ConvertClaudeRequest(c, info, request)
	require.NoError(t, err)
	openAIRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Len(t, openAIRequest.Messages, 3)
	assert.True(t, openAIRequest.Messages[1].IsStringContent())
	assert.Equal(t, "Earlier answer", openAIRequest.Messages[1].StringContent())
}

func TestConvertClaudeRequestDoesNotMirrorReasoningForOtherCustomChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	thought := "previous thought"
	request := &dto.ClaudeRequest{
		Model: "moonshotai/kimi-k3-free",
		Messages: []dto.ClaudeMessage{{
			Role: "assistant",
			Content: []dto.ClaudeMediaMessage{{
				Type:     "thinking",
				Thinking: &thought,
			}},
		}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeCustom,
			ChannelBaseUrl:    "https://example.com/v1/chat/completions",
			UpstreamModelName: "moonshotai/kimi-k3-free",
		},
	}

	converted, err := (&Adaptor{}).ConvertClaudeRequest(c, info, request)

	require.NoError(t, err)
	openAIRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Len(t, openAIRequest.Messages, 1)
	require.NotNil(t, openAIRequest.Messages[0].ReasoningContent)
	assert.Nil(t, openAIRequest.Messages[0].Reasoning)
}
