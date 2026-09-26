package kitedelayed

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// Codex 原生上下文压缩（设计文档 §6）。
//
// ⚠️ 协议修正（2026-09-26，对 codex rust-v0.157.1 源码的核对结论，见文档 §6.1）：
// 当前 Codex **不使用** `POST /v1/responses/compact`。远程压缩走的是 **v2 协议** ——
// 一次普通的 `POST /v1/responses`，只是 input 末尾多一条
// `{"type":"compaction_trigger"}`，响应里回一个 `{"type":"compaction"}` item
// （`codex-rs/core/src/compact_remote_v2.rs`、`codex-rs/protocol/src/models.rs:1227`）。
// 两条路径这里都实现：
//   - v2：`/v1/responses` + compaction_trigger（Codex 实际会走的）；
//   - v1：`/v1/responses/compact`（new-api 已路由，其他客户端可能用）。
//
// 上游 Kite Router 两条都没有（OpenAPI 里没有对应路径），所以压缩由本网关自己实现：
//
//  1. 收到压缩请求（input = 完整历史）；
//  2. 用**便宜模型**把历史调上游 /v1/chat/completions 压成一段结构化摘要；
//  3. 把摘要包成 compaction item 返回，encrypted_content 用我们自己的
//     `kr1:<base64url(JSON)>` 格式承载。
//
// 为什么必须用便宜模型：压缩请求本身要把完整历史发给上游，用贵模型会死锁 ——
// 要压的正是那个把贵模型顶爆的上下文（155k 字节历史用 gpt-5.6-luna 约 $0.05，
// 用 gpt-6-astra 要 $2.33+）。选型见 kiteRouterCompactionModel。
//
// encrypted_content 是「服务端生成、客户端原样回传的不透明串」，所以内容由我们定义，
// **网关保持无状态**：不存任何会话，下一轮请求里由 expandKiteRouterCompactionItems
// 就地解码成一条 developer 消息。
const (
	kiteRouterCompactionItemType      = "compaction"
	kiteRouterCompactionItemTypeAlias = "compaction_summary"
	kiteRouterCompactionPrefix        = "kr1:"
	kiteRouterCompactionIDPrefix      = "cmp_"
	kiteRouterCompactionObject        = "response.compaction"

	// kiteRouterCompactionTriggerType 是 Codex v2 远程压缩的请求侧标记。
	kiteRouterCompactionTriggerType = "compaction_trigger"

	// kiteRouterCompactionMaxTokens 是摘要的 max_tokens 上限。摘要长度与历史长度无关，
	// 4k 足够容纳「目标 / 已做 / 决策 / 未决 / 下一步」这套结构。
	kiteRouterCompactionMaxTokens = 4096

	kiteRouterCompactionPayloadVersion = 1
	kiteRouterCompactionContextKey     = "kite_router_compaction_plan"
)

// kiteRouterCompactionInstruction 是压缩调用前置的 system 指令。
// 用英文：实测该上游对英文指令的结构化输出更稳定，且摘要正文会按原文语言保留。
const kiteRouterCompactionInstruction = `You are a context compactor for an agentic coding session. Rewrite the conversation below as a single structured handoff summary that another instance of you will continue from.

Preserve, with exact identifiers where they matter:
- the user's overall goal, and any explicit constraint, preference or decision
- what has already been done: files created or edited (exact paths), commands run and their results
- decisions taken and why; approaches tried and rejected
- open questions, known failures, and anything still broken
- the immediate next step

Drop: redundant tool output, repeated content, resolved dead ends, pleasantries.

Do not invent anything that is not in the conversation. Do not comment on this task. Output only the summary, written in the same language as the conversation.`

// kiteRouterCompactionExpansionPrefix 在解码出的摘要前加一行说明，让模型知道这不是
// 用户新说的话，而是被压缩过的历史。
const kiteRouterCompactionExpansionPrefix = "Conversation history compressed by the API gateway to fit the upstream context budget. Continue from this summary:\n\n"

