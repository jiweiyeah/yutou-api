package claudemessages

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeMessagesRequestToOpenAIChatPreservesAssistantThinking(t *testing.T) {
	request := dto.ClaudeRequest{
		Model: "moonshotai/kimi-k3",
		Messages: []dto.ClaudeMessage{
			{
				Role: "assistant",
				Content: []dto.ClaudeMediaMessage{
					{Type: "thinking", Thinking: common.GetPointer("first thought")},
					{Type: "thinking", Thinking: common.GetPointer("second thought")},
					{Type: "text", Text: common.GetPointer("")},
				},
			},
		},
	}

	converted, err := ClaudeMessagesRequestToOpenAIChat(request, nil)

	require.NoError(t, err)
	require.Len(t, converted.Messages, 1)
	assert.Equal(t, "assistant", converted.Messages[0].Role)
	assert.Equal(t, "first thought\n\nsecond thought", converted.Messages[0].GetReasoningContent())
	assert.Empty(t, converted.Messages[0].ParseContent())
}

func TestClaudeMessagesRequestToOpenAIChatDropsEmptyAssistantMessage(t *testing.T) {
	request := dto.ClaudeRequest{
		Model: "moonshotai/kimi-k3",
		Messages: []dto.ClaudeMessage{
			{Role: "assistant", Content: "  \n"},
			{Role: "user", Content: "continue"},
		},
	}

	converted, err := ClaudeMessagesRequestToOpenAIChat(request, nil)

	require.NoError(t, err)
	require.Len(t, converted.Messages, 1)
	assert.Equal(t, "user", converted.Messages[0].Role)
	assert.Equal(t, "continue", converted.Messages[0].StringContent())
}
