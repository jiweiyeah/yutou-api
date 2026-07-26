package advancedcustom

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const (
	lovableMaxToolDescriptionLength = 4096

	lovableAdditionalToolsInputType = "additional_tools"
	lovableCustomToolCallType       = "custom_tool_call"
	lovableCustomToolOutputType     = "custom_tool_call_output"
	lovableFunctionToolOutputType   = "function_call_output"

	lovableFunctionArgumentsDeltaEvent = "response.function_call_arguments.delta"
	lovableFunctionArgumentsDoneEvent  = "response.function_call_arguments.done"
	lovableCustomToolInputDeltaEvent   = "response.custom_tool_call_input.delta"
	lovableOutputItemAddedEvent        = "response.output_item.added"
	lovableOutputItemDoneEvent         = "response.output_item.done"
	lovableResponseCompletedEvent      = "response.completed"
	lovableResponseIncompleteEvent     = "response.incomplete"
)

type lovableCustomToolCall struct {
	Name  string `json:"name,omitempty"`
	Input string `json:"input,omitempty"`
}

type lovableStreamToolCall struct {
	Index  *int                   `json:"index,omitempty"`
	ID     string                 `json:"id,omitempty"`
	Type   any                    `json:"type"`
	Custom *lovableCustomToolCall `json:"custom,omitempty"`
}

type lovableStreamChunk struct {
	Choices []struct {
		Delta struct {
			ToolCalls []lovableStreamToolCall `json:"tool_calls,omitempty"`
		} `json:"delta,omitempty"`
	} `json:"choices"`
}

type lovableStreamToolTracker struct {
	customByChatIndex   map[int]bool
	customCallIDs       map[string]struct{}
	customOutputIndexes map[int]struct{}
	pendingToolKinds    []bool
}

func normalizeLovableChatTools(tools []dto.ToolCallRequest) ([]dto.ToolCallRequest, error) {
	normalized := make([]dto.ToolCallRequest, 0, len(tools))
	for i := range tools {
		if tools[i].Type == "function" {
			descriptionRunes := []rune(tools[i].Function.Description)
			if len(descriptionRunes) > lovableMaxToolDescriptionLength {
				tools[i].Function.Description = string(descriptionRunes[:lovableMaxToolDescriptionLength])
			}
			normalized = append(normalized, tools[i])
			continue
		}
		if tools[i].Type != dto.CustomType {
			continue
		}
		if len(tools[i].Custom) == 0 {
			normalized = append(normalized, tools[i])
			continue
		}

		var responsesCustom map[string]any
		if err := common.Unmarshal(tools[i].Custom, &responsesCustom); err != nil {
			return nil, fmt.Errorf("invalid Lovable custom tool at index %d: %w", i, err)
		}

		chatCustom := make(map[string]any, 3)
		for _, key := range []string{"name", "description"} {
			if value, ok := responsesCustom[key]; ok {
				if key == "description" {
					if description, ok := value.(string); ok {
						descriptionRunes := []rune(description)
						if len(descriptionRunes) > lovableMaxToolDescriptionLength {
							value = string(descriptionRunes[:lovableMaxToolDescriptionLength])
						}
					}
				}
				chatCustom[key] = value
			}
		}
		if rawFormat, ok := responsesCustom["format"]; ok {
			format, ok := rawFormat.(map[string]any)
			if !ok {
				chatCustom["format"] = rawFormat
			} else {
				chatFormat := map[string]any{"type": format["type"]}
				if format["type"] == "grammar" {
					if grammar, ok := format["grammar"]; ok {
						chatFormat["grammar"] = grammar
					} else {
						chatFormat["grammar"] = map[string]any{
							"syntax":     format["syntax"],
							"definition": format["definition"],
						}
					}
				}
				chatCustom["format"] = chatFormat
			}
		}

		custom, err := common.Marshal(chatCustom)
		if err != nil {
			return nil, fmt.Errorf("marshal Lovable custom tool at index %d: %w", i, err)
		}
		tools[i].Custom = custom
		normalized = append(normalized, tools[i])
	}
	return normalized, nil
}

