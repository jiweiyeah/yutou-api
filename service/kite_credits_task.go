package service

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

const (
	kiteCreditsPath               = "/v1/credits"
	kiteCreditsDefaultThreshold   = 0.10
	kiteCreditsDefaultConcurrency = 16
	kiteCreditsRequestTimeout     = 15 * time.Second
	// kiteCreditsDefaultAuthFailMaxRatio caps the share of a channel's checked
	// keys that may be auto-disabled for HTTP 401 in a single scan.
	kiteCreditsDefaultAuthFailMaxRatio = 0.2
	// kiteCreditsAuthFailGuardMinChecked exempts small pools from the 401
	// guard: their blast radius is one easily re-enabled channel.
	kiteCreditsAuthFailGuardMinChecked = 10
	// kiteCreditsDefaultEnableRatio 是「重新启用阈值 / 禁用阈值」的默认倍数。
	// 两个阈值之间留一条死区，否则余额在阈值附近抖动时 key 会被反复启停。
	kiteCreditsDefaultEnableRatio = 2
)

type KiteCreditsScanSummary struct {
	Channels           int     `json:"channels"`
	KeysChecked        int     `json:"keys_checked"`
	LowBalanceKeys     int     `json:"low_balance_keys"`
	AuthFailedKeys     int     `json:"auth_failed_keys"`
	DisabledKeys       int     `json:"disabled_keys"`
	EnabledKeys        int     `json:"enabled_keys"`
	RequestErrors      int     `json:"request_errors"`
	SkippedKeys        int     `json:"skipped_keys"`
	DisableErrors      int     `json:"disable_errors"`
	EnableErrors       int     `json:"enable_errors"`
	ThresholdUSD       float64 `json:"threshold_usd"`
	EnableThresholdUSD float64 `json:"enable_threshold_usd"`
}

type kiteCreditsResponse struct {
	Currency string `json:"currency"`
	Balance  any    `json:"balance"`
}

type kiteCreditJob struct {
	channel    *model.Channel
	key        string
	wasEnabled bool
}

type kiteCreditResult struct {
	channel    *model.Channel
	key        string
	balance    float64
	err        error
	authFailed bool
	wasEnabled bool
}

type kiteCreditsRequestError struct {
	count int
	err   error
}

