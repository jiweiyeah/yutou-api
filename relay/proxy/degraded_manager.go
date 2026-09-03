package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// DegradedMode 降级模式
type DegradedMode string

const (
	ModeDirect DegradedMode = "direct" // 直连模式，走本机出口 IP
	ModeProxy  DegradedMode = "proxy"  // 代理模式，走 Xray 代理池
)

const (
	defaultDegradedThreshold = 5
	defaultRecoveryThreshold = 10
	defaultQueueDelayMinMS   = 100
	defaultQueueDelayMaxMS   = 500
)

// DegradedState 渠道降级状态，所有字段读写均由 mu 保护
type DegradedState struct {
	mu               sync.RWMutex
	Mode             DegradedMode
	SuccessCount     int
	FailureCount     int
	TriggeredAt      time.Time
	LastStatusCode   int
	ConsecutiveOK    int   // 连续成功次数
	Consecutive429   int   // 连续 429 次数
	TotalProxyReqs   int64 // 总代理请求数
	TotalDirectReqs  int64 // 总直连请求数
	LastRecoveryTime time.Time
}

// DegradedManager 降级管理器。只负责维护「渠道当前应该走直连还是走代理」这个状态，
// HTTP 客户端的构造统一交给 service.GetHttpClientWithProxy，以便复用项目的
// 超时、重定向校验、TLS、HTTP/2 与连接池配置。
type DegradedManager struct {
	mu                sync.RWMutex
	states            map[int]*DegradedState // channelID -> state
	proxyURL          string
	degradedThreshold int           // 连续 N 次 429 后降级
	recoveryThreshold int           // 连续 N 次成功后恢复
	queueDelayMin     time.Duration // 排队延迟最小值
	queueDelayMax     time.Duration // 排队延迟最大值
	enabledChannelIDs map[int]bool  // 启用动态代理的渠道 ID
}

var (
	globalManager     *DegradedManager
	globalManagerOnce sync.Once
)

// parseQueueDelayRange 解析形如 "100-500" 的毫秒区间。
// 解析失败或取值非法时回落到默认区间。
func parseQueueDelayRange(raw string) (time.Duration, time.Duration) {
	minMS, maxMS := defaultQueueDelayMinMS, defaultQueueDelayMaxMS

	var parsedMin, parsedMax int
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d-%d", &parsedMin, &parsedMax); err == nil {
		if parsedMin >= 0 && parsedMax >= parsedMin {
			minMS, maxMS = parsedMin, parsedMax
		} else {
			common.SysLog(fmt.Sprintf("[DegradedManager] invalid BITDEER_QUEUE_DELAY_MS %q, falling back to %d-%d", raw, minMS, maxMS))
		}
	} else if strings.TrimSpace(raw) != "" {
		common.SysLog(fmt.Sprintf("[DegradedManager] cannot parse BITDEER_QUEUE_DELAY_MS %q: %v, falling back to %d-%d", raw, err, minMS, maxMS))
	}

	return time.Duration(minMS) * time.Millisecond, time.Duration(maxMS) * time.Millisecond
}

