package kitedelayed

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/require"
)

// codex_lite_request.json 是从真实 Codex CLI（Responses Lite 模式）抓下来的请求，
// 只截断了超长的工具描述。它固定了 additional_tools 的结构：
//
//	functions: exec(custom) / wait / request_user_input / request_user_input_async
//	clock:     sleep
//	collaboration: followup_task / interrupt_agent / list_agents / send_message / spawn_agent / wait_agent
const codexLiteToolCount = 11

func TestHoistCodexLiteToolsNormalizesCustomToolCallHistory(t *testing.T) {
	// Codex 把上一轮的 exec 调用与结果原样回填。custom_tool_call_output 在
	// relayconvert 里没有分支，不归一化就会变成空内容的普通消息，
	// assistant 的 tool_calls 无人应答 → 上游 502。
	execSource := `const r = await tools.exec_command({cmd: "pwd"}); text(r.output);`
	request := loadResponsesRequest(t, "codex_lite_request.json")
	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	items = append(items,
		map[string]any{
			"type":    "custom_tool_call",
			"call_id": "call_1",
			"name":    "exec",
			"input":   execSource,
		},
		map[string]any{
			"type":    "custom_tool_call_output",
			"call_id": "call_1",
			"output":  "/tmp",
		},
	)
	encoded, err := common.Marshal(items)
	require.NoError(t, err)
	request.Input = encoded

	customTools, err := hoistCodexLiteTools(&request)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"exec": true}, customTools)

	var rewritten []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &rewritten))
	require.Len(t, rewritten, 4)

	callItem := rewritten[2]
	require.Equal(t, "function_call", common.Interface2String(callItem["type"]))
	require.Equal(t, "exec", common.Interface2String(callItem["name"]))
	require.Equal(t, "call_1", common.Interface2String(callItem["call_id"]))
	require.JSONEq(t,
		`{"source":`+mustRawString(execSource)+`}`,
		common.Interface2String(callItem["arguments"]),
		"历史调用参数必须与降级后的 function schema 一致")

	outputItem := rewritten[3]
	require.Equal(t, "function_call_output", common.Interface2String(outputItem["type"]))
	require.Equal(t, "call_1", common.Interface2String(outputItem["call_id"]))
	require.Equal(t, "/tmp", common.Interface2String(outputItem["output"]))
}

func TestHoistCodexLiteToolsKeepsUnknownCustomToolCallHistory(t *testing.T) {
	// 没有被降级的 custom 工具（不在 customTools 里）保持原样，交给转换层按 custom 处理
	request := dto.OpenAIResponsesRequest{
		Model: "openai/gpt-6-astra",
		Input: mustRaw([]map[string]any{
			{"type": "custom_tool_call", "call_id": "call_9", "name": "other_custom", "input": "x"},
			{"type": "custom_tool_call_output", "call_id": "call_9", "output": "y"},
		}),
	}

	_, err := hoistCodexLiteTools(&request)
	require.NoError(t, err)

	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	require.Equal(t, "custom_tool_call", common.Interface2String(items[0]["type"]))
	require.Equal(t, "function_call_output", common.Interface2String(items[1]["type"]),
		"结果项与工具无关，一律归一化，否则会被转换层丢掉")
}

func mustRawString(value string) string {
	encoded, err := common.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// mustRaw 把任意值编成 json.RawMessage，仅用于构造测试数据。
func mustRaw(value any) json.RawMessage {
	encoded, err := common.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func loadResponsesRequest(t *testing.T, name string) dto.OpenAIResponsesRequest {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(raw, &request))
	return request
}

func toolNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var tools []map[string]any
	require.NoError(t, common.Unmarshal(raw, &tools))
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, common.Interface2String(tool["name"]))
	}
	return names
}

