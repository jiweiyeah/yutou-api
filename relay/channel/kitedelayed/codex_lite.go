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
//  2. custom 类型工具在 chat 协议里无法表达；
//  3. custom_tool_call_output 输入项没有分支，被降级成空内容的普通消息，
//     assistant 的 tool_calls 无人应答，上游直接回 502。
//
// 结果就是模型手里零工具，表现为「能聊天、但拒绝执行任何操作」。
//
// 这里补一层桥，**只在 Kite Router 线生效**（其他渠道走原生逻辑，完全不受影响）：
//   - 请求侧：把 additional_tools / tool_search_output 里的工具提升为顶层 tools、
//     展平 namespace、把 custom 工具降级成单字符串参数的 function 工具（参数名 source，
//     承载原始源码），并把历史里的 custom_tool_call / custom_tool_call_output
//     归一化成 chat 认得的形态；
//   - 响应侧：把 exec 的 function_call 还原成 Codex 期望的 custom_tool_call
//     （input 为源码原文），并给展平过的工具调用补回 namespace 字段。
//
// 设计对照过 farion1231/cc-switch 的 transform_codex_chat.rs / streaming_codex_chat.rs
// （同为 Responses->Chat 桥接，5400+ 行、带完整测试）。一致的部分：custom 工具降级成
// 单字段 function、响应侧按同一字段还原、custom_tool_call_output 必须转成 tool 消息、
// 流式下抑制 function_call_arguments.* 改为一次性 custom_tool_call_input.delta/done。
// 有意保留的差异见 codexLiteToolCollector.add 的注释。
//
// 没有 additional_tools 的普通请求会在 hoistCodexLiteTools 里原样返回，零改动。
const (
	codexLiteContextKey = "kite_codex_lite_tools"

	codexLiteTypeAdditionalTools  = "additional_tools"
	codexLiteTypeToolSearchOutput = "tool_search_output"
	codexLiteTypeToolSearch       = "tool_search"
	codexLiteTypeNamespace        = "namespace"
	codexLiteTypeCustom           = "custom"
	codexLiteTypeFunction         = "function"

	// Codex 回填历史时的输入项类型。
	codexLiteInputCustomToolCall   = "custom_tool_call"
	codexLiteInputCustomToolOutput = "custom_tool_call_output"
	codexLiteInputToolSearchCall   = "tool_search_call"
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

	codexLiteStatusInProgress = "in_progress"
	codexLiteStatusCompleted  = "completed"

	codexLiteOutputFunctionCall   = "function_call"
	codexLiteOutputCustomCall     = "custom_tool_call"
	codexLiteOutputToolSearchCall = "tool_search_call"

	// codexLiteToolSearchProxyName 是 Codex 延迟加载 MCP/插件工具时的搜索入口。
	// 模型调它 → 客户端在自己的工具注册表里搜 → 用 tool_search_output 回填找到的工具。
	codexLiteToolSearchProxyName = "tool_search"
)

// codexLiteToolKind 区分降级后的 chat 工具原本是什么形态，响应侧据此还原。
type codexLiteToolKind int

const (
	codexLiteKindFunction codexLiteToolKind = iota
	codexLiteKindCustom
	codexLiteKindToolSearch
)

// codexLiteToolSpec 记录一个 chat 工具对应的原始 Codex 工具。
// chat 名沿用工具的裸名（见 codexLiteToolCollector.add 的说明），
// Namespace 用于在响应里还原 Codex 私有扩展的 namespace 字段。
type codexLiteToolSpec struct {
	Name      string
	Namespace string
	Kind      codexLiteToolKind
}

// codexLiteToolCollector 边展平边记录 chat 名到原始工具的映射。
type codexLiteToolCollector struct {
	chatTools []map[string]any
	specs     map[string]codexLiteToolSpec
}

func newCodexLiteToolCollector() *codexLiteToolCollector {
	return &codexLiteToolCollector{specs: make(map[string]codexLiteToolSpec)}
}

