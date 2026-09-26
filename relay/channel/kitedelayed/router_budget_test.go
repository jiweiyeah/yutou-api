package kitedelayed

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func resetKiteRouterCaches() {
	kiteRouterCatalogMu.Lock()
	kiteRouterCatalogCache = make(map[string]*kiteRouterCatalog)
	kiteRouterCatalogMu.Unlock()
	kiteRouterBalanceMu.Lock()
	kiteRouterBalanceByChannel = make(map[int]map[int]kiteRouterBalanceEntry)
	kiteRouterBalanceMu.Unlock()
}

func setupKiteRouterTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))

	oldDB := model.DB
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	t.Cleanup(func() {
		model.DB = oldDB
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	return db
}

func kiteRouterTestCatalogResponse() map[string]any {
	return map[string]any{
		"object": "router.catalog",
		"models": []any{
			map[string]any{"id": "gpt-6-astra", "context_length": 1050000, "max_output_tokens": 8192,
				"pricing": map[string]any{"unit": "per_1m_token", "input": "15.0", "output": "75.0", "currency": "USD"}},
			map[string]any{"id": "gpt-5.6-sol", "context_length": 1050000, "max_output_tokens": 8192,
				"pricing": map[string]any{"unit": "per_1m_token", "input": "6.0", "output": "30.0", "currency": "USD"}},
			map[string]any{"id": "gpt-5.6-luna", "context_length": 1050000, "max_output_tokens": 8192,
				"pricing": map[string]any{"unit": "per_1m_token", "input": "0.300", "output": "1.800", "currency": "USD"}},
			map[string]any{"id": "solar-pro4", "context_length": 524288, "max_output_tokens": 8192,
				"pricing": map[string]any{"unit": "per_1m_token", "input": "0.135", "output": "0.540", "currency": "USD"}},
		},
	}
}

// TestKiteRouterThresholdBytesMatchesCalibration 钉住「字节制」的阈值模型：
// threshold = (safe - max_output × 输出价/1e6 - 0.004) / (输入价/1e6)。
// 这些数字全部来自 2026-09-26 对上游 402 required_microusd 的实测标定。
func TestKiteRouterThresholdBytesMatchesCalibration(t *testing.T) {
	const safe = kiteRouterDefaultSafeBudgetUSD
	cases := []struct {
		model     string
		wantBytes int
	}{
		{"gpt-6-astra", 258773},
		{"gpt-5.6-sol", 708373},
		{"claude-opus-5", 558506},
		{"claude-sonnet-5", 1457706},
		{"gpt-5.6-luna", 14937514},
		{"solar-pro4", 33270935},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			price, ok := kiteRouterLookupPrice(kiteRouterFallbackPrices, tc.model)
			require.True(t, ok, "兜底价格表必须覆盖 %s", tc.model)
			got := price.thresholdBytes(safe)
			assert.Equal(t, tc.wantBytes, got)

			// 阈值本身必须精确落在安全线上（文档 §4.3 的校验方式）。
			required := price.requiredUSD(got, 0)
			assert.InDelta(t, safe, required, 0.0001, "阈值字节数回代后应恰好等于安全线")
		})
	}
}

// TestKiteRouterRequiredUSDReproducesUpstreamCalibration 用实测数据回归公式。
// 期望值是上游真实返回的 required_microusd（见 docs/kite-router-codex-context-budget.md）。
func TestKiteRouterRequiredUSDReproducesUpstreamCalibration(t *testing.T) {
	cases := []struct {
		model      string
		promptByte int
		maxTokens  int
		wantUSD    float64
	}{
		{"gpt-6-astra", 150000, 1024, 2.3311},
		{"gpt-6-astra", 150000, 2048, 2.4079},
		{"gpt-6-astra", 150000, 8192, 2.8687},
		{"gpt-6-astra", 2000, 8192, 0.6487},
		{"gpt-5.6-sol", 150000, 1024, 0.9324},
		{"kimi-k3", 250000, 1024, 0.8052},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			price, ok := kiteRouterLookupPrice(kiteRouterFallbackPrices, tc.model)
			require.True(t, ok)
			got := price.requiredUSD(tc.promptByte, tc.maxTokens)
			// 上游还会计入 JSON 外壳的少量字节，允许 1% 误差。
			assert.InDelta(t, tc.wantUSD, got, tc.wantUSD*0.01)
		})
	}
}