// RunKiteCreditsScan probes every enabled or auto-disabled Kite Delayed key with a
// read-only GET /v1/credits and makes the pool reflect that probe:
//   - credit at or below KITE_CREDITS_DISABLE_THRESHOLD (default 0.10) is
//     auto-disabled, as is a key the upstream rejects with HTTP 401 (dead key);
//   - credit at or above the enable threshold (the disable threshold times
//     KITE_CREDITS_ENABLE_RATIO, default 2) re-enables a key that was
//     auto-disabled, so a topped-up key comes back instead of staying dead
//     forever;
//   - anything in between leaves the key as it is — the dead band is what keeps
//     a balance hovering around the threshold from flapping.
//
// Manually disabled keys (status 2) are never probed and never re-enabled: the
// job must not overrule a human. Other transient HTTP/API errors are reported
// but never change a key.
//
// Every verdict is then applied to *all* Kite Delayed channels that hold the
// key, not just the channel it was probed through: the same upstream account is
// commonly mounted on several channels (Marathon + Kite Router), and key state
// is stored per channel.
//
// As a safety valve, 401-based disabling is skipped when the failing share
// exceeds KITE_CREDITS_AUTH_FAIL_MAX_RATIO (default 0.2): that pattern indicates
// an upstream auth outage rather than a batch of dead keys.
func RunKiteCreditsScan(ctx context.Context, report func(processed, total int)) (KiteCreditsScanSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	threshold := kiteCreditsDisableThreshold()
	enableThreshold := kiteCreditsEnableThreshold(threshold)
	summary := KiteCreditsScanSummary{ThresholdUSD: threshold, EnableThresholdUSD: enableThreshold}

	channels, err := model.GetEnabledChannelsWithKeysByType(constant.ChannelTypeKiteDelayed)
	if err != nil {
		return summary, err
	}
	summary.Channels = len(channels)

	jobs := make([]kiteCreditJob, 0)
	for _, channel := range channels {
		keys := channel.GetKeys()
		if !channel.GetAutoBan() {
			summary.SkippedKeys += len(keys)
			continue
		}
		for index, key := range keys {
			// 自动禁用的 key 也要探：不然余额回升后没人知道它能用了。
			if channel.ChannelInfo.IsMultiKey && kiteMultiKeyStatus(channel, index) == common.ChannelStatusManuallyDisabled {
				summary.SkippedKeys++
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" {
				summary.SkippedKeys++
				continue
			}
			jobs = append(jobs, kiteCreditJob{
				channel:    channel,
				key:        key,
				wasEnabled: !channel.ChannelInfo.IsMultiKey || kiteMultiKeyStatus(channel, index) == common.ChannelStatusEnabled,
			})
		}
	}

	summary.KeysChecked = len(jobs)
	if len(jobs) == 0 {
		if report != nil {
			report(0, 0)
		}
		return summary, nil
	}

	concurrency := common.GetEnvOrDefault("KITE_CREDITS_TASK_CONCURRENCY", kiteCreditsDefaultConcurrency)
	if concurrency < 1 {
		concurrency = kiteCreditsDefaultConcurrency
	}
	if concurrency > 128 {
		concurrency = 128
	}
	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}

	jobCh := make(chan kiteCreditJob, len(jobs))
	resultCh := make(chan kiteCreditResult, len(jobs))
	for _, job := range jobs {
		jobCh <- job
	}
	close(jobCh)

	var workers sync.WaitGroup
	workers.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer workers.Done()
			for job := range jobCh {
				resultCh <- checkKiteCredits(ctx, job)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(resultCh)
	}()

	processed := 0
	authFailMaxRatio := kiteCreditsAuthFailMaxRatio()
	checkedByChannel := make(map[int]int)
	authFailedByChannel := make(map[int]map[string]string)
	disableReasonsByChannel := make(map[int]map[string]string)
	requestErrorsByChannel := make(map[int]kiteCreditsRequestError)
	recoveredKeys := make(map[string]struct{})
	for result := range resultCh {
		processed++
		if report != nil {
			report(processed, len(jobs))
		}
		// 401 熔断闸门的分母只算「扫描开始时还在启用」的 key：已经被禁用的 key
		// 再 401 也不可能被禁第二次，算进去只会稀释闸门的灵敏度。
		if result.wasEnabled {
			checkedByChannel[result.channel.Id]++
		}
		if result.err != nil {
			if result.authFailed {
				if result.wasEnabled {
					summary.AuthFailedKeys++
					if authFailedByChannel[result.channel.Id] == nil {
						authFailedByChannel[result.channel.Id] = make(map[string]string)
					}
					authFailedByChannel[result.channel.Id][result.key] = fmt.Sprintf("Kite credits key unauthorized: %v", result.err)
				}
				continue
			}
			summary.RequestErrors++
			entry := requestErrorsByChannel[result.channel.Id]
			entry.count++
			if entry.err == nil {
				entry.err = result.err
			}
			requestErrorsByChannel[result.channel.Id] = entry
			continue
		}
		if result.balance >= enableThreshold {
			recoveredKeys[result.key] = struct{}{}
			continue
		}
		// 已经禁用的 key 只是被探来确认能不能放回，余额低不低已经没有新信息了：
		// 计数和禁用判定都只看扫描开始时还在启用的那些。
		if result.balance > threshold || !result.wasEnabled {
			continue
		}

		summary.LowBalanceKeys++
		reason := fmt.Sprintf("Kite credits low: $%.6f <= configured threshold $%.6f", result.balance, threshold)
		if disableReasonsByChannel[result.channel.Id] == nil {
			disableReasonsByChannel[result.channel.Id] = make(map[string]string)
		}
		disableReasonsByChannel[result.channel.Id][result.key] = reason
	}

	// 401-based disabling is skipped wholesale for a channel when too many of
	// its keys fail auth at once — that is an upstream auth outage, not a batch
	// of dead keys. Guard-tripped keys are reported as request errors instead.
	// They are also recorded in guardedKeys so the cross-channel fan-out below
	// cannot re-disable them through a sibling channel that did not trip.
	guardedKeys := make(map[string]struct{})
	for channelID, failedKeys := range authFailedByChannel {
		checked := checkedByChannel[channelID]
		if kiteAuthFailGuardTripped(len(failedKeys), checked, authFailMaxRatio) {
			summary.RequestErrors += len(failedKeys)
			entry := requestErrorsByChannel[channelID]
			entry.count += len(failedKeys)
			if entry.err == nil {
				entry.err = fmt.Errorf("upstream returned HTTP %d", http.StatusUnauthorized)
			}
			requestErrorsByChannel[channelID] = entry
			for key := range failedKeys {
				guardedKeys[key] = struct{}{}
			}
			common.SysError(fmt.Sprintf("Kite credits auth-fail disable skipped for channel #%d: %d/%d checked keys returned HTTP 401, exceeding guard ratio %.2f", channelID, len(failedKeys), checked, authFailMaxRatio))
			continue
		}
		if disableReasonsByChannel[channelID] == nil {
			disableReasonsByChannel[channelID] = make(map[string]string)
		}
		for key, reason := range failedKeys {
			disableReasonsByChannel[channelID][key] = reason
		}
	}

	for channelID, requestError := range requestErrorsByChannel {
		common.SysError(fmt.Sprintf("Kite credits check failed for %d key(s) in channel #%d; first error: %v", requestError.count, channelID, requestError.err))
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}

	// 同一个上游账号常常同时挂在多个 Kite Delayed 渠道上（10821 Marathon 与
	// 10867 Kite Router 就是同一个 key 池）。禁停状态是按渠道存的，所以判定必须
	// 并成一份再逐渠道下发：只写判定出来的那个渠道，另一个渠道会继续拿这个
	// key 去撞上游。AutoDisableChannelKeys 只处理渠道自己持有的 key，把并集
	// 传给不持有它的渠道是无害的空操作。
	mergedDisableReasons := make(map[string]string, len(disableReasonsByChannel))
	for _, reasonsByKey := range disableReasonsByChannel {
		for key, reason := range reasonsByKey {
			if _, guarded := guardedKeys[key]; guarded {
				continue
			}
			if _, exists := mergedDisableReasons[key]; !exists {
				mergedDisableReasons[key] = reason
			}
		}
	}
	for _, channel := range channels {
		if !channel.GetAutoBan() {
			continue // 该渠道明确不参与自动禁停，别把并集灌进去
		}
		affected := 0
		for _, key := range channel.GetKeys() {
			if _, ok := mergedDisableReasons[strings.TrimSpace(key)]; ok {
				affected++
			}
		}
		if affected == 0 {
			continue
		}
		disabled, err := model.AutoDisableChannelKeys(channel.Id, mergedDisableReasons)
		if err != nil {
			summary.DisableErrors += affected
			common.SysError(fmt.Sprintf("Kite credits failed to disable low-balance keys in channel #%d: %v", channel.Id, err))
			continue
		}
		summary.DisabledKeys += disabled
	}

	// 余额回到 enableThreshold 以上的 key 走同一套跨渠道下发。用
	// EnableAutoDisabledChannelKeys 而不是 EnableChannelKeys：只翻自动禁用的那一档，
	// 人工禁用（status=2）的绝不碰。
	mergedEnableKeys := make(map[string]struct{}, len(recoveredKeys))
	for key := range recoveredKeys {
		if _, conflicting := mergedDisableReasons[key]; conflicting {
			continue // 同一轮里两个渠道读出相反结论，以「低余额」为准
		}
		mergedEnableKeys[key] = struct{}{}
	}
	if len(mergedEnableKeys) > 0 {
		keysToEnable := make([]string, 0, len(mergedEnableKeys))
		for key := range mergedEnableKeys {
			keysToEnable = append(keysToEnable, key)
		}
		for _, channel := range channels {
			if !channel.GetAutoBan() {
				continue
			}
			held := 0
			for _, key := range channel.GetKeys() {
				if _, ok := mergedEnableKeys[strings.TrimSpace(key)]; ok {
					held++
				}
			}
			if held == 0 {
				continue
			}
			enabled, err := model.EnableAutoDisabledChannelKeys(channel.Id, keysToEnable)
			if err != nil {
				summary.EnableErrors += held
				common.SysError(fmt.Sprintf("Kite credits failed to re-enable recovered keys in channel #%d: %v", channel.Id, err))
				continue
			}
			summary.EnabledKeys += enabled
		}
	}

	return summary, nil
}