// hoistCodexLiteTools 把 Responses Lite 的 additional_tools 项提升为顶层 tools，
// 归一化历史里的 custom_tool_call/output 项，并返回 chat 名到原始工具的映射。
// 返回空 map 表示这不是一个 Lite 请求，调用方保持原行为。
func hoistCodexLiteTools(req *dto.OpenAIResponsesRequest) (map[string]codexLiteToolSpec, error) {
	if req == nil {
		return nil, nil
	}

	items, hoisted, removedDeclaredTools, err := splitCodexLiteInput(req.Input)
	if err != nil {
		return nil, err
	}

	existing, err := decodeCodexLiteTools(req.Tools)
	if err != nil {
		return nil, err
	}

	// custom_tool_call_output 单独出现时同样要归一化（否则会被转换层丢掉），
	// 所以两个类型都算「需要重写 input」。
	needsInputRewrite := containsCodexLiteInputType(items, codexLiteInputCustomToolCall) ||
		containsCodexLiteInputType(items, codexLiteInputCustomToolOutput) ||
		containsCodexLiteInputType(items, codexLiteInputToolSearchCall) ||
		containsCodexLiteInputType(items, codexLiteTypeToolSearchOutput)
	if len(hoisted) == 0 && !containsCodexLiteNamespace(existing) &&
		!containsCodexLiteToolSearch(existing) && !needsInputRewrite {
		return nil, nil
	}

	collector := newCodexLiteToolCollector()
	collector.addAll(existing, "")
	collector.addAll(hoisted, "")
	if len(collector.chatTools) > 0 {
		encoded, err := common.Marshal(collector.chatTools)
		if err != nil {
			return nil, fmt.Errorf("encode responses tools failed: %w", err)
		}
		req.Tools = encoded
	}

	if removedDeclaredTools || needsInputRewrite {
		encoded, err := common.Marshal(rewriteCodexLiteInputItems(items, collector.specs))
		if err != nil {
			return nil, fmt.Errorf("encode responses input failed: %w", err)
		}
		req.Input = encoded
	}
	return collector.specs, nil
}

