package proxy

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 并发下计数必须精确守恒：状态机在真实流量里是被大量 goroutine 同时写的，
// 计数漂移会让降级/恢复阈值判断失准。
// 该断言只在 -race 下才有完整意义，但计数守恒本身与 race 检测无关。
func TestConcurrentRecordResponseKeepsCountsExact(t *testing.T) {
	dm := newTestManager(testProxyURL, 1<<30, 1<<30) // 阈值取极大值，隔离出纯计数路径

	const (
		workers      = 64
		perWorker    = 200
		wantTotal    = workers * perWorker
		wantSuccess  = wantTotal / 2
		wantFailures = wantTotal - wantSuccess
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// 交替成功/429，两条分支都被并发压到
				if (w*perWorker+i)%2 == 0 {
					dm.RecordResponse(10836, 200, true)
				} else {
					dm.RecordResponse(10836, 429, false)
				}
			}
		}(w)
	}
	wg.Wait()

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()

	assert.Equal(t, wantSuccess, state.SuccessCount, "成功计数必须精确")
	assert.Equal(t, wantFailures, state.FailureCount, "失败计数必须精确")
	assert.EqualValues(t, wantTotal, state.TotalDirectReqs+state.TotalProxyReqs,
		"请求总数必须守恒，不得因竞态丢失")
}

// 并发调用 GetState 必须返回同一个实例：
// 若双重检查写错，会给同一渠道创建多份状态，计数被摊薄后阈值永远触发不了。
func TestConcurrentGetStateReturnsSameInstance(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)

	const workers = 64
	ptrs := make([]*DegradedState, workers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时冲进 GetState
			ptrs[i] = dm.GetState(10836)
		}(i)
	}
	close(start)
	wg.Wait()

	first := ptrs[0]
	require.NotNil(t, first)
	for i, p := range ptrs {
		assert.Same(t, first, p, "第 %d 个 goroutine 拿到了不同的 state 实例", i)
	}
}

// 纯 429 洪峰下必须降级并**永久**留在代理模式：
// 恢复分支只应由成功请求驱动，绝不能因并发计数错乱而误判恢复。
func TestConcurrent429FloodNeverRecovers(t *testing.T) {
	dm := newTestManager(testProxyURL, 10, 1<<30)

	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				dm.RecordResponse(10836, 429, false)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "持续 429 后必须处于代理模式")

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, ModeProxy, state.Mode)
	assert.Equal(t, 32*50, state.FailureCount)
	assert.Equal(t, 0, state.SuccessCount, "没有成功请求，成功计数必须为 0")
	assert.True(t, state.LastRecoveryTime.IsZero(), "从未恢复过，恢复时间必须为零值")
	assert.EqualValues(t, 32*50, state.TotalDirectReqs+state.TotalProxyReqs)
	assert.GreaterOrEqual(t, state.TotalDirectReqs, int64(10),
		"切换前至少有阈值数量的请求走过直连")
}

// 读写混合：ResolveProxyURL 是每请求都走的热路径，
// 与 RecordResponse 并发时不得死锁或读到撕裂状态。
// 两侧都跑固定轮数，不用停止信号，避免读侧空转拖慢 -race 运行。
func TestConcurrentResolveAndRecord(t *testing.T) {
	dm := newTestManager(testProxyURL, 5, 10)

	const (
		readers        = 16
		readsPerReader = 500
		writers        = 8
		writesPerW     = 500
	)

	var reads int64
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerReader; i++ {
				got := dm.ResolveProxyURL(10836)
				// 只可能是这两个值，出现别的说明读到了中间状态
				if got != "" && got != testProxyURL {
					t.Errorf("读到非法代理地址: %q", got)
					return
				}
				atomic.AddInt64(&reads, 1)
			}
		}()
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writesPerW; i++ {
				if i%3 == 0 {
					dm.RecordResponse(10836, 429, false)
				} else {
					dm.RecordResponse(10836, 200, true)
				}
			}
		}()
	}

	wg.Wait()

	assert.EqualValues(t, readers*readsPerReader, atomic.LoadInt64(&reads), "读路径应全部执行到")

	state := dm.GetState(10836)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.EqualValues(t, writers*writesPerW, state.TotalDirectReqs+state.TotalProxyReqs,
		"并发读写下请求总数仍须守恒")
}

// 多渠道并发时状态不得串台。
func TestConcurrentMultiChannelIsolation(t *testing.T) {
	dm := newTestManager(testProxyURL, 20, 1<<30)

	var wg sync.WaitGroup
	// 10836 全部 429（应降级），10835 全部成功（应留在直连）
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				dm.RecordResponse(10836, 429, false)
				dm.RecordResponse(10835, 200, true)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, testProxyURL, dm.ResolveProxyURL(10836), "10836 必须降级")
	assert.Equal(t, "", dm.ResolveProxyURL(10835), "10835 必须仍为直连")

	s35 := dm.GetState(10835)
	s35.mu.RLock()
	defer s35.mu.RUnlock()
	assert.Equal(t, 16*100, s35.SuccessCount)
	assert.Equal(t, 0, s35.FailureCount, "10835 不应染上 10836 的失败计数")
}