func TestKiteRouterRequiredUSDUsesCatalogMaxOutputWhenMaxTokensAbsent(t *testing.T) {
	price, ok := kiteRouterLookupPrice(kiteRouterFallbackPrices, "gpt-6-astra")
	require.True(t, ok)

	// 不传 max_tokens 时按目录的 max_output_tokens（8192）预留，而不是 0。
	assert.Equal(t, 8192, price.reserveOutputTokens(0))
	// 显式更小的 max_tokens 会真正降低预留。
	assert.Equal(t, 1024, price.reserveOutputTokens(1024))
	// 超过目录上限的值按目录上限算（上游本身会 400，这里不放大估算）。
	assert.Equal(t, 8192, price.reserveOutputTokens(99999))
}

func TestKiteRouterBudgetExceededBoundary(t *testing.T) {
	cases := []struct {
		required float64
		want     bool
	}{
		{4.4999, false},
		{4.5000, false},
		{4.5001, true},
		{5.0000, true},
	}
	for _, tc := range cases {
		budget := &kiteRouterBudget{RequiredUSD: tc.required, SafeUSD: kiteRouterDefaultSafeBudgetUSD}
		assert.Equal(t, tc.want, budget.exceeded(), "required=$%.4f", tc.required)
	}
	assert.False(t, (*kiteRouterBudget)(nil).exceeded())
}

func TestKiteRouterCompactionPayloadRoundTrip(t *testing.T) {
	original := kiteRouterCompactionPayload{
		Version: kiteRouterCompactionPayloadVersion,
		Model:   "openai/gpt-5.6-luna",
		At:      1790416000,
		Tokens:  310000,
		Summary: "目标：修好 X。已完成：改了 a.go:12。下一步：跑测试。",
	}
	encrypted, err := encodeKiteRouterCompactionPayload(original)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(encrypted, kiteRouterCompactionPrefix))

	decoded, err := decodeKiteRouterCompactionPayload(encrypted)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}

func TestKiteRouterCompactionPayloadRejectsBadInput(t *testing.T) {
	cases := []struct {
		name      string
		encrypted string
	}{
		{"empty", ""},
		{"foreign prefix", "gAAAAABopaque-from-another-gateway"},
		{"bad base64", kiteRouterCompactionPrefix + "!!!not-base64!!!"},
		{"not json", kiteRouterCompactionPrefix + "aGVsbG8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeKiteRouterCompactionPayload(tc.encrypted)
			require.Error(t, err)
		})
	}

	// 版本不认识 → 明确错误，不猜。
	unsupported, err := encodeKiteRouterCompactionPayload(kiteRouterCompactionPayload{Version: 99, Summary: "x"})
	require.NoError(t, err)
	_, err = decodeKiteRouterCompactionPayload(unsupported)
	require.ErrorContains(t, err, "unsupported compaction block version")

	// 空摘要也算损坏。
	empty, err := encodeKiteRouterCompactionPayload(kiteRouterCompactionPayload{Version: kiteRouterCompactionPayloadVersion, Summary: "   "})
	require.NoError(t, err)
	_, err = decodeKiteRouterCompactionPayload(empty)
	require.ErrorContains(t, err, "empty summary")
}