func TestHoistCodexLiteToolsPromotesAdditionalTools(t *testing.T) {
	request := loadResponsesRequest(t, "codex_lite_request.json")

	customTools, err := hoistCodexLiteTools(&request)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"exec": true}, customTools)

	names := toolNames(t, request.Tools)
	require.Len(t, names, codexLiteToolCount)
	require.Contains(t, names, "exec")
	require.Contains(t, names, "wait")
	require.Contains(t, names, "request_user_input")
	require.Contains(t, names, "request_user_input_async")
	require.Contains(t, names, "sleep")
	require.Contains(t, names, "spawn_agent")
	require.NotContains(t, names, "functions", "namespace 必须被展平")
	require.NotContains(t, names, "collaboration", "namespace 必须被展平")

	// exec 原本是 custom + lark grammar，chat 里表达不了，必须降级成单字符串参数的 function
	var execTool map[string]any
	var tools []map[string]any
	require.NoError(t, common.Unmarshal(request.Tools, &tools))
	for _, tool := range tools {
		if common.Interface2String(tool["name"]) == "exec" {
			execTool = tool
		}
	}
	require.NotNil(t, execTool)
	require.Equal(t, "function", common.Interface2String(execTool["type"]))
	parameters, ok := execTool["parameters"].(map[string]any)
	require.True(t, ok)
	properties, ok := parameters["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, properties, codexLiteSourceParam)
	require.Contains(t, common.Interface2String(execTool["description"]), "Run JavaScript")

	// additional_tools 项必须从 input 里摘掉，否则转换层会生成空内容的 developer 消息
	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	require.Len(t, items, 2)
	for _, item := range items {
		require.NotEqual(t, codexLiteTypeAdditionalTools, common.Interface2String(item["type"]))
	}
}

func TestHoistCodexLiteToolsLeavesPlainRequestUntouched(t *testing.T) {
	request := loadResponsesRequest(t, "plain_responses_request.json")
	originalTools := string(request.Tools)
	originalInput := string(request.Input)

	customTools, err := hoistCodexLiteTools(&request)
	require.NoError(t, err)
	require.Nil(t, customTools, "普通请求不应产生 custom 工具")
	require.Equal(t, originalTools, string(request.Tools), "普通请求的 tools 必须原样保留")
	require.Equal(t, originalInput, string(request.Input), "普通请求的 input 必须原样保留")
}

func TestHoistCodexLiteToolsFlattensTopLevelNamespace(t *testing.T) {
	// legacy 模式也会带顶层 namespace（如 multi_agent_v1），chat 表达不了，同样要展平
	request := dto.OpenAIResponsesRequest{
		Model: "openai/gpt-6-astra",
		Tools: mustRaw([]map[string]any{
			{"type": "function", "name": "exec_command", "description": "run", "parameters": map[string]any{"type": "object"}},
			{"type": "namespace", "name": "multi_agent_v1", "tools": []map[string]any{
				{"type": "function", "name": "spawn_agent", "description": "spawn", "parameters": map[string]any{"type": "object"}},
			}},
			{"type": "web_search"},
		}),
	}

	customTools, err := hoistCodexLiteTools(&request)
	require.NoError(t, err)
	require.Empty(t, customTools)
	names := toolNames(t, request.Tools)
	require.Equal(t, []string{"exec_command", "spawn_agent"}, names)
}

func TestCodexLiteSourceFromArguments(t *testing.T) {
	cases := map[string]string{
		`{"source":"console.log('hi');"}`:       "console.log('hi');",
		`{"input":"raw text"}`:                  "raw text",
		`console.log('no json wrapper');`:       "console.log('no json wrapper');",
		`{"source":"line1\nline2"}`:             "line1\nline2",
		`{"unknown":{"nested":true}}`:           `{"unknown":{"nested":true}}`,
		`{"source":{"unexpected":"structure"}}`: `{"unexpected":"structure"}`,
		``:                                      "",
	}
	for arguments, want := range cases {
		require.Equal(t, want, codexLiteSourceFromArguments(arguments), "arguments=%q", arguments)
	}
}

func TestApplyCodexLiteOutputRewritesExecToCustomToolCall(t *testing.T) {
	output := []dto.ResponsesOutput{
		{Type: "message", Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: "hi"}}},
		{
			Type:      "function_call",
			ID:        "call_1",
			CallId:    "call_1",
			Name:      "exec",
			Arguments: mustRaw(`{"source":"console.log(1);"}`),
		},
		{
			Type:      "function_call",
			ID:        "call_2",
			CallId:    "call_2",
			Name:      "wait",
			Arguments: mustRaw(`{"seconds":1}`),
		},
	}

	applyCodexLiteOutput(output, map[string]bool{"exec": true})

	require.Equal(t, "message", output[0].Type)
	require.Equal(t, codexLiteOutputCustomCall, output[1].Type)
	require.JSONEq(t, `"console.log(1);"`, string(output[1].Input))
	require.Nil(t, output[1].Arguments)
	require.Equal(t, "function_call", output[2].Type, "非 custom 工具保持 function_call")
	require.JSONEq(t, `{"seconds":1}`, output[2].ArgumentsString())
}

