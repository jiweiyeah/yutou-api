package kitedelayed

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"

	"github.com/gin-gonic/gin"
)

// Codex 对带 use_responses_lite 元数据的模型会切到 "Responses Lite" 协议：
// 工具不再放在顶层 tools，而是塞进 input 里一条 {"type":"additional_tools"} 项；
// 其中 functions.exec 是 type=custom + lark grammar —— 模型输出的是**原始 JS 源码**，
// 不是 JSON；exec_command / apply_patch / view_image 这些真正的能力都活在 JS 沙箱里
// （`await tools.exec_command(...)`），不作为 API 工具暴露。
//
// 上游 Kite Router 只提供 chat completions，必须做 responses -> chat 转换，而转换层
// 只认顶层 tools、且只会产出 function_call，于是：
//  1. additional_tools 项落到默认分支被当成一条空内容的 developer 消息，工具整段丢失；
//  2. custom 类型工具在 chat 协议里无法表达。
//
// 结果就是模型手里零工具，表现为「能聊天、但拒绝执行任何操作」。
//
// 这里补一层桥，**只在 Kite Router 线生效**（其他渠道走原生逻辑，完全不受影响）：
//   - 请求侧：把 additional_tools 提升为顶层 tools、展平 namespace、把 custom 工具
//     降级成单字符串参数的 function 工具（参数名 source，承载原始源码）；
//   - 响应侧：把 exec 的 function_call 还原成 Codex 期望的 custom_tool_call
//     （input 为源码原文）。
//
// 没有 additional_tools 的普通请求会在 hoistCodexLiteTools 里原样返回，零改动。
const (
	codexLiteCustomToolsContextKey = "kite_codex_lite_custom_tools"

	codexLiteTypeAdditionalTools = "additional_tools"
	codexLiteTypeNamespace       = "namespace"
	codexLiteTypeCustom          = "custom"
	codexLiteTypeFunction        = "function"

	// Codex 回填历史时的输入项类型。custom_tool_call_output 在 relayconvert 里
	// 没有对应分支，会被当成空内容的普通消息丢掉，导致 assistant 的 tool_calls
	// 没有应答，上游直接回 502 —— 这里归一化成 chat 侧认得的形态。
	codexLiteInputCustomToolCall   = "custom_tool_call"
	codexLiteInputCustomToolOutput = "custom_tool_call_output"
	codexLiteInputFunctionCall     = "function_call"
	codexLiteInputFunctionOutput   = "function_call_output"

	codexLiteSourceParam = "source"

	codexLiteEventOutputItemAdded = "response.output_item.added"
	codexLiteEventOutputItemDone  = "response.output_item.done"
	codexLiteEventArgsDelta       = "response.function_call_arguments.delta"
	codexLiteEventArgsDone        = "response.function_call_arguments.done"
	codexLiteEventInputDelta      = "response.custom_tool_call_input.delta"
	codexLiteEventInputDone       = "response.custom_tool_call_input.done"
	codexLiteEventCompleted       = "response.completed"

	codexLiteOutputFunctionCall = "function_call"
	codexLiteOutputCustomCall   = "custom_tool_call"
)

// hoistCodexLiteTools 把 Responses Lite 的 additional_tools 项提升为顶层 tools，
// 归一化历史里的 custom_tool_call/output 项，并返回「需要按 custom_tool_call
// 回给客户端」的工具名集合。返回空集合表示这不是一个 Lite 请求，调用方保持原行为。
func hoistCodexLiteTools(req *dto.OpenAIResponsesRequest) (map[string]bool, error) {
	if req == nil {
		return nil, nil
	}

	items, hoisted, removedAdditionalTools, err := splitCodexLiteInput(req.Input)
	if err != nil {
		return nil, err
	}

	existing, err := decodeCodexLiteTools(req.Tools)
	if err != nil {
		return nil, err
	}

	hasCustomToolCall := containsCodexLiteInputType(items, codexLiteInputCustomToolCall)
	if len(hoisted) == 0 && !containsCodexLiteNamespace(existing) && !hasCustomToolCall {
		return nil, nil
	}

	customTools := make(map[string]bool)
	merged := make([]map[string]any, 0, len(existing)+len(hoisted))
	merged = append(merged, flattenCodexLiteTools(existing, customTools)...)
	merged = append(merged, flattenCodexLiteTools(hoisted, customTools)...)
	if len(merged) > 0 {
		encoded, err := common.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("encode responses tools failed: %w", err)
		}
		req.Tools = encoded
	}

	if removedAdditionalTools || hasCustomToolCall {
		encoded, err := common.Marshal(rewriteCodexLiteInputItems(items, customTools))
		if err != nil {
			return nil, fmt.Errorf("encode responses input failed: %w", err)
		}
		req.Input = encoded
	}
	return customTools, nil
}