func TestExpandKiteRouterCompactionItemsReplacesBlockWithDeveloperMessage(t *testing.T) {
	encrypted, err := encodeKiteRouterCompactionPayload(kiteRouterCompactionPayload{
		Version: kiteRouterCompactionPayloadVersion,
		Model:   "openai/gpt-5.6-luna",
		Summary: "已完成 a.go 的改动。",
	})
	require.NoError(t, err)

	raw := `{"model":"openai/gpt-6-astra","input":[` +
		`{"type":"compaction","id":"cmp_1","encrypted_content":"` + encrypted + `"},` +
		`{"type":"message","role":"user","content":"继续"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))

	changed, err := expandKiteRouterCompactionItems(&request)
	require.NoError(t, err)
	assert.True(t, changed)

	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	require.Len(t, items, 2)
	assert.Equal(t, "message", items[0]["type"])
	assert.Equal(t, "developer", items[0]["role"])
	assert.Contains(t, items[0]["content"], "已完成 a.go 的改动。")
	assert.NotContains(t, items[0], "encrypted_content")
	// 后续条目原样保留
	assert.Equal(t, "user", items[1]["role"])
}

func TestExpandKiteRouterCompactionItemsLeavesPlainInputUntouched(t *testing.T) {
	raw := `{"model":"openai/gpt-6-astra","input":[{"type":"message","role":"user","content":"hi"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))
	before := string(request.Input)

	changed, err := expandKiteRouterCompactionItems(&request)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, before, string(request.Input), "没有压缩块时必须逐字节不变")
}

func TestExpandKiteRouterCompactionItemsFailsLoudOnForeignBlock(t *testing.T) {
	raw := `{"model":"openai/gpt-6-astra","input":[{"type":"compaction","id":"cmp_1","encrypted_content":"opaque"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))

	_, err := expandKiteRouterCompactionItems(&request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input[0]")
}

// TestKiteRouterBudgetGateRejectsOversizedPrompt 覆盖 §5.1：超限请求必须在**发上游之前**
// 被拒，且绝不能白跑一次往返。
func TestKiteRouterBudgetGateRejectsOversizedPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	var chatCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == kiteRouterCatalogPath:
			writeJSONResponse(t, w, http.StatusOK, kiteRouterTestCatalogResponse())
		case r.URL.Path == routerPath:
			chatCalls.Add(1)
			writeJSONResponse(t, w, http.StatusOK, chatCompletionResult())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.RelayMode = relayconstant.RelayModeResponses
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.UpstreamModelName = "openai/gpt-6-astra"

	// astra 的 $4.5 安全线是 258,773 字节；这里给 300KB 正文，注定超限。
	payload := `{"model":"openai/gpt-6-astra","input":"` + strings.Repeat("x", 300000) + `"}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(payload), &request))

	adaptor := &Adaptor{}
	adaptor.Init(info)
	converted, err := adaptor.ConvertOpenAIResponsesRequest(ctx, info, request)
	require.Error(t, err, "超限请求必须被闸门拦下")
	assert.Nil(t, converted)

	apiErr, ok := err.(*types.NewAPIError)
	require.True(t, ok, "必须是 *types.NewAPIError，got %T", err)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, "context_budget_exceeded", string(apiErr.GetErrorCode()))
	assert.Contains(t, apiErr.Error(), "258773")
	assert.Contains(t, apiErr.Error(), "字节")
	assert.Contains(t, apiErr.Error(), "压缩上下文", "错误信息必须给出可执行建议")
	assert.Contains(t, string(apiErr.Metadata), `"threshold_bytes":258773`)

	assert.Equal(t, int32(0), chatCalls.Load(), "注定失败的请求绝不能发给上游")
}