// parseChannelIDs 解析形如 "10835,10836" 的渠道 ID 列表。
// 非法项会被跳过并记日志；整体为空（含全部非法）时返回 nil，由调用方回落默认值。
func parseChannelIDs(raw string) map[int]bool {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	ids := make(map[int]bool)
	var invalid []string
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.Atoi(field)
		if err != nil || id <= 0 {
			invalid = append(invalid, field)
			continue
		}
		ids[id] = true
	}

	if len(invalid) > 0 {
		common.SysLog(fmt.Sprintf("[DegradedManager] ignored invalid BITDEER_ENABLED_CHANNELS entries: %s", strings.Join(invalid, ",")))
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// sortedChannelIDs 返回排序后的渠道 ID，仅用于日志输出稳定可读。
func sortedChannelIDs(ids map[int]bool) []int {
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// GetDegradedManager 获取全局降级管理器
func GetDegradedManager() *DegradedManager {
	globalManagerOnce.Do(func() {
		proxyURL := strings.TrimSpace(common.GetEnvOrDefaultString("BITDEER_PROXY_URL", ""))
		degradedThreshold := common.GetEnvOrDefault("BITDEER_DEGRADED_THRESHOLD", defaultDegradedThreshold)
		recoveryThreshold := common.GetEnvOrDefault("BITDEER_RECOVERY_THRESHOLD", defaultRecoveryThreshold)
		queueDelayMin, queueDelayMax := parseQueueDelayRange(common.GetEnvOrDefaultString("BITDEER_QUEUE_DELAY_MS", ""))

		if degradedThreshold <= 0 {
			degradedThreshold = defaultDegradedThreshold
		}
		if recoveryThreshold <= 0 {
			recoveryThreshold = defaultRecoveryThreshold
		}

		// 纳管渠道可用 BITDEER_ENABLED_CHANNELS 覆盖（逗号分隔的渠道 ID）。
		// 未配置时回落到 Bitdeer 两个渠道：多 key + 高并发，易触发上游 Cloudflare 的 IP 级限流。
		// 注意渠道重建后 ID 会变，届时必须更新该环境变量，否则机制会静默失效。
		enabledChannelIDs := parseChannelIDs(common.GetEnvOrDefaultString("BITDEER_ENABLED_CHANNELS", ""))
		if enabledChannelIDs == nil {
			enabledChannelIDs = map[int]bool{
				10835: true, // bitdeer-free
				10836: true, // bitdeer
			}
		}

		globalManager = &DegradedManager{
			states:            make(map[int]*DegradedState),
			proxyURL:          proxyURL,
			degradedThreshold: degradedThreshold,
			recoveryThreshold: recoveryThreshold,
			queueDelayMin:     queueDelayMin,
			queueDelayMax:     queueDelayMax,
			enabledChannelIDs: enabledChannelIDs,
		}

		common.SysLog(fmt.Sprintf("DegradedManager initialized: enabled=%t, channels=%v, degradedThreshold=%d, recoveryThreshold=%d, queueDelay=%v-%v",
			proxyURL != "", sortedChannelIDs(enabledChannelIDs), degradedThreshold, recoveryThreshold, queueDelayMin, queueDelayMax))
	})
	return globalManager
}

// IsEnabled 检查渠道是否启用动态代理管理
func (dm *DegradedManager) IsEnabled(channelID int) bool {
	if dm.proxyURL == "" {
		return false
	}
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return dm.enabledChannelIDs[channelID]
}

// GetState 获取渠道状态，不存在时初始化为直连模式
func (dm *DegradedManager) GetState(channelID int) *DegradedState {
	dm.mu.RLock()
	state, exists := dm.states[channelID]
	dm.mu.RUnlock()
	if exists {
		return state
	}

	dm.mu.Lock()
	defer dm.mu.Unlock()
	// 双重检查，避免并发下重复初始化
	if state, exists = dm.states[channelID]; exists {
		return state
	}
	state = &DegradedState{
		Mode:        ModeDirect,
		TriggeredAt: time.Now(),
	}
	dm.states[channelID] = state
	return state
}

// ResolveProxyURL 返回渠道在当前模式下应使用的代理地址，空字符串表示直连。
func (dm *DegradedManager) ResolveProxyURL(channelID int) string {
	if !dm.IsEnabled(channelID) {
		return ""
	}

	state := dm.GetState(channelID)
	state.mu.RLock()
	defer state.mu.RUnlock()

	if state.Mode == ModeProxy {
		return dm.proxyURL
	}
	return ""
}

// ApplyQueueDelay 在代理模式下应用随机排队延迟，降低瞬时并发速率。
// ctx 取消时立即返回，不阻塞已断开的请求。
func (dm *DegradedManager) ApplyQueueDelay(ctx context.Context, channelID int) {
	if !dm.IsEnabled(channelID) {
		return
	}

	state := dm.GetState(channelID)
	state.mu.RLock()
	mode := state.Mode
	state.mu.RUnlock()

	if mode != ModeProxy {
		return
	}

	delay := dm.queueDelayMin
	if span := int64(dm.queueDelayMax - dm.queueDelayMin); span > 0 {
		delay += time.Duration(rand.Int63n(span))
	}
	if delay <= 0 {
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// RecordResponse 记录响应结果并驱动状态机切换
func (dm *DegradedManager) RecordResponse(channelID int, statusCode int, isSuccess bool) {
	if !dm.IsEnabled(channelID) {
		return
	}

	state := dm.GetState(channelID)
	state.mu.Lock()
	defer state.mu.Unlock()

	state.LastStatusCode = statusCode
	currentMode := state.Mode

	if currentMode == ModeProxy {
		state.TotalProxyReqs++
	} else {
		state.TotalDirectReqs++
	}

	switch {
	case statusCode == 429:
		state.Consecutive429++
		state.ConsecutiveOK = 0
		state.FailureCount++

		if currentMode == ModeDirect && state.Consecutive429 >= dm.degradedThreshold {
			state.Mode = ModeProxy
			state.TriggeredAt = time.Now()
			state.Consecutive429 = 0
			common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d degraded to PROXY mode after %d consecutive 429 errors",
				channelID, dm.degradedThreshold))
		}

	case isSuccess:
		state.ConsecutiveOK++
		state.Consecutive429 = 0
		state.SuccessCount++

		if currentMode == ModeProxy && state.ConsecutiveOK >= dm.recoveryThreshold {
			state.Mode = ModeDirect
			state.LastRecoveryTime = time.Now()
			state.ConsecutiveOK = 0
			common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d recovered to DIRECT mode after %d consecutive successes (proxy_reqs=%d, direct_reqs=%d)",
				channelID, dm.recoveryThreshold, state.TotalProxyReqs, state.TotalDirectReqs))
		}

	default:
		// 其他错误：不计入降级/恢复判定，仅清空连续计数
		state.ConsecutiveOK = 0
		state.Consecutive429 = 0
		state.FailureCount++
	}
}

// GetStats 获取统计信息
func (dm *DegradedManager) GetStats(channelID int) map[string]interface{} {
	if !dm.IsEnabled(channelID) {
		return map[string]interface{}{
			"enabled": false,
		}
	}

	state := dm.GetState(channelID)
	state.mu.RLock()
	defer state.mu.RUnlock()

	return map[string]interface{}{
		"enabled":            true,
		"mode":               state.Mode,
		"success_count":      state.SuccessCount,
		"failure_count":      state.FailureCount,
		"consecutive_ok":     state.ConsecutiveOK,
		"consecutive_429":    state.Consecutive429,
		"total_proxy_reqs":   state.TotalProxyReqs,
		"total_direct_reqs":  state.TotalDirectReqs,
		"triggered_at":       state.TriggeredAt,
		"last_recovery_time": state.LastRecoveryTime,
		"degraded_threshold": dm.degradedThreshold,
		"recovery_threshold": dm.recoveryThreshold,
		"queue_delay_ms":     fmt.Sprintf("%d-%d", dm.queueDelayMin.Milliseconds(), dm.queueDelayMax.Milliseconds()),
	}
}

// ResetState 重置渠道状态（用于手动干预）
func (dm *DegradedManager) ResetState(channelID int) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	delete(dm.states, channelID)
	common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d state reset", channelID))
}