func kiteMultiKeyStatus(channel *model.Channel, index int) int {
	if channel.ChannelInfo.MultiKeyStatusList == nil {
		return common.ChannelStatusEnabled
	}
	if status, ok := channel.ChannelInfo.MultiKeyStatusList[index]; ok {
		return status
	}
	return common.ChannelStatusEnabled
}

// kiteCreditsHostBaseURL 返回 /v1/credits 所在的 host 根地址。Kite Delayed 渠道用
// base_url 的后缀区分 gokite 的两条线（Marathon / Kite Router），但 /v1/credits 是
// host 级端点、两条线共用 —— 不剥掉标记就会拼出 /kite-router/v1/credits，上游回 404。
func kiteCreditsHostBaseURL(channelBaseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(channelBaseURL), "/")
	if strings.HasSuffix(strings.ToLower(trimmed), constant.KiteDelayedRouterMarker) {
		trimmed = strings.TrimRight(trimmed[:len(trimmed)-len(constant.KiteDelayedRouterMarker)], "/")
	}
	return trimmed
}

func checkKiteCredits(ctx context.Context, job kiteCreditJob) kiteCreditResult {
	result := kiteCreditResult{channel: job.channel, key: job.key, wasEnabled: job.wasEnabled}
	requestCtx, cancel := context.WithTimeout(ctx, kiteCreditsRequestTimeout)
	defer cancel()

	client, err := GetHttpClientWithProxy(job.channel.GetSetting().Proxy)
	if err != nil {
		result.err = err
		return result
	}
	requestURL := kiteCreditsHostBaseURL(job.channel.GetBaseURL()) + kiteCreditsPath
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		result.err = err
		return result
	}
	req.Header.Set("Authorization", "Bearer "+job.key)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		result.err = err
		return result
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.err = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
		// 401 means the upstream rejected the key itself — the key is dead.
		// 403 is deliberately excluded: it may signal endpoint-level
		// permissions rather than a revoked key.
		result.authFailed = resp.StatusCode == http.StatusUnauthorized
		return result
	}

	var payload kiteCreditsResponse
	if err := common.DecodeJson(io.LimitReader(resp.Body, 1<<20), &payload); err != nil {
		result.err = fmt.Errorf("decode credits response: %w", err)
		return result
	}
	if payload.Currency != "" && !strings.EqualFold(strings.TrimSpace(payload.Currency), "USD") {
		result.err = fmt.Errorf("unexpected credits currency %q", payload.Currency)
		return result
	}
	result.balance, result.err = parseKiteCreditsBalance(payload.Balance)
	return result
}