func TestKiteRouterBudgetGateAllowsRequestUnderBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == kiteRouterCatalogPath {
			writeJSONResponse(t, w, http.StatusOK, kiteRouterTestCatalogResponse())
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.RelayMode = relayconstant.RelayModeResponses
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.UpstreamModelName = "openai/gpt-6-astra"

	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"openai/gpt-6-astra","input":"hi"}`), &request))

	adaptor := &Adaptor{}
	adaptor.Init(info)
	converted, err := adaptor.ConvertOpenAIResponsesRequest(ctx, info, request)
	require.NoError(t, err)
	assert.IsType(t, &dto.GeneralOpenAIRequest{}, converted)

	budget := kiteRouterBudgetFrom(ctx)
	require.NotNil(t, budget, "放行的请求要把估算结果留给选 key 复用")
	assert.Less(t, budget.RequiredUSD, budget.SafeUSD)
	assert.Equal(t, "openai/gpt-6-astra", budget.Model)
}

// TestKiteRouterBudgetGateSkipsWhenPriceUnknown 取价失败不能变成拒绝请求。
func TestKiteRouterBudgetGateSkipsWhenPriceUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == kiteRouterCatalogPath {
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"object": "router.catalog", "models": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.RelayMode = relayconstant.RelayModeResponses
	info.UpstreamModelName = "brand-new-model-without-price"

	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(
		`{"model":"brand-new-model-without-price","input":"`+strings.Repeat("x", 400000)+`"}`), &request))

	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.ConvertOpenAIResponsesRequest(ctx, info, request)
	require.NoError(t, err, "取不到价格时必须放行，保持既有行为")
	assert.Nil(t, kiteRouterBudgetFrom(ctx))
}

// TestKiteRouterBudgetScopeIsolation Marathon 线（不带 /kite-router 后缀）的转换结果
// 必须与原生 openai 适配器逐字节相同，且不产生任何上游旁路请求。
func TestKiteRouterBudgetScopeIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	var bypassCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bypassCalls.Add(1)
		writeJSONResponse(t, w, http.StatusOK, kiteRouterTestCatalogResponse())
	}))
	t.Cleanup(server.Close)

	body := `{"model":"glm-5.2","input":[{"type":"message","role":"user","content":"hi"}],"instructions":"be nice"}`
	build := func(baseURL string) []byte {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
		info := testRelayInfo(baseURL, false)
		info.RelayMode = relayconstant.RelayModeResponses
		info.UpstreamModelName = "glm-5.2"
		var request dto.OpenAIResponsesRequest
		require.NoError(t, common.Unmarshal([]byte(body), &request))
		adaptor := &Adaptor{}
		adaptor.Init(info)
		converted, err := adaptor.ConvertOpenAIResponsesRequest(ctx, info, request)
		require.NoError(t, err)
		encoded, err := common.Marshal(converted)
		require.NoError(t, err)
		assert.Nil(t, kiteRouterBudgetFrom(ctx), "Marathon 线不得触发预算闸门")
		return encoded
	}

	marathon := build(server.URL)

	// Marathon 线走的正是内嵌 openai 适配器（原样透传），所以参照物就是请求本身。
	var reference dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(body), &reference))
	referenceJSON, err := common.Marshal(reference)
	require.NoError(t, err)

	assert.Equal(t, string(referenceJSON), string(marathon), "Marathon 线必须与原生 openai 转换逐字节一致")
	assert.Equal(t, int32(0), bypassCalls.Load(), "Marathon 线不得发起任何 catalog/credits 旁路请求")
}

func kiteRouterTestChannel(models string, mapping string) *model.Channel {
	channel := &model.Channel{Models: models}
	if mapping != "" {
		channel.ModelMapping = common.GetPointer(mapping)
	}
	return channel
}

func TestKiteRouterCompactionModelPrefersCheapestNeverTriggering(t *testing.T) {
	channel := kiteRouterTestChannel("openai/gpt-6-astra,openai/gpt-5.6-sol,openai/gpt-5.6-luna,upstage/solar-pro4", "")

	// 有「永不触发」档时，取其中输入价最低的。
	name, upstream, price, ok := kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	require.True(t, ok)
	assert.Equal(t, "upstage/solar-pro4", name)
	assert.Equal(t, 0.135, price.InputPerMillion)
	// 没配映射时，上游名退回目录里的裸 id（上游只认裸 id）。
	assert.Equal(t, "solar-pro4", upstream)

	// 目录里只剩会触发的模型时，退化为「最便宜的那个」，而不是报错。
	channel = kiteRouterTestChannel("openai/gpt-6-astra,openai/gpt-5.6-sol", "")
	name, _, _, ok = kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.6-sol", name)

	// 一个都没有 → 明确失败，调用方返回 compaction_model_unavailable。
	channel = kiteRouterTestChannel("unknown/model", "")
	_, _, _, ok = kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	assert.False(t, ok)
}

// TestKiteRouterCompactionModelResolvesAliasesThroughModelMapping 钉住纯别名渠道：
// 声明名在上游目录里不存在（如 gpt-5-codex → gpt-6-astra），价格必须按映射后的名字查，
// 否则这类渠道一个候选都选不出来。
func TestKiteRouterCompactionModelResolvesAliasesThroughModelMapping(t *testing.T) {
	channel := kiteRouterTestChannel(
		"gpt-5-codex,my-cheap-alias",
		`{"gpt-5-codex":"gpt-6-astra","my-cheap-alias":"solar-pro4"}`,
	)
	name, upstream, price, ok := kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	require.True(t, ok)
	// 两个别名都不在目录里；映射后一个贵一个便宜，应选中便宜的那个。
	assert.Equal(t, "my-cheap-alias", name)
	assert.Equal(t, "solar-pro4", upstream)
	assert.Equal(t, 0.135, price.InputPerMillion)

	// 别名映射到一个目录里也没有的模型 → 选不出来（不猜）。
	channel = kiteRouterTestChannel("mystery", `{"mystery":"not-in-catalog"}`)
	_, _, _, ok = kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	assert.False(t, ok)
}

func TestKiteRouterPickKeyRejectsWhenEveryProbedKeyIsDrained(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()
	db := setupKiteRouterTestDB(t)

	var creditsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == kiteRouterCreditsPath {
			creditsCalls.Add(1)
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"currency": "USD", "balance": "0.10"})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	autoBan := 1
	channel := &model.Channel{
		Id:      10901,
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "kite-router-test",
		Key:     "key-a\nkey-b\nkey-c",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: common.GetPointer(server.URL + routerMarker),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 3,
			MultiKeyMode: constant.MultiKeyModeRandom,
		},
	}
	require.NoError(t, db.Create(channel).Error)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = channel.Id
	info.ChannelIsMultiKey = true
	info.ChannelMultiKeyIndex = 0
	info.ApiKey = "key-a"

	adaptor := &Adaptor{}
	adaptor.Init(info)
	key, index, pick := adaptor.kiteRouterPickKey(ctx, info, 2.0)
	assert.Equal(t, kiteRouterKeyPickNoneAffordable, pick)
	assert.Empty(t, key)
	assert.Zero(t, index)
	assert.Positive(t, creditsCalls.Load(), "必须真的去查过余额")
}

func TestKiteRouterPickKeySelectsAKeyWithEnoughBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()
	db := setupKiteRouterTestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != kiteRouterCreditsPath {
			http.NotFound(w, r)
			return
		}
		balance := "0.10"
		if strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == "key-b" {
			balance = "4.90"
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"currency": "USD", "balance": balance})
	}))
	t.Cleanup(server.Close)

	autoBan := 1
	channel := &model.Channel{
		Id:      10902,
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "kite-router-test",
		Key:     "key-b",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: common.GetPointer(server.URL + routerMarker),
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 1,
			MultiKeyMode: constant.MultiKeyModeRandom,
		},
	}
	require.NoError(t, db.Create(channel).Error)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = channel.Id
	info.ChannelIsMultiKey = true
	info.ChannelMultiKeyIndex = 7
	info.ApiKey = "key-a"

	adaptor := &Adaptor{}
	adaptor.Init(info)
	key, index, pick := adaptor.kiteRouterPickKey(ctx, info, 2.0)
	assert.Equal(t, kiteRouterKeyPickSelected, pick)
	assert.Equal(t, "key-b", key)
	assert.Equal(t, 0, index)
}

func TestKiteRouterPickKeyKeepsCurrentKeyWhenItIsHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()
	setupKiteRouterTestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"currency": "USD", "balance": "4.90"})
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = 10903
	info.ChannelMultiKeyIndex = 0
	info.ApiKey = "key-a"

	adaptor := &Adaptor{}
	adaptor.Init(info)
	key, _, pick := adaptor.kiteRouterPickKey(ctx, info, 2.0)
	assert.Equal(t, kiteRouterKeyPickUnchanged, pick, "余额够用时不该换 key")
	assert.Empty(t, key)
}

func TestKiteRouterPickKeySkipsSmallRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"currency": "USD", "balance": "0.01"})
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = 10904

	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, _, pick := adaptor.kiteRouterPickKey(ctx, info, kiteRouterBalanceFilterFloorUSD()/2)
	assert.Equal(t, kiteRouterKeyPickUnchanged, pick)
	assert.Zero(t, calls.Load(), "低于门槛的小请求不该产生 credits 往返")
}

// TestKiteRouterDoRequestMapsUpstream402 上游 402 必须变成可读错误，并记下该 key 已耗尽。
func TestKiteRouterDoRequestMapsUpstream402(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routerPath {
			http.NotFound(w, r)
			return
		}
		writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{
			"detail": map[string]any{
				"code":              "insufficient_router_balance",
				"message":           "insufficient Kite Router allowance",
				"required_microusd": 5278695,
			},
		})
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = 10905
	info.ChannelIsMultiKey = true
	info.ChannelMultiKeyIndex = 3
	info.ApiKey = "key-drained"

	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"openai/gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`))
	require.Error(t, err)

	apiErr, ok := err.(*types.NewAPIError)
	require.True(t, ok, "必须是 *types.NewAPIError，got %T", err)
	assert.Equal(t, "insufficient_router_balance", string(apiErr.GetErrorCode()))
	assert.NotContains(t, apiErr.Error(), "insufficient Kite Router allowance", "不得原样透传上游错误")
	assert.Contains(t, apiErr.Error(), "5.2787")
	assert.Contains(t, string(apiErr.Metadata), `"required_usd":5.278695`)

	balance, cached := kiteRouterCachedBalance(info.ChannelId, info.ChannelMultiKeyIndex)
	assert.True(t, cached, "402 之后必须把这个 key 记为余额不足")
	assert.Zero(t, balance)
}