// splitCodexLiteInput 摘出 input 里 additional_tools 项携带的工具并移除这些项，
// 返回剩余输入项与「是否真的移除过」。
func splitCodexLiteInput(raw json.RawMessage) ([]map[string]any, []map[string]any, bool, error) {
	if len(raw) == 0 || common.GetJsonType(raw) != "array" {
		return nil, nil, false, nil
	}
	var items []map[string]any
	if err := common.Unmarshal(raw, &items); err != nil {
		return nil, nil, false, fmt.Errorf("decode responses input failed: %w", err)
	}

	hoisted := make([]map[string]any, 0)
	kept := make([]map[string]any, 0, len(items))
	removed := false
	for _, item := range items {
		if strings.TrimSpace(common.Interface2String(item["type"])) != codexLiteTypeAdditionalTools {
			kept = append(kept, item)
			continue
		}
		removed = true
		nested, ok := item["tools"].([]any)
		if !ok {
			continue
		}
		for _, raw := range nested {
			if tool, ok := raw.(map[string]any); ok {
				hoisted = append(hoisted, tool)
			}
		}
	}
	return kept, hoisted, removed, nil
}

// rewriteCodexLiteInputItems 把 Codex 历史里的 custom 工具项归一化成 chat 侧认得的形态：
//   - custom_tool_call（我们已把该工具降级成 function）→ function_call，参数包成
//     {"source": "<原始源码>"}，与降级后的工具 schema 一致；
//   - custom_tool_call_output → function_call_output，否则转换层会把它当普通消息丢掉，
//     assistant 的 tool_calls 就没人应答，上游会直接 502。
func rewriteCodexLiteInputItems(items []map[string]any, customTools map[string]bool) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch strings.TrimSpace(common.Interface2String(item["type"])) {
		case codexLiteInputCustomToolCall:
			name := strings.TrimSpace(common.Interface2String(item["name"]))
			if !customTools[name] {
				out = append(out, item)
				continue
			}
			out = append(out, map[string]any{
				"type":      codexLiteInputFunctionCall,
				"name":      name,
				"call_id":   common.Interface2String(item["call_id"]),
				"arguments": codexLiteArgumentsFromSource(common.Interface2String(item["input"])),
			})
		case codexLiteInputCustomToolOutput:
			out = append(out, map[string]any{
				"type":    codexLiteInputFunctionOutput,
				"call_id": common.Interface2String(item["call_id"]),
				"output":  item["output"],
			})
		default:
			out = append(out, item)
		}
	}
	return out
}

func containsCodexLiteInputType(items []map[string]any, itemType string) bool {
	for _, item := range items {
		if strings.TrimSpace(common.Interface2String(item["type"])) == itemType {
			return true
		}
	}
	return false
}

// codexLiteArgumentsFromSource 把原始源码包成降级后 function 工具期望的 JSON 参数文本。
func codexLiteArgumentsFromSource(source string) string {
	encoded, err := common.Marshal(map[string]string{codexLiteSourceParam: source})
	if err != nil {
		return source
	}
	return string(encoded)
}

func decodeCodexLiteTools(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || common.GetJsonType(raw) != "array" {
		return nil, nil
	}
	var tools []map[string]any
	if err := common.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode responses tools failed: %w", err)
	}
	return tools, nil
}

func containsCodexLiteNamespace(tools []map[string]any) bool {
	for _, tool := range tools {
		if strings.TrimSpace(common.Interface2String(tool["type"])) == codexLiteTypeNamespace {
			return true
		}
	}
	return false
}