func TestCodexLiteStreamRewriterRewritesExecEvents(t *testing.T) {
	outputIndex := 0
	itemID := "call_1"
	execArguments := `{"source":"console.log('hi');"}`

	rewriter := newCodexLiteStreamRewriter(map[string]bool{"exec": true})

	var emitted []dto.ResponsesStreamResponse
	feed := func(event dto.ResponsesStreamResponse) {
		emitted = append(emitted, rewriter.rewrite(event)...)
	}

	feed(dto.ResponsesStreamResponse{Type: "response.created"})
	feed(dto.ResponsesStreamResponse{
		Type:        codexLiteEventOutputItemAdded,
		OutputIndex: &outputIndex,
		ItemID:      itemID,
		Item:        &dto.ResponsesOutput{Type: "function_call", ID: itemID, CallId: itemID, Name: "exec"},
	})
	feed(dto.ResponsesStreamResponse{
		Type:        codexLiteEventArgsDelta,
		OutputIndex: &outputIndex,
		ItemID:      itemID,
		Delta:       execArguments,
	})
	feed(dto.ResponsesStreamResponse{
		Type:        codexLiteEventArgsDone,
		OutputIndex: &outputIndex,
		ItemID:      itemID,
	})
	feed(dto.ResponsesStreamResponse{
		Type:        codexLiteEventOutputItemDone,
		OutputIndex: &outputIndex,
		ItemID:      itemID,
		Item: &dto.ResponsesOutput{
			Type:      "function_call",
			ID:        itemID,
			CallId:    itemID,
			Name:      "exec",
			Arguments: mustRaw(execArguments),
		},
	})
	feed(dto.ResponsesStreamResponse{
		Type: codexLiteEventCompleted,
		Response: &dto.OpenAIResponsesResponse{Output: []dto.ResponsesOutput{
			{
				Type:      "function_call",
				ID:        itemID,
				CallId:    itemID,
				Name:      "exec",
				Arguments: mustRaw(execArguments),
			},
		}},
	})

	types := make([]string, 0, len(emitted))
	for _, event := range emitted {
		types = append(types, event.Type)
	}
	require.Equal(t, []string{
		"response.created",
		codexLiteEventOutputItemAdded,
		codexLiteEventInputDelta,
		codexLiteEventInputDone,
		codexLiteEventOutputItemDone,
		codexLiteEventCompleted,
	}, types, "参数 delta 要被替换成一次性源码 delta，不能漏给客户端")

	require.Equal(t, codexLiteOutputCustomCall, emitted[1].Item.Type)
	require.Nil(t, emitted[1].Item.Arguments)
	require.Equal(t, "console.log('hi');", emitted[2].Delta)
	require.Equal(t, codexLiteOutputCustomCall, emitted[4].Item.Type)
	require.JSONEq(t, `"console.log('hi');"`, string(emitted[4].Item.Input))
	require.Equal(t, codexLiteOutputCustomCall, emitted[5].Response.Output[0].Type)
	require.JSONEq(t, `"console.log('hi');"`, string(emitted[5].Response.Output[0].Input))
}

func TestCodexLiteStreamRewriterPassesThroughOtherTools(t *testing.T) {
	outputIndex := 0
	rewriter := newCodexLiteStreamRewriter(map[string]bool{"exec": true})
	event := dto.ResponsesStreamResponse{
		Type:        codexLiteEventOutputItemAdded,
		OutputIndex: &outputIndex,
		ItemID:      "call_2",
		Item:        &dto.ResponsesOutput{Type: "function_call", ID: "call_2", Name: "wait"},
	}

	rewritten := rewriter.rewrite(event)
	require.Len(t, rewritten, 1)
	require.Equal(t, "function_call", rewritten[0].Item.Type, "非 custom 工具不应被改写")
}

func TestCodexLiteStreamRewriterIsNoopWithoutCustomTools(t *testing.T) {
	require.Nil(t, newCodexLiteStreamRewriter(nil))
	require.Nil(t, newCodexLiteStreamRewriter(map[string]bool{}))

	var rewriter *codexLiteStreamRewriter
	event := dto.ResponsesStreamResponse{Type: "response.created"}
	require.Len(t, rewriter.rewrite(event), 1)
}