func prepareLovableResponsesRequest(request *dto.OpenAIResponsesRequest) error {
	if request == nil || len(request.Input) == 0 || common.GetJsonType(request.Input) != "array" {
		return nil
	}

	var inputItems []map[string]any
	if err := common.Unmarshal(request.Input, &inputItems); err != nil {
		return fmt.Errorf("invalid Lovable input array: %w", err)
	}

	tools := make([]any, 0)
	if len(request.Tools) > 0 {
		if err := common.Unmarshal(request.Tools, &tools); err != nil {
			return fmt.Errorf("invalid Lovable tools: %w", err)
		}
	}

	changed := false
	normalizedInput := make([]map[string]any, 0, len(inputItems))
	for _, item := range inputItems {
		switch strings.TrimSpace(common.Interface2String(item["type"])) {
		case lovableAdditionalToolsInputType:
			additionalTools, ok := item["tools"].([]any)
			if !ok {
				return errors.New("Lovable additional_tools item has invalid tools")
			}
			for _, tool := range additionalTools {
				if _, ok := tool.(map[string]any); !ok {
					return errors.New("Lovable additional_tools item contains an invalid tool")
				}
				tools = append(tools, tool)
			}
			changed = true
		case lovableCustomToolOutputType:
			item["type"] = lovableFunctionToolOutputType
			normalizedInput = append(normalizedInput, item)
			changed = true
		default:
			normalizedInput = append(normalizedInput, item)
		}
	}
	if !changed {
		return nil
	}

	input, err := common.Marshal(normalizedInput)
	if err != nil {
		return fmt.Errorf("marshal Lovable input: %w", err)
	}
	request.Input = input
	if len(tools) > 0 {
		request.Tools, err = common.Marshal(tools)
		if err != nil {
			return fmt.Errorf("marshal Lovable tools: %w", err)
		}
	}
	return nil
}

func lovableChatToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(errors.New("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResponse dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResponse); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if openAIError := chatResponse.GetOpenAIError(); openAIError != nil && openAIError.Type != "" {
		return nil, types.WithOpenAIError(*openAIError, resp.StatusCode)
	}

	if responseID := helper.GetResponseID(c); responseID != "" {
		chatResponse.Id = responseID
	}
	result, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responsesResponse, ok := result.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", result.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	usage := result.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(responsesResponse)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		responsesResponse.Usage = relayconvert.UsageFromChatUsage(usage)
	}

	responseBody, err := marshalLovableResponsesResponse(responsesResponse, &chatResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func marshalLovableResponsesResponse(response *dto.OpenAIResponsesResponse, chatResponse *dto.OpenAITextResponse) ([]byte, error) {
	body, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}

	var payload map[string]any
	if err := common.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	customCalls := make(map[string]lovableCustomToolCall)
	if chatResponse != nil && len(chatResponse.Choices) > 0 {
		for _, toolCall := range chatResponse.Choices[0].Message.ParseToolCalls() {
			if toolCall.Type != dto.CustomType && len(toolCall.Custom) == 0 {
				continue
			}
			custom := lovableCustomToolCall{
				Name:  toolCall.Function.Name,
				Input: toolCall.Function.Arguments,
			}
			if len(toolCall.Custom) > 0 {
				if err := common.Unmarshal(toolCall.Custom, &custom); err != nil {
					return nil, fmt.Errorf("invalid Lovable custom tool call: %w", err)
				}
			}
			customCalls[toolCall.ID] = custom
		}
	}

	outputs, _ := payload["output"].([]any)
	for _, rawOutput := range outputs {
		output, ok := rawOutput.(map[string]any)
		if !ok || strings.TrimSpace(common.Interface2String(output["type"])) != dto.CustomType {
			continue
		}

		custom := customCalls[strings.TrimSpace(common.Interface2String(output["call_id"]))]
		if arguments, ok := output["arguments"].(map[string]any); ok {
			if name := strings.TrimSpace(common.Interface2String(arguments["name"])); name != "" {
				custom.Name = name
			}
			if input, ok := arguments["input"]; ok {
				custom.Input = common.Interface2String(input)
			}
		}
		output["type"] = lovableCustomToolCallType
		output["name"] = custom.Name
		output["input"] = custom.Input
		delete(output, "arguments")
	}
	return common.Marshal(payload)
}

func lovableChatToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(errors.New("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, relayconvert.ResponseStreamOptions{
		ID:    helper.GetResponseID(c),
		Model: info.UpstreamModelName,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	tracker := &lovableStreamToolTracker{
		customByChatIndex:   make(map[int]bool),
		customCallIDs:       make(map[string]struct{}),
		customOutputIndexes: make(map[int]struct{}),
	}
	var streamError *types.NewAPIError

	sendEvent := func(event relayconvert.ChatToResponsesStreamEvent) bool {
		eventType, data, skip, err := tracker.marshalEvent(event)
		if err != nil {
			streamError = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		if skip {
			return true
		}
		if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: eventType}, string(data)); err != nil {
			streamError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, streamResult *helper.StreamResult) {
		if streamError != nil {
			streamResult.Stop(streamError)
			return
		}

		var errorResponse dto.OpenAITextResponse
		if err := common.UnmarshalJsonStr(data, &errorResponse); err == nil {
			if openAIError := errorResponse.GetOpenAIError(); openAIError != nil && openAIError.Type != "" {
				streamError = types.WithOpenAIError(*openAIError, resp.StatusCode)
				streamResult.Stop(streamError)
				return
			}
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			logger.LogError(c, "failed to unmarshal Lovable chat stream response: "+err.Error())
			streamResult.Error(err)
			return
		}
		if err := tracker.normalizeChunk(data, &chunk); err != nil {
			streamError = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			streamResult.Stop(streamError)
			return
		}

		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			streamError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			streamResult.Stop(streamError)
			return
		}
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamError = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				streamResult.Stop(streamError)
				return
			}
			if !sendEvent(event) {
				streamResult.Stop(streamError)
				return
			}
		}
	})
	if streamError != nil {
		return nil, streamError
	}

	usage := state.Usage()
	if usage == nil || usage.TotalTokens == 0 {
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}
	finalResults, err := relayconvert.FinalizeStreamResponse(c, info, state)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
		if !ok {
			return nil, types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if !sendEvent(event) {
			return nil, streamError
		}
	}
	return usage, nil
}