// flattenCodexLiteTools 把 namespace 摊平成其内部的工具，并把 chat 协议表达不了的
// custom 工具降级为 function 工具。web_search / local_shell 之类在 chat 里没有对应
// 形态的条目直接丢弃 —— 之前它们会被发成 {"type":"web_search","custom":{...}} 这种
// 非法工具，上游一律忽略，丢掉语义等价但更干净。
func flattenCodexLiteTools(tools []map[string]any, customTools map[string]bool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		switch strings.TrimSpace(common.Interface2String(tool["type"])) {
		case codexLiteTypeNamespace:
			nested, ok := tool["tools"].([]any)
			if !ok {
				continue
			}
			children := make([]map[string]any, 0, len(nested))
			for _, raw := range nested {
				if child, ok := raw.(map[string]any); ok {
					children = append(children, child)
				}
			}
			out = append(out, flattenCodexLiteTools(children, customTools)...)
		case codexLiteTypeFunction:
			out = append(out, tool)
		case codexLiteTypeCustom:
			name := strings.TrimSpace(common.Interface2String(tool["name"]))
			if name == "" {
				continue
			}
			customTools[name] = true
			out = append(out, codexLiteCustomToolToFunction(tool, name))
		}
	}
	return out
}

// codexLiteExecGuidance 会被前置到降级后的 custom 工具描述里。
// custom 工具原本是 lark grammar 约束的「纯源码」输出，降级成 function 后模型倾向于
// 自创 API（实测出现过 tools.exec / Deno.Command），这里显式点明只有 tools.* 存在。
const codexLiteExecGuidance = "IMPORTANT: this tool takes raw source text, not JSON and not a quoted string. " +
	"The source must drive the nested tools through the global `tools` object, for example: " +
	"`const r = await tools.exec_command({cmd: \"pwd\"}); text(r.output);`. " +
	"Only the `tools.*` methods listed below and the documented global helpers exist — " +
	"there is no Node, Deno, require, import, fetch or console."

// codexLiteCustomToolToFunction 把 custom(grammar) 工具降级成 function 工具：
// 上游只会回 JSON 参数，所以用单个字符串字段承载原始源码，
// 响应侧再由 codexLiteSourceFromArguments 还原回 custom_tool_call 的 input。
func codexLiteCustomToolToFunction(tool map[string]any, name string) map[string]any {
	description := common.Interface2String(tool["description"])
	if strings.TrimSpace(description) == "" {
		description = fmt.Sprintf("Freeform tool %s.", name)
	}
	return map[string]any{
		"type":        codexLiteTypeFunction,
		"name":        name,
		"description": codexLiteExecGuidance + "\n\n" + description,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				codexLiteSourceParam: map[string]any{
					"type":        "string",
					"description": "The tool input as raw source text, passed verbatim without JSON quoting or markdown fences. Example: const r = await tools.exec_command({cmd: \"ls\"}); text(r.output);",
				},
			},
			"required": []string{codexLiteSourceParam},
		},
	}
}

// codexLiteSourceFromArguments 从 chat 侧的 JSON 参数里取出原始源码。
// 解析失败（模型直接给了源码）或找不到约定字段时按原文返回，尽量不丢内容。
func codexLiteSourceFromArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var payload map[string]any
	if err := common.UnmarshalJsonStr(trimmed, &payload); err != nil {
		return trimmed
	}
	for _, key := range []string{codexLiteSourceParam, "input", "code", "script", "text", "source_code"} {
		value, ok := payload[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			return text
		}
		if encoded, err := common.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return trimmed
}

// applyCodexLiteOutput 把非流式响应里 exec 这类 custom 工具的 function_call 项
// 还原成 custom_tool_call（input 为源码原文，arguments 清空）。
func applyCodexLiteOutput(output []dto.ResponsesOutput, customTools map[string]bool) {
	if len(customTools) == 0 {
		return
	}
	for i := range output {
		item := &output[i]
		if item.Type != codexLiteOutputFunctionCall || !customTools[item.Name] {
			continue
		}
		source := codexLiteSourceFromArguments(item.ArgumentsString())
		item.Type = codexLiteOutputCustomCall
		item.Arguments = nil
		if encoded, err := common.Marshal(source); err == nil {
			item.Input = encoded
		}
	}
}