func TestKiteRouterDoRequestLeavesOther402Untouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{"detail": "some other payment problem"})
	}))
	t.Cleanup(server.Close)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	info := testRelayInfo(server.URL+routerMarker, false)
	info.ChannelId = 10906
	info.ChannelMultiKeyIndex = 0

	adaptor := &Adaptor{}
	adaptor.Init(info)
	_, err := adaptor.DoRequest(ctx, info, strings.NewReader(`{"model":"openai/gpt-6-astra","messages":[]}`))
	require.Error(t, err)

	_, cached := kiteRouterCachedBalance(info.ChannelId, 0)
	assert.False(t, cached, "非额度类 402 不该影响余额缓存")
}

func TestKiteRouterCompactionModelSelectionIgnoresUnknownModels(t *testing.T) {
	channel := kiteRouterTestChannel("  ,vendor/unknown-model,openai/gpt-5.6-luna", "")
	name, upstream, price, ok := kiteRouterCompactionModel(channel, kiteRouterFallbackPrices, kiteRouterDefaultSafeBudgetUSD)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5.6-luna", name)
	assert.Equal(t, "gpt-5.6-luna", upstream)
	assert.Equal(t, 0.30, price.InputPerMillion)
}

func TestKiteRouterSafeBudgetChannelOverride(t *testing.T) {
	info := &relaycommon.RelayInfo{}
	assert.Equal(t, kiteRouterDefaultSafeBudgetUSD, kiteRouterSafeBudgetUSD(info))

	info.ChannelMeta = &relaycommon.ChannelMeta{}
	custom := 3.25
	info.ChannelOtherSettings = dto.ChannelOtherSettings{KiteRouterSafeBudgetUSD: &custom}
	assert.Equal(t, 3.25, kiteRouterSafeBudgetUSD(info))

	// 越界配置必须回退到默认值，而不是把闸门变成「永远拒绝」或「永不触发」。
	invalid := 9.9
	info.ChannelOtherSettings = dto.ChannelOtherSettings{KiteRouterSafeBudgetUSD: &invalid}
	assert.Equal(t, kiteRouterDefaultSafeBudgetUSD, kiteRouterSafeBudgetUSD(info))

	// 余额过滤默认开启，可被渠道设置关掉。
	assert.True(t, kiteRouterBalanceFilterEnabled(info))
	disabled := false
	info.ChannelOtherSettings = dto.ChannelOtherSettings{KiteRouterBalanceFilter: &disabled}
	assert.False(t, kiteRouterBalanceFilterEnabled(info))
}