// splitCodexLiteInput 摘出 input 里携带工具声明的项（additional_tools /
// tool_search_output）并移除这些项，返回剩余输入项与「是否真的移除过」。
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
		itemType := strings.TrimSpace(common.Interface2String(item["type"]))
		if itemType != codexLiteTypeAdditionalTools && itemType != codexLiteTypeToolSearchOutput {
			kept = append(kept, item)
			continue
		}
		removed = true
		// additional_tools 只是工具载体（有 role 无 content），保留会变成空消息，必须摘掉；
		// tool_search_output 既是工具载体、又是上一轮 tool_search 的结果，要留下当 tool 消息。
		if itemType == codexLiteTypeToolSearchOutput {
			kept = append(kept, item)
		}
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
func rewriteCodexLiteInputItems(items []map[string]any, specs map[string]codexLiteToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch strings.TrimSpace(common.Interface2String(item["type"])) {
		case codexLiteInputCustomToolCall:
			name := strings.TrimSpace(common.Interface2String(item["name"]))
			spec, ok := lookupCodexLiteSpec(specs, name, common.Interface2String(item["namespace"]))
			if !ok || spec.Kind != codexLiteKindCustom {
				out = append(out, item)
				continue
			}
			out = append(out, map[string]any{
				"type":      codexLiteInputFunctionCall,
				"name":      spec.Name,
				"call_id":   common.Interface2String(item["call_id"]),
				"arguments": codexLiteArgumentsFromSource(common.Interface2String(item["input"])),
			})
		case codexLiteInputToolSearchCall:
			out = append(out, map[string]any{
				"type":      codexLiteInputFunctionCall,
				"name":      codexLiteToolSearchProxyName,
				"call_id":   common.Interface2String(item["call_id"]),
				"arguments": codexLiteArgumentsText(item["arguments"]),
			})
		case codexLiteTypeToolSearchOutput:
			// 工具已在上一步提升为顶层 tools，这里把搜索结果本身作为 tool 消息回填，
			// 保持 assistant 的 tool_search 调用有对应应答（与 cc-switch 一致）。
			out = append(out, map[string]any{
				"type":    codexLiteInputFunctionOutput,
				"call_id": common.Interface2String(item["call_id"]),
				"output":  item,
			})
		case codexLiteInputCustomToolOutput:
			// 结果项与工具形态无关，一律归一化；缺失 output 时保留整项，避免丢内容。
			output := item["output"]
			if output == nil {
				output = item
			}
			out = append(out, map[string]any{
				"type":    codexLiteInputFunctionOutput,
				"call_id": common.Interface2String(item["call_id"]),
				"output":  output,
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

func containsCodexLiteToolSearch(tools []map[string]any) bool {
	for _, tool := range tools {
		if strings.TrimSpace(common.Interface2String(tool["type"])) == codexLiteTypeToolSearch {
			return true
		}
	}
	return false
}

func containsCodexLiteNamespace(tools []map[string]any) bool {
	for _, tool := range tools {
		if strings.TrimSpace(common.Interface2String(tool["type"])) == codexLiteTypeNamespace {
			return true
		}
	}
	return false
}

func (c *codexLiteToolCollector) addAll(tools []map[string]any, namespace string) {
	for _, tool := range tools {
		c.add(tool, namespace)
	}
}

// add 把一个 Codex 工具转成 chat 能表达的形态。
//
// namespace 展平后**沿用工具的裸名**，而不是 cc-switch 的 `<namespace>__<name>`：
// 实测 Codex 客户端按裸名也能匹配到工具（Lite 的 exec 走通全闭环），保持裸名可以不改变
// 模型看到的工具名，且当前 Codex 工具集内不存在同名冲突。namespace 会在响应侧通过
// codexLiteToolSpec.Namespace 补回，供客户端按命名空间匹配。
// 若将来出现同名冲突，改成 <namespace>__<name> 即可（响应侧需同步改回裸名）。
func (c *codexLiteToolCollector) add(tool map[string]any, namespace string) {
	switch strings.TrimSpace(common.Interface2String(tool["type"])) {
	case codexLiteTypeNamespace:
		nested, ok := tool["tools"].([]any)
		if !ok {
			return
		}
		childNamespace := strings.TrimSpace(common.Interface2String(tool["name"]))
		children := make([]map[string]any, 0, len(nested))
		for _, raw := range nested {
			if child, ok := raw.(map[string]any); ok {
				children = append(children, child)
			}
		}
		c.addAll(children, childNamespace)
	case codexLiteTypeFunction:
		name := strings.TrimSpace(common.Interface2String(tool["name"]))
		if name == "" {
			return
		}
		c.chatTools = append(c.chatTools, normalizeCodexLiteFunctionTool(tool, name))
		c.specs[name] = codexLiteToolSpec{Name: name, Namespace: namespace, Kind: codexLiteKindFunction}
	case codexLiteTypeCustom:
		name := strings.TrimSpace(common.Interface2String(tool["name"]))
		if name == "" {
			return
		}
		c.chatTools = append(c.chatTools, codexLiteCustomToolToFunction(tool, name))
		c.specs[name] = codexLiteToolSpec{Name: name, Namespace: namespace, Kind: codexLiteKindCustom}
	case codexLiteTypeToolSearch:
		// Codex 把 MCP/插件工具延迟到 tool_search 之后（由模型元数据 supports_search_tool 决定）。
		// 不声明这个入口，模型就没法加载被延迟的工具 —— 之前是静默丢能力。
		if _, exists := c.specs[codexLiteToolSearchProxyName]; exists {
			return
		}
		c.chatTools = append(c.chatTools, codexLiteToolSearchProxyTool())
		c.specs[codexLiteToolSearchProxyName] = codexLiteToolSpec{
			Name: codexLiteToolSearchProxyName,
			Kind: codexLiteKindToolSearch,
		}
	}
	// web_search / local_shell / tool_search 等 chat 表达不了的条目直接丢弃 ——
	// 之前它们会被发成 {"type":"web_search","custom":{...}} 这种非法工具，上游一律忽略。
	// 已知缺口：Codex 的 tool_search（动态加载工具）未实现，声明该工具时模型将无法搜索工具。
}

// normalizeCodexLiteFunctionTool 保证 function 工具的 parameters 是合法 JSON Schema：
// 严格的上游会因 parameters 为 null 直接 400（"expected object, received null"）。
func normalizeCodexLiteFunctionTool(tool map[string]any, name string) map[string]any {
	normalized := map[string]any{
		"type":       codexLiteTypeFunction,
		"name":       name,
		"parameters": normalizeCodexLiteParameters(tool["parameters"]),
	}
	if description, ok := tool["description"].(string); ok && strings.TrimSpace(description) != "" {
		normalized["description"] = description
	}
	return normalized
}

func normalizeCodexLiteParameters(parameters any) map[string]any {
	normalized, ok := parameters.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if common.Interface2String(normalized["type"]) != "object" {
		normalized["type"] = "object"
	}
	return normalized
}

// codexLiteToolSearchProxyTool 是 tool_search 的 chat 侧声明（与 cc-switch 一致）。
func codexLiteToolSearchProxyTool() map[string]any {
	return map[string]any{
		"type":        codexLiteTypeFunction,
		"name":        codexLiteToolSearchProxyName,
		"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query for tools or connectors to load.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of tool groups to return.",
				},
			},
			"required": []string{"query"},
		},
	}
}

// codexLiteArgumentsText 把历史项里的 arguments 归一化成 chat 侧的 JSON 文本。
func codexLiteArgumentsText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		encoded, err := common.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// codexLiteToolSearchArguments 把 chat 侧的 arguments 文本还原成 tool_search_call
// 期望的**对象**（cc-switch 的 parse_tool_arguments_object 同款兜底）。
func codexLiteToolSearchArguments(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed != "" {
		var parsed any
		if err := common.UnmarshalJsonStr(trimmed, &parsed); err == nil {
			if _, ok := parsed.(map[string]any); ok {
				if encoded, err := common.Marshal(parsed); err == nil {
					return encoded
				}
			}
		}
		return codexLiteMustJSON(map[string]any{"query": trimmed})
	}
	return codexLiteMustJSON(map[string]any{})
}

func codexLiteMustJSON(value any) json.RawMessage {
	encoded, err := common.Marshal(value)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
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
// 原始工具定义（含全部嵌套工具的 TS 声明）必须原样保留 —— 它是模型写 JS 的唯一依据。
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

func lookupCodexLiteSpec(specs map[string]codexLiteToolSpec, name, namespace string) (codexLiteToolSpec, bool) {
	if len(specs) == 0 {
		return codexLiteToolSpec{}, false
	}
	if namespace = strings.TrimSpace(namespace); namespace != "" {
		if spec, ok := specs[namespace+"__"+name]; ok {
			return spec, true
		}
	}
	spec, ok := specs[strings.TrimSpace(name)]
	return spec, ok
}

// applyCodexLiteOutput 把非流式响应里的工具调用还原成 Codex 期望的形态：
// custom 工具 → custom_tool_call（input 为源码原文、arguments 清空）；
// 展平过的 function 工具 → 补回 namespace 字段，供客户端按命名空间匹配。
func applyCodexLiteOutput(output []dto.ResponsesOutput, specs map[string]codexLiteToolSpec) {
	if len(specs) == 0 {
		return
	}
	for i := range output {
		restoreCodexLiteToolItem(&output[i], specs, codexLiteStatusCompleted)
	}
}

// restoreCodexLiteToolItem 按原始工具形态把一个 chat 工具调用还原成 Codex 期望的项：
// custom → custom_tool_call（input 为源码原文）；tool_search → tool_search_call；
// 展平过的 function → 补回 namespace 字段。
func restoreCodexLiteToolItem(item *dto.ResponsesOutput, specs map[string]codexLiteToolSpec, status string) {
	if item == nil || item.Type != codexLiteOutputFunctionCall {
		return
	}
	spec, ok := lookupCodexLiteSpec(specs, item.Name, "")
	if !ok {
		return
	}
	switch spec.Kind {
	case codexLiteKindToolSearch:
		item.Type = codexLiteOutputToolSearchCall
		item.Execution = "client"
		item.Status = status
		item.Arguments = codexLiteToolSearchArguments(item.ArgumentsString())
	case codexLiteKindCustom:
		source := codexLiteSourceFromArguments(item.ArgumentsString())
		item.Type = codexLiteOutputCustomCall
		item.Arguments = nil
		if encoded, err := common.Marshal(source); err == nil {
			item.Input = encoded
		}
	default:
		item.Namespace = spec.Namespace
	}
}

// codexLiteStreamRewriter 把流式事件里 custom 工具的 function_call 改写成
// custom_tool_call：added/done 换 item 类型，参数 delta 攒起来在 done 时
// 一次性以源码原文发给客户端（JSON 参数被逐字符切碎后无法直接还原成源码）。
// 与 cc-switch 的 streaming_codex_chat.rs 一致：custom 工具不再发
// function_call_arguments.*，改为 custom_tool_call_input.delta / .done。
type codexLiteStreamRewriter struct {
	specs        map[string]codexLiteToolSpec
	customItems  map[string]bool
	pendingDelta map[string]string
}

func newCodexLiteStreamRewriter(specs map[string]codexLiteToolSpec) *codexLiteStreamRewriter {
	if len(specs) == 0 {
		return nil
	}
	return &codexLiteStreamRewriter{
		specs:        specs,
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
		if !r.isCodexLiteToolItem(event.Item) {
			return []dto.ResponsesStreamResponse{event}
		}
		spec, _ := lookupCodexLiteSpec(r.specs, event.Item.Name, "")
		if spec.Kind != codexLiteKindCustom {
			status := codexLiteStatusCompleted
			if event.Type == codexLiteEventOutputItemAdded {
				status = codexLiteStatusInProgress
			}
			restoreCodexLiteToolItem(event.Item, r.specs, status)
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
		itemID := codexLiteEventItemID(event)
		if !r.customItems[itemID] {
			return []dto.ResponsesStreamResponse{event}
		}
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
		// .done 事件按官方形态带最终 input，客户端两种累加方式都能取到内容。
		done.Input = source
		return []dto.ResponsesStreamResponse{delta, done}

	case codexLiteEventCompleted:
		if event.Response != nil {
			applyCodexLiteOutput(event.Response.Output, r.specs)
		}
		return []dto.ResponsesStreamResponse{event}
	}

	return []dto.ResponsesStreamResponse{event}
}

func (r *codexLiteStreamRewriter) isCodexLiteToolItem(item *dto.ResponsesOutput) bool {
	if item == nil || item.Type != codexLiteOutputFunctionCall {
		return false
	}
	_, ok := lookupCodexLiteSpec(r.specs, item.Name, "")
	return ok
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

// codexLiteTools 取出请求侧存下的 chat 名 → 原始工具映射。
func codexLiteTools(c *gin.Context) map[string]codexLiteToolSpec {
	if c == nil {
		return nil
	}
	value, exists := c.Get(codexLiteContextKey)
	if !exists {
		return nil
	}
	specs, _ := value.(map[string]codexLiteToolSpec)
	return specs
}