// codexLiteStreamRewriter 把流式事件里 custom 工具的 function_call 改写成
// custom_tool_call：added/done 换 item 类型，参数 delta 攒起来在 done 时
// 一次性以源码原文发给客户端（JSON 参数被逐字符切碎后无法直接还原成源码）。
type codexLiteStreamRewriter struct {
	customTools  map[string]bool
	customItems  map[string]bool
	pendingDelta map[string]string
}

func newCodexLiteStreamRewriter(customTools map[string]bool) *codexLiteStreamRewriter {
	if len(customTools) == 0 {
		return nil
	}
	return &codexLiteStreamRewriter{
		customTools:  customTools,
		customItems:  make(map[string]bool),
		pendingDelta: make(map[string]string),
	}
}

func (r *codexLiteStreamRewriter) rewrite(event dto.ResponsesStreamResponse) []dto.ResponsesStreamResponse {
	if r == nil {
		return []dto.ResponsesStreamResponse{event}
	}

	switch event.Type {
	case codexLiteEventOutputItemAdded, codexLiteEventOutputItemDone:
		if !r.isCustomItem(event.Item) {
			return []dto.ResponsesStreamResponse{event}
		}
		itemID := codexLiteItemID(event.Item)
		r.customItems[itemID] = true
		source := codexLiteSourceFromArguments(event.Item.ArgumentsString())
		if source == "" {
			source = r.takePendingDelta(itemID)
		}
		event.Item.Type = codexLiteOutputCustomCall
		event.Item.Arguments = nil
		if encoded, err := common.Marshal(source); err == nil {
			event.Item.Input = encoded
		}
		return []dto.ResponsesStreamResponse{event}

	case codexLiteEventArgsDelta:
		if !r.customItems[codexLiteEventItemID(event)] {
			return []dto.ResponsesStreamResponse{event}
		}
		itemID := codexLiteEventItemID(event)
		r.pendingDelta[itemID] += event.Delta
		return nil

	case codexLiteEventArgsDone:
		itemID := codexLiteEventItemID(event)
		if !r.customItems[itemID] {
			return []dto.ResponsesStreamResponse{event}
		}
		source := codexLiteSourceFromArguments(r.takePendingDelta(itemID))
		if source == "" {
			return nil
		}
		delta := event
		delta.Type = codexLiteEventInputDelta
		delta.Delta = source
		done := event
		done.Type = codexLiteEventInputDone
		done.Delta = ""
		return []dto.ResponsesStreamResponse{delta, done}

	case codexLiteEventCompleted:
		if event.Response != nil {
			applyCodexLiteOutput(event.Response.Output, r.customTools)
		}
		return []dto.ResponsesStreamResponse{event}
	}

	return []dto.ResponsesStreamResponse{event}
}

func (r *codexLiteStreamRewriter) isCustomItem(item *dto.ResponsesOutput) bool {
	if item == nil || item.Type != codexLiteOutputFunctionCall {
		return false
	}
	return r.customTools[item.Name]
}

func (r *codexLiteStreamRewriter) takePendingDelta(itemID string) string {
	if itemID == "" {
		return ""
	}
	buffered := r.pendingDelta[itemID]
	delete(r.pendingDelta, itemID)
	return buffered
}

func codexLiteItemID(item *dto.ResponsesOutput) string {
	if item == nil {
		return ""
	}
	if id := strings.TrimSpace(item.ID); id != "" {
		return id
	}
	return strings.TrimSpace(item.CallId)
}

func codexLiteEventItemID(event dto.ResponsesStreamResponse) string {
	return strings.TrimSpace(event.ItemID)
}

// codexLiteCustomTools 取出请求侧存下的 custom 工具名集合。
func codexLiteCustomTools(c *gin.Context) map[string]bool {
	if c == nil {
		return nil
	}
	value, exists := c.Get(codexLiteCustomToolsContextKey)
	if !exists {
		return nil
	}
	customTools, _ := value.(map[string]bool)
	return customTools
}