// kiteRouterCompactionPayload 是 encrypted_content 里承载的内容。
type kiteRouterCompactionPayload struct {
	Version int    `json:"v"`
	Model   string `json:"model"`
	At      int64  `json:"at"`
	Tokens  int    `json:"tokens"`
	Summary string `json:"summary"`
}

// kiteRouterCompactionPlan 记录本轮压缩的选型，供 DoResponse 组装响应时复用。
type kiteRouterCompactionPlan struct {
	RequestedModel string
	SummaryModel   string
	PromptBytes    int
	MaxTokens      int
	RequiredUSD    float64
	SafeUSD        float64
	// V2 为 true 表示这是 Codex 的 /v1/responses + compaction_trigger 形态，
	// 响应要按 Responses 事件流回，而不是 /v1/responses/compact 的 JSON 信封。
	V2 bool
}

// stripKiteRouterCompactionTrigger 摘掉 input 里的 `{"type":"compaction_trigger"}`
// 标记项。返回是否命中：命中即代表这是 Codex 的一次远程压缩请求。
//
// 该标记只是**请求侧控制项**（"Compaction triggers are request controls, not durable
// response items"，codex-rs/protocol/src/models.rs:1240），转换层不认它，留着会变成
// 一条空消息，所以必须先摘掉。
func stripKiteRouterCompactionTrigger(request *dto.OpenAIResponsesRequest) (bool, error) {
	if request == nil || len(request.Input) == 0 || common.GetJsonType(request.Input) != "array" {
		return false, nil
	}
	var items []map[string]any
	if err := common.Unmarshal(request.Input, &items); err != nil {
		return false, fmt.Errorf("decode responses input failed: %w", err)
	}

	kept := make([]map[string]any, 0, len(items))
	found := false
	for _, item := range items {
		if strings.TrimSpace(common.Interface2String(item["type"])) == kiteRouterCompactionTriggerType {
			found = true
			continue
		}
		kept = append(kept, item)
	}
	if !found {
		return false, nil
	}
	encoded, err := common.Marshal(kept)
	if err != nil {
		return false, fmt.Errorf("encode responses input failed: %w", err)
	}
	request.Input = encoded
	return true, nil
}