func (t *lovableStreamToolTracker) normalizeChunk(data string, chunk *dto.ChatCompletionsStreamResponse) error {
	var lovableChunk lovableStreamChunk
	if err := common.UnmarshalJsonStr(data, &lovableChunk); err != nil {
		return fmt.Errorf("invalid Lovable chat stream response: %w", err)
	}

	for choiceIndex := range lovableChunk.Choices {
		if choiceIndex >= len(chunk.Choices) {
			break
		}
		for callIndex, toolCall := range lovableChunk.Choices[choiceIndex].Delta.ToolCalls {
			if callIndex >= len(chunk.Choices[choiceIndex].Delta.ToolCalls) {
				break
			}
			chatIndex := callIndex
			if toolCall.Index != nil {
				chatIndex = *toolCall.Index
			}
			isCustom, known := t.customByChatIndex[chatIndex]
			if strings.TrimSpace(common.Interface2String(toolCall.Type)) == dto.CustomType || toolCall.Custom != nil {
				isCustom = true
			}
			if !known {
				t.customByChatIndex[chatIndex] = isCustom
				t.pendingToolKinds = append(t.pendingToolKinds, isCustom)
			}
			if !isCustom {
				continue
			}

			standardToolCall := &chunk.Choices[choiceIndex].Delta.ToolCalls[callIndex]
			if toolCall.Custom != nil {
				if toolCall.Custom.Name != "" {
					standardToolCall.Function.Name = toolCall.Custom.Name
				}
				if toolCall.Custom.Input != "" {
					standardToolCall.Function.Arguments = toolCall.Custom.Input
				}
			}
			if toolCall.ID != "" {
				t.customCallIDs[toolCall.ID] = struct{}{}
			}
		}
	}
	return nil
}

func (t *lovableStreamToolTracker) marshalEvent(event relayconvert.ChatToResponsesStreamEvent) (string, []byte, bool, error) {
	eventType := event.Type
	outputIndex := -1
	if event.Payload.OutputIndex != nil {
		outputIndex = *event.Payload.OutputIndex
	}

	if eventType == lovableOutputItemAddedEvent && event.Payload.Item != nil && event.Payload.Item.Type == "function_call" {
		isCustom := false
		if len(t.pendingToolKinds) > 0 {
			isCustom = t.pendingToolKinds[0]
			t.pendingToolKinds = t.pendingToolKinds[1:]
		}
		if _, ok := t.customCallIDs[event.Payload.Item.CallId]; ok {
			isCustom = true
		}
		if isCustom {
			t.customOutputIndexes[outputIndex] = struct{}{}
			t.customCallIDs[event.Payload.Item.CallId] = struct{}{}
		}
	}

	_, customOutput := t.customOutputIndexes[outputIndex]
	if eventType == lovableFunctionArgumentsDoneEvent && customOutput {
		return "", nil, true, nil
	}
	if eventType == lovableFunctionArgumentsDeltaEvent && customOutput {
		eventType = lovableCustomToolInputDeltaEvent
	}

	data, err := common.Marshal(event.Payload)
	if err != nil {
		return "", nil, false, err
	}
	var payload map[string]any
	if err := common.Unmarshal(data, &payload); err != nil {
		return "", nil, false, err
	}
	payload["type"] = eventType

	if customOutput && (eventType == lovableOutputItemAddedEvent || eventType == lovableOutputItemDoneEvent) {
		if item, ok := payload["item"].(map[string]any); ok {
			convertLovableStreamToolItem(item)
		}
	}
	if eventType == lovableResponseCompletedEvent || eventType == lovableResponseIncompleteEvent {
		if response, ok := payload["response"].(map[string]any); ok {
			if outputs, ok := response["output"].([]any); ok {
				for index, rawOutput := range outputs {
					output, ok := rawOutput.(map[string]any)
					if !ok {
						continue
					}
					_, customIndex := t.customOutputIndexes[index]
					_, customCall := t.customCallIDs[strings.TrimSpace(common.Interface2String(output["call_id"]))]
					if customIndex || customCall {
						convertLovableStreamToolItem(output)
					}
				}
			}
		}
	}

	data, err = common.Marshal(payload)
	return eventType, data, false, err
}

func convertLovableStreamToolItem(item map[string]any) {
	item["type"] = lovableCustomToolCallType
	item["input"] = common.Interface2String(item["arguments"])
	delete(item, "arguments")
}
