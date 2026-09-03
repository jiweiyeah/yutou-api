package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// DegradedMode 降级模式
type DegradedMode string

const (
	ModeDirect DegradedMode = "direct" // 直连模式
	ModeProxy  DegradedMode = "proxy"  // 代理模式
)

// DegradedState 渠道降级状态
type DegradedState struct {
	Mode             DegradedMode  `json:"mode"`
	SuccessCount     int           `json:"success_count"`
	FailureCount     int           `json:"failure_count"`
	TriggeredAt      time.Time     `json:"triggered_at"`
	LastStatusCode   int           `json:"last_status_code"`
	ConsecutiveOK    int           `json:"consecutive_ok"`     // 连续成功次数
	Consecutive429   int           `json:"consecutive_429"`    // 连续 429 次数
	TotalProxyReqs   int64         `json:"total_proxy_reqs"`   // 总代理请求数
	TotalDirectReqs  int64         `json:"total_direct_reqs"`  // 总直连请求数
	LastRecoveryTime time.Time     `json:"last_recovery_time"` // 上次恢复时间
	mu               sync.RWMutex  `json:"-"`
}

// DegradedManager 降级管理器
type DegradedManager struct {
	states             map[int]*DegradedState // channelID -> state
	mu                 sync.RWMutex
	proxyURL           string
	degradedThreshold  int           // 连续 N 次 429 后降级
	recoveryThreshold  int           // 连续 N 次成功后恢复
	queueDelayMin      time.Duration // 排队延迟最小值
	queueDelayMax      time.Duration // 排队延迟最大值
	enabledChannelIDs  map[int]bool  // 启用代理的渠道 ID
	transportCache     *http.Transport
	proxyTransportCache *http.Transport
}

var (
	globalManager     *DegradedManager
	globalManagerOnce sync.Once
)

// GetDegradedManager 获取全局降级管理器
func GetDegradedManager() *DegradedManager {
	globalManagerOnce.Do(func() {
		proxyURL := common.GetEnvOrDefaultString("BITDEER_PROXY_URL", "")
		degradedThreshold := common.GetEnvOrDefault("BITDEER_DEGRADED_THRESHOLD", 5)
		recoveryThreshold := common.GetEnvOrDefault("BITDEER_RECOVERY_THRESHOLD", 10)
		queueDelayRange := common.GetEnvOrDefaultString("BITDEER_QUEUE_DELAY_MS", "100-500")

		var queueDelayMin, queueDelayMax time.Duration
		_, _ = fmt.Sscanf(queueDelayRange, "%d-%d", &queueDelayMin, &queueDelayMax)
		if queueDelayMin == 0 {
			queueDelayMin = 100 * time.Millisecond
		}
		if queueDelayMax == 0 {
			queueDelayMax = 500 * time.Millisecond
		}

		// 默认启用 10835 和 10836 渠道
		enabledChannelIDs := map[int]bool{
			10835: true, // bitdeer-free
			10836: true, // bitdeer
		}

		globalManager = &DegradedManager{
			states:            make(map[int]*DegradedState),
			proxyURL:          proxyURL,
			degradedThreshold: degradedThreshold,
			recoveryThreshold: recoveryThreshold,
			queueDelayMin:     queueDelayMin,
			queueDelayMax:     queueDelayMax,
			enabledChannelIDs: enabledChannelIDs,
			transportCache: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		}

		// 初始化代理 Transport
		if proxyURL != "" {
			proxyURLParsed, err := url.Parse(proxyURL)
			if err == nil {
				globalManager.proxyTransportCache = &http.Transport{
					Proxy:               http.ProxyURL(proxyURLParsed),
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 10,
					IdleConnTimeout:     90 * time.Second,
				}
			}
		}

		common.SysLog(fmt.Sprintf("DegradedManager initialized: proxyURL=%s, degradedThreshold=%d, recoveryThreshold=%d, queueDelay=%v-%v",
			proxyURL, degradedThreshold, recoveryThreshold, queueDelayMin, queueDelayMax))
	})
	return globalManager
}

