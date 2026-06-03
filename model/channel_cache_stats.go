package model

import (
	"sync/atomic"
	"time"
)

// ChannelCacheStats 渠道缓存性能统计
type ChannelCacheStats struct {
	// 渠道选择统计
	SelectionCount       atomic.Int64 // 总选择次数
	SelectionDurationUs  atomic.Int64 // 累计耗时（微秒）
	SlowSelectionCount   atomic.Int64 // 慢查询次数（>10ms）
	SelectionErrorCount  atomic.Int64 // 选择失败次数

	// 缓存同步统计
	SyncCount            atomic.Int64 // 同步次数
	SyncDurationMs       atomic.Int64 // 累计同步耗时（毫秒）
	LastSyncTime         atomic.Int64 // 最后同步时间（Unix时间戳）
	LastSyncChannelCount atomic.Int64 // 最后同步的渠道总数

	// 渠道状态变更统计
	ChannelDisableCount  atomic.Int64 // 渠道禁用次数
	ChannelEnableCount   atomic.Int64 // 渠道启用次数

	// 锁竞争统计（近似）
	LockWaitCount        atomic.Int64 // 锁等待次数（每次获取锁都计数）
}

var channelCacheStats ChannelCacheStats

// RecordChannelSelection 记录一次渠道选择
func RecordChannelSelection(durationUs int64, isError bool, isSlow bool) {
	channelCacheStats.SelectionCount.Add(1)
	channelCacheStats.SelectionDurationUs.Add(durationUs)
	if isError {
		channelCacheStats.SelectionErrorCount.Add(1)
	}
	if isSlow {
		channelCacheStats.SlowSelectionCount.Add(1)
	}
}

// RecordCacheSync 记录一次缓存同步
func RecordCacheSync(durationMs int64, channelCount int64) {
	channelCacheStats.SyncCount.Add(1)
	channelCacheStats.SyncDurationMs.Add(durationMs)
	channelCacheStats.LastSyncTime.Store(time.Now().Unix())
	channelCacheStats.LastSyncChannelCount.Store(channelCount)
}

// RecordChannelStatusChange 记录渠道状态变更
func RecordChannelStatusChange(isEnable bool) {
	if isEnable {
		channelCacheStats.ChannelEnableCount.Add(1)
	} else {
		channelCacheStats.ChannelDisableCount.Add(1)
	}
}

// RecordLockWait 记录锁等待
func RecordLockWait() {
	channelCacheStats.LockWaitCount.Add(1)
}

// GetChannelCacheStats 获取统计数据快照
func GetChannelCacheStats() map[string]interface{} {
	selectionCount := channelCacheStats.SelectionCount.Load()
	totalUs := channelCacheStats.SelectionDurationUs.Load()
	avgUs := int64(0)
	if selectionCount > 0 {
		avgUs = totalUs / selectionCount
	}

	syncCount := channelCacheStats.SyncCount.Load()
	totalSyncMs := channelCacheStats.SyncDurationMs.Load()
	avgSyncMs := int64(0)
	if syncCount > 0 {
		avgSyncMs = totalSyncMs / syncCount
	}

	return map[string]interface{}{
		// 选择统计
		"selection_count":         selectionCount,
		"selection_total_us":      totalUs,
		"selection_avg_us":        avgUs,
		"selection_avg_ms":        float64(avgUs) / 1000.0,
		"slow_selection_count":    channelCacheStats.SlowSelectionCount.Load(),
		"selection_error_count":   channelCacheStats.SelectionErrorCount.Load(),

		// 同步统计
		"sync_count":              syncCount,
		"sync_total_ms":           totalSyncMs,
		"sync_avg_ms":             avgSyncMs,
		"last_sync_time":          channelCacheStats.LastSyncTime.Load(),
		"last_sync_channel_count": channelCacheStats.LastSyncChannelCount.Load(),

		// 状态变更统计
		"channel_disable_count":   channelCacheStats.ChannelDisableCount.Load(),
		"channel_enable_count":    channelCacheStats.ChannelEnableCount.Load(),

		// 锁竞争统计
		"lock_wait_count":         channelCacheStats.LockWaitCount.Load(),
	}
}

// ResetChannelCacheStats 重置统计数据（用于测试或手动重置）
func ResetChannelCacheStats() {
	channelCacheStats = ChannelCacheStats{}
}