func parseKiteCreditsBalance(value any) (float64, error) {
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case float64:
		return typed, validateKiteCreditsBalance(typed)
	case nil:
		return 0, fmt.Errorf("credits response balance is missing")
	default:
		text = fmt.Sprint(typed)
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid credits balance %q: %w", text, err)
	}
	return balance, validateKiteCreditsBalance(balance)
}

func validateKiteCreditsBalance(balance float64) error {
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return fmt.Errorf("invalid non-finite credits balance")
	}
	return nil
}

func kiteCreditsDisableThreshold() float64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("KITE_CREDITS_DISABLE_THRESHOLD", "0.10"))
	threshold, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 {
		common.SysError(fmt.Sprintf("invalid KITE_CREDITS_DISABLE_THRESHOLD=%q, using %.2f", raw, kiteCreditsDefaultThreshold))
		return kiteCreditsDefaultThreshold
	}
	return threshold
}

// kiteCreditsEnableThreshold 返回重新启用一个自动禁用 key 所需的余额下限。
// 默认是禁用阈值的 KITE_CREDITS_ENABLE_RATIO 倍：两个阈值之间留一条死区，
// 否则余额在阈值附近抖动时 key 会被反复启停。
func kiteCreditsEnableThreshold(disableThreshold float64) float64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("KITE_CREDITS_ENABLE_RATIO", "2"))
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 1 {
		common.SysError(fmt.Sprintf("invalid KITE_CREDITS_ENABLE_RATIO=%q, using %d", raw, kiteCreditsDefaultEnableRatio))
		ratio = kiteCreditsDefaultEnableRatio
	}
	return disableThreshold * ratio
}

func kiteCreditsAuthFailMaxRatio() float64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("KITE_CREDITS_AUTH_FAIL_MAX_RATIO", "0.2"))
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		common.SysError(fmt.Sprintf("invalid KITE_CREDITS_AUTH_FAIL_MAX_RATIO=%q, using %.2f", raw, kiteCreditsDefaultAuthFailMaxRatio))
		return kiteCreditsDefaultAuthFailMaxRatio
	}
	return ratio
}

// kiteAuthFailGuardTripped reports whether 401-based disabling should be
// skipped for a channel: too many keys failing auth in one scan means the
// upstream auth endpoint is broken, not that the keys are dead. Small pools
// are exempt — their blast radius is a single easily re-enabled channel.
// A maxRatio of 0 disables the guard (every 401 disables its key).
func kiteAuthFailGuardTripped(authFailed, checked int, maxRatio float64) bool {
	if authFailed == 0 || checked < kiteCreditsAuthFailGuardMinChecked || maxRatio <= 0 {
		return false
	}
	return float64(authFailed) > float64(checked)*maxRatio
}
