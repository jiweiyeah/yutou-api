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
)

type KiteCreditsScanSummary struct {
	Channels       int     `json:"channels"`
	KeysChecked    int     `json:"keys_checked"`
	LowBalanceKeys int     `json:"low_balance_keys"`
	DisabledKeys   int     `json:"disabled_keys"`
	RequestErrors  int     `json:"request_errors"`
	SkippedKeys    int     `json:"skipped_keys"`
	DisableErrors  int     `json:"disable_errors"`
	ThresholdUSD   float64 `json:"threshold_usd"`
}

type kiteCreditsResponse struct {
	Currency string `json:"currency"`
	Balance  any    `json:"balance"`
}

type kiteCreditJob struct {
	channel *model.Channel
	key     string
}

type kiteCreditResult struct {
	channel *model.Channel
	key     string
	balance float64
	err     error
}

type kiteCreditsRequestError struct {
	count int
	err   error
}

// RunKiteCreditsScan checks each enabled Kite Delayed key concurrently and
// auto-disables keys whose remaining USD credit is at or below the configured
// threshold. Transient HTTP/API errors are reported but never disable a key.
func RunKiteCreditsScan(ctx context.Context, report func(processed, total int)) (KiteCreditsScanSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	threshold := kiteCreditsDisableThreshold()
	summary := KiteCreditsScanSummary{ThresholdUSD: threshold}

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
			if channel.ChannelInfo.IsMultiKey && kiteMultiKeyStatus(channel, index) != common.ChannelStatusEnabled {
				summary.SkippedKeys++
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" {
				summary.SkippedKeys++
				continue
			}
			jobs = append(jobs, kiteCreditJob{channel: channel, key: key})
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
	lowBalanceByChannel := make(map[int]map[string]string)
	requestErrorsByChannel := make(map[int]kiteCreditsRequestError)
	for result := range resultCh {
		processed++
		if report != nil {
			report(processed, len(jobs))
		}
		if result.err != nil {
			summary.RequestErrors++
			entry := requestErrorsByChannel[result.channel.Id]
			entry.count++
			if entry.err == nil {
				entry.err = result.err
			}
			requestErrorsByChannel[result.channel.Id] = entry
			continue
		}
		if result.balance > threshold {
			continue
		}

		summary.LowBalanceKeys++
		reason := fmt.Sprintf("Kite credits low: $%.6f <= configured threshold $%.6f", result.balance, threshold)
		if lowBalanceByChannel[result.channel.Id] == nil {
			lowBalanceByChannel[result.channel.Id] = make(map[string]string)
		}
		lowBalanceByChannel[result.channel.Id][result.key] = reason
	}

	for channelID, requestError := range requestErrorsByChannel {
		common.SysError(fmt.Sprintf("Kite credits check failed for %d key(s) in channel #%d; first error: %v", requestError.count, channelID, requestError.err))
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	for channelID, reasonsByKey := range lowBalanceByChannel {
		disabled, err := model.AutoDisableChannelKeys(channelID, reasonsByKey)
		if err != nil {
			summary.DisableErrors += len(reasonsByKey)
			common.SysError(fmt.Sprintf("Kite credits failed to disable low-balance keys in channel #%d: %v", channelID, err))
			continue
		}
		summary.DisabledKeys += disabled
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

func checkKiteCredits(ctx context.Context, job kiteCreditJob) kiteCreditResult {
	result := kiteCreditResult{channel: job.channel, key: job.key}
	requestCtx, cancel := context.WithTimeout(ctx, kiteCreditsRequestTimeout)
	defer cancel()

	client, err := GetHttpClientWithProxy(job.channel.GetSetting().Proxy)
	if err != nil {
		result.err = err
		return result
	}
	requestURL := strings.TrimRight(job.channel.GetBaseURL(), "/") + kiteCreditsPath
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