func encodeKiteRouterCompactionPayload(payload kiteRouterCompactionPayload) (string, error) {
	encoded, err := common.Marshal(payload)
	if err != nil {
		return "", err
	}
	return kiteRouterCompactionPrefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeKiteRouterCompactionPayload(encrypted string) (kiteRouterCompactionPayload, error) {
	trimmed := strings.TrimSpace(encrypted)
	if !strings.HasPrefix(trimmed, kiteRouterCompactionPrefix) {
		return kiteRouterCompactionPayload{}, fmt.Errorf("unsupported compaction block prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(trimmed, kiteRouterCompactionPrefix))
	if err != nil {
		return kiteRouterCompactionPayload{}, fmt.Errorf("decode compaction block: %w", err)
	}
	var payload kiteRouterCompactionPayload
	if err := common.Unmarshal(raw, &payload); err != nil {
		return kiteRouterCompactionPayload{}, fmt.Errorf("parse compaction block: %w", err)
	}
	if payload.Version != kiteRouterCompactionPayloadVersion {
		return kiteRouterCompactionPayload{}, fmt.Errorf("unsupported compaction block version %d", payload.Version)
	}
	if strings.TrimSpace(payload.Summary) == "" {
		return kiteRouterCompactionPayload{}, fmt.Errorf("compaction block carries an empty summary")
	}
	return payload, nil
}

func kiteRouterIsCompactionItemType(itemType string) bool {
	return itemType == kiteRouterCompactionItemType || itemType == kiteRouterCompactionItemTypeAlias
}

// expandKiteRouterCompactionItems 把 input 里的 compaction item 就地还原成一条
// developer 消息。返回 changed 为 false 时调用方保持原有输入不变（逐字节相同）。
//
// 解码失败不猜也不丢：按设计文档 §6.4 返回明确错误。
func expandKiteRouterCompactionItems(request *dto.OpenAIResponsesRequest) (bool, error) {
	if request == nil || len(request.Input) == 0 || common.GetJsonType(request.Input) != "array" {
		return false, nil
	}
	var items []map[string]any
	if err := common.Unmarshal(request.Input, &items); err != nil {
		return false, fmt.Errorf("decode responses input failed: %w", err)
	}

	changed := false
	for index, item := range items {
		itemType := strings.TrimSpace(common.Interface2String(item["type"]))
		if !kiteRouterIsCompactionItemType(itemType) {
			continue
		}
		payload, err := decodeKiteRouterCompactionPayload(common.Interface2String(item["encrypted_content"]))
		if err != nil {
			return false, fmt.Errorf("invalid compaction block at input[%d]: %w", index, err)
		}
		items[index] = map[string]any{
			"type":    "message",
			"role":    "developer",
			"content": kiteRouterCompactionExpansionPrefix + payload.Summary,
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	encoded, err := common.Marshal(items)
	if err != nil {
		return false, fmt.Errorf("encode responses input failed: %w", err)
	}
	request.Input = encoded
	return true, nil
}

func kiteRouterCompactionPlanFrom(c *gin.Context) *kiteRouterCompactionPlan {
	if c == nil {
		return nil
	}
	value, exists := c.Get(kiteRouterCompactionContextKey)
	if !exists {
		return nil
	}
	plan, _ := value.(*kiteRouterCompactionPlan)
	return plan
}

// kiteRouterCompactionModel 在渠道在售模型里挑压缩专用模型：优先「阈值永不触发」
// 的那一档，再取其中输入价最低的（压缩成本几乎全在输入侧）。
func kiteRouterCompactionModel(models []string, catalog map[string]kiteRouterModelPrice, safeBudgetUSD float64) (string, kiteRouterModelPrice, bool) {
	bestSafe, bestAny := "", ""
	var priceSafe, priceAny kiteRouterModelPrice
	for _, candidate := range models {
		name := strings.TrimSpace(candidate)
		if name == "" {
			continue
		}
		price, ok := kiteRouterLookupPrice(catalog, name)
		if !ok || !price.usable() {
			continue
		}
		if bestAny == "" || kiteRouterPriceLess(price, priceAny) {
			bestAny, priceAny = name, price
		}
		if price.canTriggerBudget(safeBudgetUSD) {
			continue
		}
		if bestSafe == "" || kiteRouterPriceLess(price, priceSafe) {
			bestSafe, priceSafe = name, price
		}
	}
	if bestSafe != "" {
		return bestSafe, priceSafe, true
	}
	if bestAny != "" {
		return bestAny, priceAny, true
	}
	return "", kiteRouterModelPrice{}, false
}

func kiteRouterPriceLess(left, right kiteRouterModelPrice) bool {
	if left.InputPerMillion != right.InputPerMillion {
		return left.InputPerMillion < right.InputPerMillion
	}
	return left.OutputPerMillion < right.OutputPerMillion
}

// kiteRouterUpstreamModelName 把渠道在售的模型名解析成上游真正接受的名字。
//
// ⚠️ 实测坑：上游只认目录里的**裸 id**（`solar-pro4`），带 vendor 前缀的
// `upstage/solar-pro4` 会直接 400 `unsupported or unavailable Kite Router model`。
// 渠道的 models 用的是 `vendor/name` 形式，靠 model_mapping 映射到裸 id。
// 所以压缩调用的 model 字段必须过一遍映射，否则会拿一个上游不认的名字去打。
func kiteRouterUpstreamModelName(channel *model.Channel, catalog map[string]kiteRouterModelPrice, modelName string) string {
	name := strings.TrimSpace(modelName)
	if name == "" || channel == nil {
		return name
	}
	if mapping := strings.TrimSpace(channel.GetModelMapping()); mapping != "" {
		var mapped map[string]string
		if err := common.UnmarshalJsonStr(mapping, &mapped); err == nil {
			if target := strings.TrimSpace(mapped[name]); target != "" {
				return target
			}
		}
	}
	if _, ok := catalog[strings.ToLower(name)]; ok {
		return name
	}
	// 没配映射、且这个名字不是目录里的 id 时，退回最后一段（vendor/name → name）。
	if _, tail, found := strings.Cut(name, "/"); found {
		if _, ok := catalog[strings.ToLower(tail)]; ok {
			return tail
		}
	}
	return name
}

// buildKiteRouterCompactionRequest 把压缩请求翻译成一次 chat completions 调用：
// 目标是压缩专用模型，前置一段摘要指令，不带任何工具。
func (a *Adaptor) buildKiteRouterCompactionRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest, v2 bool) (any, *types.NewAPIError) {
	channel, err := model.CacheGetChannel(info.ChannelId)
	if err != nil || channel == nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("load channel %d for compaction failed: %v", info.ChannelId, err),
			types.ErrorCodeGetChannelFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}

	safeBudget := kiteRouterSafeBudgetUSD(info)
	catalog := a.kiteRouterCatalogFor(c, info)
	summaryModel, price, ok := kiteRouterCompactionModel(channel.GetModels(), catalog, safeBudget)
	if !ok {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("no usable upstream model price for Kite Router compaction on channel %d", info.ChannelId),
			types.ErrorCode("compaction_model_unavailable"), http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	upstreamSummaryModel := kiteRouterUpstreamModelName(channel, catalog, summaryModel)
	// 日志里要如实写出「这一轮实际上游用的是哪个模型」，否则事后排查根本看不出
	// 压缩调用发去了哪（UpstreamModelName 只用于日志与响应 model 字段，不参与计费）。
	info.UpstreamModelName = upstreamSummaryModel

	// 转换层不接受 stateful 字段；compact 请求常带 previous_response_id，必须清掉。
	request.PreviousResponseID = ""
	request.Tools = nil
	request.ToolChoice = nil
	request.Model = upstreamSummaryModel
	request.MaxOutputTokens = common.GetPointer(uint(kiteRouterCompactionMaxTokens))

	converted, err := relayconvert.ResponsesRequestToChatCompletionsRequest(&request)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(
			err, types.ErrorCodeConvertRequestFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	converted.Stream = common.GetPointer(false)
	converted.StreamOptions = nil
	converted.MaxTokens = nil
	converted.Messages = append(
		[]dto.Message{{Role: "system", Content: kiteRouterCompactionInstruction}},
		converted.Messages...,
	)

	// §6.5：单条输入本身就超阈值时压缩也救不了 —— 先估预算，再决定是否值得发出去。
	budget := &kiteRouterBudget{
		Model:       upstreamSummaryModel,
		PromptBytes: kiteRouterPromptBytes(converted),
		MaxTokens:   kiteRouterCompactionMaxTokens,
		SafeUSD:     safeBudget,
		Price:       price,
	}
	budget.RequiredUSD = price.requiredUSD(budget.PromptBytes, budget.MaxTokens)
	kiteRouterSetBudgetHeader(c, budget)
	if budget.exceeded() {
		return nil, a.kiteRouterBudgetError(c, info, budget)
	}

	c.Set(kiteRouterCompactionContextKey, &kiteRouterCompactionPlan{
		RequestedModel: info.UpstreamModelName,
		SummaryModel:   upstreamSummaryModel,
		PromptBytes:    budget.PromptBytes,
		MaxTokens:      budget.MaxTokens,
		RequiredUSD:    budget.RequiredUSD,
		SafeUSD:        safeBudget,
		V2:             v2,
	})
	// 压缩调用同样要挑一个余额够的 key（历史很长时它的预检金额并不小）。
	c.Set(kiteRouterBudgetContextKey, budget)
	return converted, nil
}

// kiteRouterCompactionSummary 从上游 chat 响应里取出摘要正文，并把摘要包成
// compaction item 的 encrypted_content。
func (a *Adaptor) kiteRouterCompactionSummary(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (string, *dto.Usage, *types.NewAPIError) {
	resultBody, err := readAndCloseResponse(resp, maximumBodySize)
	if err != nil {
		return "", nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(resultBody, &chatResp); err != nil {
		return "", nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if upstreamError := chatResp.GetOpenAIError(); upstreamError != nil && upstreamError.Type != "" {
		return "", nil, types.WithOpenAIError(*upstreamError, resp.StatusCode)
	}

	summary := kiteRouterFirstMessageText(&chatResp)
	if summary == "" {
		return "", nil, types.NewOpenAIError(
			fmt.Errorf("Kite Router compaction returned no summary text"),
			types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}

	plan := kiteRouterCompactionPlanFrom(c)
	summaryModel := ""
	if plan != nil {
		summaryModel = plan.SummaryModel
	}
	encrypted, err := encodeKiteRouterCompactionPayload(kiteRouterCompactionPayload{
		Version: kiteRouterCompactionPayloadVersion,
		Model:   summaryModel,
		At:      common.GetTimestamp(),
		Tokens:  info.GetEstimatePromptTokens(),
		Summary: summary,
	})
	if err != nil {
		return "", nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	usage := &dto.Usage{
		PromptTokens:     chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
		TotalTokens:      chatResp.Usage.TotalTokens,
		InputTokens:      chatResp.Usage.PromptTokens,
		OutputTokens:     chatResp.Usage.CompletionTokens,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: chatResp.Usage.PromptTokensDetails.CachedTokens,
		},
	}
	return encrypted, usage, nil
}

// kiteRouterCompactionItem 组装 Codex 期望的 compaction item。
func kiteRouterCompactionItem(compactionID, encrypted string) map[string]any {
	return map[string]any{
		"type":              kiteRouterCompactionItemType,
		"id":                compactionID,
		"encrypted_content": encrypted,
	}
}

// doKiteRouterCompaction 把上游的 chat 响应包成 /v1/responses/compact 的 JSON 信封
// （v1 形态，handler 侧是纯透传）。
func (a *Adaptor) doKiteRouterCompaction(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	encrypted, usage, apiErr := a.kiteRouterCompactionSummary(c, resp, info)
	if apiErr != nil {
		return nil, apiErr
	}

	compactionID := kiteRouterCompactionIDPrefix + common.GetRandomString(24)
	output, err := common.Marshal([]any{kiteRouterCompactionItem(compactionID, encrypted)})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	responseBody, err := common.Marshal(dto.OpenAIResponsesCompactionResponse{
		ID:        compactionID,
		Object:    kiteRouterCompactionObject,
		CreatedAt: int(common.GetTimestamp()),
		Output:    output,
		Usage:     usage,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

// doKiteRouterCompactionV2Stream 按 Codex v2 远程压缩的契约回一个 Responses 事件流：
// 一个 response.output_item.done（item 为 compaction）+ 一个 response.completed。
// 客户端只认这两件事（codex-rs/core/src/compact_remote_v2.rs 的 collect_compaction_output）。
func (a *Adaptor) doKiteRouterCompactionV2Stream(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	encrypted, usage, apiErr := a.kiteRouterCompactionSummary(c, resp, info)
	if apiErr != nil {
		return nil, apiErr
	}

	compactionID := kiteRouterCompactionIDPrefix + common.GetRandomString(24)
	item := kiteRouterCompactionItem(compactionID, encrypted)

	helper.SetEventStreamHeaders(c)
	donePayload, err := common.Marshal(map[string]any{
		"type": codexLiteEventOutputItemDone,
		"item": item,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: codexLiteEventOutputItemDone}, string(donePayload)); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	completedPayload, err := common.Marshal(map[string]any{
		"type": codexLiteEventCompleted,
		"response": map[string]any{
			"id":         "resp_" + common.GetRandomString(24),
			"object":     "response",
			"status":     "completed",
			"created_at": common.GetTimestamp(),
			"output":     []any{item},
			"usage":      usage,
		},
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}
	if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: codexLiteEventCompleted}, string(completedPayload)); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	return usage, nil
}

// kiteRouterFirstMessageText 从 chat 响应的第一条 choice 里取出正文。
func kiteRouterFirstMessageText(response *dto.OpenAITextResponse) string {
	if response == nil || len(response.Choices) == 0 {
		return ""
	}
	switch content := response.Choices[0].Content.(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		var text strings.Builder
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text.WriteString(common.Interface2String(part["text"]))
		}
		return strings.TrimSpace(text.String())
	}
	return ""
}