// IsEnabled 检查渠道是否启用代理管理
func (dm *DegradedManager) IsEnabled(channelID int) bool {
	if dm.proxyURL == "" {
		return false
	}
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return dm.enabledChannelIDs[channelID]
}

// GetState 获取渠道状态
func (dm *DegradedManager) GetState(channelID int) *DegradedState {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	state, exists := dm.states[channelID]
	if !exists {
		state = &DegradedState{
			Mode:        ModeDirect,
			TriggeredAt: time.Now(),
		}
		dm.states[channelID] = state
	}
	return state
}

// GetHTTPClient 获取适合当前模式的 HTTP 客户端
func (dm *DegradedManager) GetHTTPClient(channelID int) *http.Client {
	if !dm.IsEnabled(channelID) {
		return &http.Client{
			Timeout:   60 * time.Second,
			Transport: dm.transportCache,
		}
	}

	state := dm.GetState(channelID)
	state.mu.RLock()
	mode := state.Mode
	state.mu.RUnlock()

	var transport *http.Transport
	if mode == ModeProxy && dm.proxyTransportCache != nil {
		transport = dm.proxyTransportCache
	} else {
		transport = dm.transportCache
	}

	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}
}

// ApplyQueueDelay 在代理模式下应用排队延迟
func (dm *DegradedManager) ApplyQueueDelay(ctx context.Context, channelID int) {
	if !dm.IsEnabled(channelID) {
		return
	}

	state := dm.GetState(channelID)
	state.mu.RLock()
	mode := state.Mode
	state.mu.RUnlock()

	if mode == ModeProxy {
		// 随机延迟，避免请求同时发出
		delayRange := int(dm.queueDelayMax - dm.queueDelayMin)
		delay := dm.queueDelayMin + time.Duration(rand.Intn(delayRange))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

// RecordResponse 记录响应结果，动态调整模式
func (dm *DegradedManager) RecordResponse(channelID int, statusCode int, isSuccess bool) {
	if !dm.IsEnabled(channelID) {
		return
	}

	state := dm.GetState(channelID)
	state.mu.Lock()
	defer state.mu.Unlock()

	state.LastStatusCode = statusCode
	currentMode := state.Mode

	// 记录请求数
	if currentMode == ModeProxy {
		state.TotalProxyReqs++
	} else {
		state.TotalDirectReqs++
	}

	if statusCode == 429 {
		// 429 错误
		state.Consecutive429++
		state.ConsecutiveOK = 0
		state.FailureCount++

		// 检查是否需要降级
		if currentMode == ModeDirect && state.Consecutive429 >= dm.degradedThreshold {
			state.Mode = ModeProxy
			state.TriggeredAt = time.Now()
			state.Consecutive429 = 0
			common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d degraded to PROXY mode after %d consecutive 429 errors",
				channelID, dm.degradedThreshold))
		}
	} else if isSuccess {
		// 成功请求
		state.ConsecutiveOK++
		state.Consecutive429 = 0
		state.SuccessCount++

		// 检查是否需要恢复直连
		if currentMode == ModeProxy && state.ConsecutiveOK >= dm.recoveryThreshold {
			state.Mode = ModeDirect
			state.LastRecoveryTime = time.Now()
			state.ConsecutiveOK = 0
			common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d recovered to DIRECT mode after %d consecutive successes (proxy_reqs=%d, direct_reqs=%d)",
				channelID, dm.recoveryThreshold, state.TotalProxyReqs, state.TotalDirectReqs))
		}
	} else {
		// 其他错误
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
		"proxy_url":          dm.proxyURL,
	}
}

// ResetState 重置渠道状态（用于手动干预）
func (dm *DegradedManager) ResetState(channelID int) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	delete(dm.states, channelID)
	common.SysLog(fmt.Sprintf("[DegradedManager] Channel %d state reset", channelID))
}