func TestStripKiteRouterCompactionTriggerRemovesRequestControl(t *testing.T) {
	raw := `{"model":"openai/gpt-6-astra","input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"compaction_trigger"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))

	found, err := stripKiteRouterCompactionTrigger(&request)
	require.NoError(t, err)
	assert.True(t, found)

	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	require.Len(t, items, 1, "compaction_trigger 只是请求侧控制项，必须摘掉")
	assert.Equal(t, "user", items[0]["role"])

	// 普通请求必须逐字节不变。
	var plain dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`), &plain))
	before := string(plain.Input)
	found, err = stripKiteRouterCompactionTrigger(&plain)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, before, string(plain.Input))
}

// TestKiteRouterCompactionV2EndToEnd 覆盖 Codex 远程压缩 v2 的完整往返：
// /v1/responses + compaction_trigger → 便宜模型摘要 → compaction item → 下一轮解码回消息。
func TestKiteRouterCompactionV2EndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetKiteRouterCaches()
	db := setupKiteRouterTestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case kiteRouterCatalogPath:
			writeJSONResponse(t, w, http.StatusOK, kiteRouterTestCatalogResponse())
		case routerPath:
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				"id":      "chatcmpl_compact",
				"object":  "chat.completion",
				"created": 1785418437,
				"model":   "solar-pro4",
				"choices": []any{map[string]any{
					"index":         0,
					"finish_reason": "stop",
					"message":       map[string]any{"role": "assistant", "content": "目标：修好 X。已改 a.go:12。下一步：跑测试。"},
				}},
				"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 40, "total_tokens": 940},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	autoBan := 1
	channel := &model.Channel{
		Id:      10907,
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "kite-router-test",
		Key:     "key-a",
		Models:  "openai/gpt-6-astra,openai/gpt-5.6-luna,upstage/solar-pro4",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: common.GetPointer(server.URL + routerMarker),
	}
	// 生产渠道就是这么配的：上游只认裸目录 id，靠 model_mapping 映射过去。
	channel.ModelMapping = common.GetPointer(`{"upstage/solar-pro4":"solar-pro4","openai/gpt-6-astra":"gpt-6-astra"}`)
	require.NoError(t, db.Create(channel).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	info := testRelayInfo(server.URL+routerMarker, true)
	info.ChannelId = channel.Id
	info.RelayMode = relayconstant.RelayModeResponses
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.UpstreamModelName = "openai/gpt-6-astra"

	raw := `{"model":"openai/gpt-6-astra","input":[` +
		`{"type":"message","role":"user","content":"把 X 修好"},` +
		`{"type":"message","role":"assistant","content":"已改 a.go:12"},` +
		`{"type":"compaction_trigger"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))

	adaptor := &Adaptor{}
	adaptor.Init(info)
	converted, err := adaptor.ConvertOpenAIResponsesRequest(ctx, info, request)
	require.NoError(t, err)

	chatReq, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Equal(t, "solar-pro4", chatReq.Model, "压缩必须打到最便宜的可用模型上，且用上游认得的裸 id")
	require.NotEmpty(t, chatReq.Messages)
	assert.Equal(t, "system", chatReq.Messages[0].Role)
	assert.Contains(t, chatReq.Messages[0].Content, "context compactor")
	assert.Empty(t, chatReq.Tools, "压缩不需要工具")
	assert.Equal(t, false, *chatReq.Stream)
	plan := kiteRouterCompactionPlanFrom(ctx)
	require.NotNil(t, plan)
	assert.True(t, plan.V2)

	chatBody, err := common.Marshal(map[string]any{
		"id":      "chatcmpl_compact",
		"object":  "chat.completion",
		"created": 1785418437,
		"model":   "solar-pro4",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": "目标：修好 X。已改 a.go:12。下一步：跑测试。"},
		}},
		"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 40, "total_tokens": 940},
	})
	require.NoError(t, err)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(chatBody))),
	}

	usage, apiErr := adaptor.DoResponse(ctx, response, info)
	require.Nil(t, apiErr)
	require.IsType(t, &dto.Usage{}, usage, "responses_handler 用裸断言取 *dto.Usage")

	streamBody := recorder.Body.String()
	assert.Contains(t, streamBody, "event: "+codexLiteEventOutputItemDone)
	assert.Contains(t, streamBody, "event: "+codexLiteEventCompleted)
	assert.Contains(t, streamBody, `"type":"compaction"`)
	assert.NotContains(t, streamBody, "response.output_text.delta", "压缩响应不该带正文事件")

	// 取出 compaction item 的 encrypted_content，确认下一轮能解码回摘要。
	var doneEvent struct {
		Item struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	var payload string
	lines := strings.Split(streamBody, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) != "event: "+codexLiteEventOutputItemDone {
			continue
		}
		for _, candidate := range lines[index+1:] {
			if strings.HasPrefix(candidate, "data: ") {
				payload = strings.TrimPrefix(candidate, "data: ")
				break
			}
		}
		break
	}
	require.NotEmpty(t, payload, "必须回出 compaction item；body=%s", streamBody)
	require.NoError(t, common.Unmarshal([]byte(payload), &doneEvent))
	assert.Equal(t, "compaction", doneEvent.Item.Type)
	assert.True(t, strings.HasPrefix(doneEvent.Item.EncryptedContent, kiteRouterCompactionPrefix))

	decoded, err := decodeKiteRouterCompactionPayload(doneEvent.Item.EncryptedContent)
	require.NoError(t, err)
	assert.Contains(t, decoded.Summary, "已改 a.go:12")
	assert.Equal(t, "solar-pro4", decoded.Model)

	// 下一轮：Codex 只回传这个 compaction item。
	followUp := `{"model":"openai/gpt-6-astra","input":[` +
		`{"type":"compaction","id":"` + doneEvent.Item.ID + `","encrypted_content":"` + doneEvent.Item.EncryptedContent + `"},` +
		`{"type":"message","role":"user","content":"继续"}]}`
	var next dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(followUp), &next))
	changed, err := expandKiteRouterCompactionItems(&next)
	require.NoError(t, err)
	assert.True(t, changed)
	var nextItems []map[string]any
	require.NoError(t, common.Unmarshal(next.Input, &nextItems))
	assert.Equal(t, "developer", nextItems[0]["role"])
	assert.Contains(t, nextItems[0]["content"], "已改 a.go:12")
}

func TestKiteRouterCompactionRejectsForeignTriggerlessFollowUp(t *testing.T) {
	// 非本网关签发的压缩块必须报明确错误，不猜、不丢弃。
	raw := `{"model":"m","input":[{"type":"compaction","encrypted_content":"gAAAAA-foreign"}]}`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &request))
	_, err := expandKiteRouterCompactionItems(&request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input[0]")
}
