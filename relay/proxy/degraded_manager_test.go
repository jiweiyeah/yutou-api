package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testProxyURL = "socks5://127.0.0.1:10808"

// newTestManager 构造一个不依赖环境变量与全局单例的管理器，
// 便于确定性地断言状态机行为。
func newTestManager(proxyURL string, degraded, recovery int) *DegradedManager {
	return &DegradedManager{
		states:            make(map[int]*DegradedState),
		proxyURL:          proxyURL,
		degradedThreshold: degraded,
		recoveryThreshold: recovery,
		queueDelayMin:     0,
		queueDelayMax:     0,
		enabledChannelIDs: map[int]bool{10835: true, 10836: true},
	}
}

// parseQueueDelayRange 必须按毫秒解析。
// 回归保护：早前实现直接用 %d 扫进 time.Duration，得到的是纳秒，
// 使排队延迟实际只有 100-500ns，等于完全失效。
func TestParseQueueDelayRange(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"默认区间", "100-500", 100 * time.Millisecond, 500 * time.Millisecond},
		{"自定义区间", "200-800", 200 * time.Millisecond, 800 * time.Millisecond},
		{"上下界相等", "300-300", 300 * time.Millisecond, 300 * time.Millisecond},
		{"下界为零", "0-250", 0, 250 * time.Millisecond},
		{"含空白", "  50-150  ", 50 * time.Millisecond, 150 * time.Millisecond},
		{"空值回落默认", "", 100 * time.Millisecond, 500 * time.Millisecond},
		{"格式非法回落默认", "abc", 100 * time.Millisecond, 500 * time.Millisecond},
		{"缺少上界回落默认", "100", 100 * time.Millisecond, 500 * time.Millisecond},
		{"上界小于下界回落默认", "500-100", 100 * time.Millisecond, 500 * time.Millisecond},
		{"负数回落默认", "-100-500", 100 * time.Millisecond, 500 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMin, gotMax := parseQueueDelayRange(tc.raw)
			assert.Equal(t, tc.wantMin, gotMin, "min delay")
			assert.Equal(t, tc.wantMax, gotMax, "max delay")
			assert.LessOrEqual(t, gotMin, gotMax, "min 不得大于 max")
		})
	}
}

// 纳管渠道必须能用环境变量覆盖：渠道重建后 ID 会变，
// 硬编码会让整个降级机制静默失效。
// 返回 nil 表示"未配置"，由调用方回落默认值，不能与"配了个空集合"混淆。
func TestParseChannelIDs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[int]bool
	}{
		{"单个渠道", "10836", map[int]bool{10836: true}},
		{"多个渠道", "10835,10836", map[int]bool{10835: true, 10836: true}},
		{"含空白", " 10835 , 10836 ", map[int]bool{10835: true, 10836: true}},
		{"忽略空项与重复项", "10835,,10836,10835,", map[int]bool{10835: true, 10836: true}},
		{"跳过非法项保留合法项", "10835,abc,10836", map[int]bool{10835: true, 10836: true}},
		{"跳过零与负数", "0,-5,10836", map[int]bool{10836: true}},
		{"空值视为未配置", "", nil},
		{"纯空白视为未配置", "   ", nil},
		{"全部非法视为未配置", "abc,def", nil},
		{"仅分隔符视为未配置", ",,,", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseChannelIDs(tc.raw))
		})
	}
}

// 未配置 BITDEER_PROXY_URL 时整个功能必须完全关闭，
// 且不得影响未纳管渠道的请求路径。
func TestIsEnabled(t *testing.T) {
	t.Run("代理地址为空则全部禁用", func(t *testing.T) {
		dm := newTestManager("", 5, 10)
		assert.False(t, dm.IsEnabled(10836))
		assert.False(t, dm.IsEnabled(10835))
	})

	t.Run("仅纳管指定渠道", func(t *testing.T) {
		dm := newTestManager(testProxyURL, 5, 10)
		assert.True(t, dm.IsEnabled(10836))
		assert.True(t, dm.IsEnabled(10835))
		assert.False(t, dm.IsEnabled(1), "未纳管渠道必须走原有路径")
		assert.False(t, dm.IsEnabled(0))
	})
}

// 初始必须是直连，即本机出口 IP 为主力。
func TestInitialModeIsDirect(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)

	state := dm.GetState(10836)
	assert.Equal(t, ModeDirect, state.Mode)
	assert.Equal(t, "", dm.ResolveProxyURL(10836), "直连模式必须返回空代理地址")
}

func TestDegradeAfterConsecutive429(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)

	for i := 1; i < 5; i++ {
		dm.RecordResponse(10836, 429, false)
		require.Equal(t, "", dm.ResolveProxyURL(10836), "第 %d 次 429 尚未达到阈值，必须保持直连", i)
	}

	dm.RecordResponse(10836, 429, false)
	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "达到阈值后必须切换到代理")

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, ModeProxy, state.Mode)
	assert.Equal(t, 5, state.FailureCount)
	assert.Equal(t, 0, state.Consecutive429, "切换后连续计数必须清零")
	assert.EqualValues(t, 5, state.TotalDirectReqs)
	assert.EqualValues(t, 0, state.TotalProxyReqs)
}

// 偶发 429 不应触发降级：成功请求必须打断连续计数。
func TestSuccessResetsConsecutive429(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)

	for i := 0; i < 4; i++ {
		dm.RecordResponse(10836, 429, false)
	}
	dm.RecordResponse(10836, 200, true)
	for i := 0; i < 4; i++ {
		dm.RecordResponse(10836, 429, false)
	}

	assert.Equal(t, "", dm.ResolveProxyURL(10836), "4+1+4 不构成连续 5 次 429，必须保持直连")
}

// 非 429 的上游错误不应导致降级，代理换 IP 对这类错误无效。
func TestNon429ErrorDoesNotDegrade(t *testing.T) {
	dm := newTestManager(testProxyURL, 3, 10)

	for _, code := range []int{500, 502, 503, 400, 401} {
		dm.RecordResponse(10836, code, false)
	}

	assert.Equal(t, "", dm.ResolveProxyURL(10836))

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, ModeDirect, state.Mode)
	assert.Equal(t, 5, state.FailureCount)
	assert.Equal(t, 0, state.Consecutive429)
}

func TestRecoverAfterConsecutiveSuccess(t *testing.T) {
	dm := newTestManager(testProxyURL, 2, 3)

	dm.RecordResponse(10836, 429, false)
	dm.RecordResponse(10836, 429, false)
	require.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "前置条件：应已处于代理模式")

	dm.RecordResponse(10836, 200, true)
	dm.RecordResponse(10836, 200, true)
	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "未达恢复阈值前必须留在代理模式")

	dm.RecordResponse(10836, 200, true)
	assert.Equal(t, "", dm.ResolveProxyURL(10836), "达到恢复阈值后必须回到直连")

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, ModeDirect, state.Mode)
	assert.EqualValues(t, 3, state.TotalProxyReqs, "代理模式下的 3 次请求应计入 proxy 计数")
	assert.EqualValues(t, 2, state.TotalDirectReqs)
	assert.False(t, state.LastRecoveryTime.IsZero())
}

// 代理模式下 429 未达阈值不得把状态"降级两次"或错误恢复。
func TestProxyMode429KeepsProxyMode(t *testing.T) {
	dm := newTestManager(testProxyURL, 2, 3)

	dm.RecordResponse(10836, 429, false)
	dm.RecordResponse(10836, 429, false)
	require.Equal(t, testProxyURL, dm.ResolveProxyURL(10836))

	for i := 0; i < 10; i++ {
		dm.RecordResponse(10836, 429, false)
	}

	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "代理模式下持续 429 仍应留在代理模式")
}

// 各渠道状态必须相互隔离，10836 降级不应影响 10835。
func TestPerChannelStateIsolation(t *testing.T) {
	dm := newTestManager(testProxyURL, 2, 10)

	dm.RecordResponse(10836, 429, false)
	dm.RecordResponse(10836, 429, false)

	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836))
	assert.Equal(t, "", dm.ResolveProxyURL(10835), "另一渠道必须仍为直连")
}

func TestDisabledManagerIgnoresRecording(t *testing.T) {
	dm := newTestManager("", 1, 1)

	for i := 0; i < 10; i++ {
		dm.RecordResponse(10836, 429, false)
	}

	assert.Equal(t, "", dm.ResolveProxyURL(10836))
	assert.Equal(t, map[string]interface{}{"enabled": false}, dm.GetStats(10836))
}

func TestResetState(t *testing.T) {
	dm := newTestManager(testProxyURL, 2, 10)

	dm.RecordResponse(10836, 429, false)
	dm.RecordResponse(10836, 429, false)
	require.Equal(t, testProxyURL, dm.ResolveProxyURL(10836))

	dm.ResetState(10836)

	assert.Equal(t, "", dm.ResolveProxyURL(10836), "重置后必须回到直连")
	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, 0, state.FailureCount)
	assert.EqualValues(t, 0, state.TotalDirectReqs)
}

// 直连模式不得引入任何延迟，这是"正常情况以本机 IP 为主力、零额外开销"的前提。
func TestApplyQueueDelaySkipsDirectMode(t *testing.T) {
	dm := newTestManager(testProxyURL, 2, 10)
	dm.queueDelayMin = 500 * time.Millisecond
	dm.queueDelayMax = 500 * time.Millisecond

	start := time.Now()
	dm.ApplyQueueDelay(context.Background(), 10836)
	assert.Less(t, time.Since(start), 100*time.Millisecond, "直连模式不应延迟")

	start = time.Now()
	dm.ApplyQueueDelay(context.Background(), 1)
	assert.Less(t, time.Since(start), 100*time.Millisecond, "未纳管渠道不应延迟")
}

// 客户端断开时排队延迟必须立即让出，不能把已取消的请求继续挂住。
func TestApplyQueueDelayRespectsContextCancel(t *testing.T) {
	dm := newTestManager(testProxyURL, 1, 10)
	dm.queueDelayMin = 10 * time.Second
	dm.queueDelayMax = 10 * time.Second

	dm.RecordResponse(10836, 429, false)
	require.Equal(t, testProxyURL, dm.ResolveProxyURL(10836))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	dm.ApplyQueueDelay(ctx, 10836)
	assert.Less(t, time.Since(start), time.Second, "context 取消后必须立即返回")
}

func TestGetStatsReportsConfiguredThresholds(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)
	dm.queueDelayMin = 100 * time.Millisecond
	dm.queueDelayMax = 500 * time.Millisecond

	stats := dm.GetStats(10836)

	assert.Equal(t, true, stats["enabled"])
	assert.Equal(t, ModeDirect, stats["mode"])
	assert.Equal(t, 5, stats["degraded_threshold"])
	assert.Equal(t, 10, stats["recovery_threshold"])
	assert.Equal(t, "100-500", stats["queue_delay_ms"])
	assert.NotContains(t, stats, "proxy_url", "统计接口不应回显代理地址")
}
