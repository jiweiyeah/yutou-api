package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	dahlBalancePath           = "/tokens/current"
	dahlChatPath              = "/v1/chat/completions"
	dahlDefaultProbeModelHint = "minimax-m2.7"

	dahlDefaultDisableThreshold      int64 = 500_000
	dahlDefaultIdleDays                    = 5
	dahlDefaultBalanceCheckMinutes         = 240
	dahlDefaultProbeRetryMinutes           = 180
	dahlDefaultMaxProbesPerRun             = 0
	dahlDefaultScanConcurrency             = 4
	dahlDefaultProbeConcurrency            = 2
	dahlDefaultProbeMaxTokens              = 16
	dahlDefaultRequestTimeoutSeconds       = 20
	dahlDefaultProbeTimeoutSeconds         = 60
)

var errDahlBalanceExhausted = errors.New("dahl available tokens exhausted")

// DahlKeyMaintenanceSummary reports one dahl key-pool maintenance pass.
type DahlKeyMaintenanceSummary struct {
	Channels          int `json:"channels"`
	KeysEnabled       int `json:"keys_enabled"`
	BalanceChecked    int `json:"balance_checked"`
	BalanceErrors     int `json:"balance_errors"`
	LowBalanceKeys    int `json:"low_balance_keys"`
	AuthFailedKeys    int `json:"auth_failed_keys"`
	DisabledKeys      int `json:"disabled_keys"`
	IdleProbes        int `json:"idle_probes"`
	ProbeSucceeded    int `json:"probe_succeeded"`
	ProbeFailed       int `json:"probe_failed"`
	ProbeErrors       int `json:"probe_errors"`
	SkippedKeys       int `json:"skipped_keys"`
	DisableErrors     int `json:"disable_errors"`
	HealthWriteErrors int `json:"health_write_errors"`
}

type dahlMaintenanceConfig struct {
	channelIDs        map[int]struct{}
	disableThreshold  int64
	idle              time.Duration
	balanceCheckEvery time.Duration
	probeRetryEvery   time.Duration
	maxBalanceChecks  int
	maxProbes         int
	probeEnabled      bool
	scanConcurrency   int
	probeConcurrency  int
	probeModel        string
	probeMaxTokens    int
	requestTimeout    time.Duration
	probeTimeout      time.Duration
}

type dahlKeyEntry struct {
	key      string
	healthID string
	health   model.ChannelKeyHealth
}

type dahlBalanceCheckResult struct {
	entry      dahlKeyEntry
	balance    int64
	authFailed bool
	err        error
}

type dahlProbeResult struct {
	entry      dahlKeyEntry
	ok         bool
	exhausted  bool
	authFailed bool
	err        error
}

// RunDahlKeyMaintenance keeps dahl key pools healthy with the smallest useful
// amount of upstream traffic:
//   - every enabled key is balance-checked on a rolling interval, with keys
//     near the threshold checked first and much more often;
//   - a key below the disable threshold is disabled in one batch write;
//   - a key whose balance has not moved for the configured idle window gets one
//     cheap chat completion (max_tokens is intentionally tiny), which both
//     proves the key works and resets dahl's 7-day inactivity clock;
//   - all state is persisted in channel_info, so the task needs no extra table
//     and a restart only loses the in-flight work, never the schedule.
//
// Balance checks are capped per run and spread with a priority/oldest-first
// order; idle probes are capped separately. At steady state a 1.2k-key pool
// needs roughly one request per key per five days plus a 4-hour balance sweep.
func RunDahlKeyMaintenance(ctx context.Context, report func(processed, total int)) (DahlKeyMaintenanceSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := loadDahlMaintenanceConfig()
	summary := DahlKeyMaintenanceSummary{}

	channels, err := model.GetChannelsWithKeys()
	if err != nil {
		return summary, err
	}
	for _, channel := range channels {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if channel.Status != common.ChannelStatusEnabled || !channel.ChannelInfo.IsMultiKey {
			continue
		}
		if !isDahlMaintenanceChannel(channel, cfg.channelIDs) {
			continue
		}
		if !channel.GetAutoBan() {
			summary.SkippedKeys += len(channel.GetKeys())
			continue
		}
		summary.Channels++
		runDahlChannelMaintenance(ctx, channel, cfg, &summary, report)
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	return summary, nil
}

func runDahlChannelMaintenance(ctx context.Context, channel *model.Channel, cfg dahlMaintenanceConfig, summary *DahlKeyMaintenanceSummary, report func(processed, total int)) {
	start := *summary
	baseURL := strings.TrimRight(strings.TrimSpace(channel.GetBaseURL()), "/")
	if baseURL == "" {
		common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d has no base_url", channel.Id))
		return
	}
	client, err := GetHttpClientWithProxy(channel.GetSetting().Proxy)
	if err != nil {
		common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d client unavailable: %v", channel.Id, err))
		return
	}

	keys := channel.GetKeys()
	if len(keys) == 0 {
		return
	}
	health := make(map[string]model.ChannelKeyHealth, len(channel.ChannelInfo.MultiKeyHealth))
	maps.Copy(health, channel.ChannelInfo.MultiKeyHealth)
	now := common.GetTimestamp()

	entries := make([]dahlKeyEntry, 0, len(keys))
	enabledCount := 0
	seenKeys := make(map[string]struct{}, len(keys))
	for index, rawKey := range keys {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			summary.SkippedKeys++
			continue
		}
		if dahlMultiKeyStatus(channel, index) != common.ChannelStatusEnabled {
			continue
		}
		enabledCount++
		healthID := dahlKeyHealthID(key)
		if _, duplicate := seenKeys[healthID]; duplicate {
			continue
		}
		seenKeys[healthID] = struct{}{}
		entry := dahlKeyEntry{
			key:      key,
			healthID: healthID,
			health:   health[healthID],
		}
		if dahlBalanceDue(entry.health, now, cfg.balanceCheckEvery, cfg.disableThreshold) {
			entries = append(entries, entry)
		}
	}
	summary.KeysEnabled += enabledCount

	sort.Slice(entries, func(i, j int) bool {
		left := dahlBalancePriority(entries[i].health, cfg.disableThreshold)
		right := dahlBalancePriority(entries[j].health, cfg.disableThreshold)
		if left != right {
			return left < right
		}
		return entries[i].health.CheckedAt < entries[j].health.CheckedAt
	})

	maxChecks := cfg.maxBalanceChecks
	if maxChecks <= 0 {
		maxChecks = (enabledCount + 3) / 4
		maxChecks = max(100, min(maxChecks, 2000))
	}
	if len(entries) > maxChecks {
		entries = entries[:maxChecks]
	}

	reasonsByKey := make(map[string]string)
	firstBalanceError := ""
	if len(entries) > 0 {
		results := runDahlBalanceChecks(ctx, client, baseURL, entries, cfg, func(processed int) {
			if report != nil {
				report(processed, len(entries))
			}
		})
		for _, result := range results {
			summary.BalanceChecked++
			entry := result.entry
			current := health[entry.healthID]
			current.CheckedAt = now
			if result.err != nil {
				health[entry.healthID] = current
				if result.authFailed {
					summary.AuthFailedKeys++
					reasonsByKey[entry.key] = fmt.Sprintf("dahl maintenance: key unauthorized: %v", result.err)
					continue
				}
				if errors.Is(result.err, errDahlBalanceExhausted) {
					summary.LowBalanceKeys++
					current.Balance = 0
					health[entry.healthID] = current
					reasonsByKey[entry.key] = "dahl maintenance: available tokens exhausted"
					continue
				}
				summary.BalanceErrors++
				if firstBalanceError == "" {
					firstBalanceError = result.err.Error()
				}
				continue
			}

			// A lower balance than the previous observation is the cheapest
			// reliable "this key did real work" signal: dahl returns integer
			// tokens and every successful inference deducts at least one.
			if entry.health.CheckedAt > 0 && result.balance < entry.health.Balance {
				current.LastUsed = now
			}
			current.Balance = result.balance
			health[entry.healthID] = current
			if result.balance < cfg.disableThreshold {
				summary.LowBalanceKeys++
				reasonsByKey[entry.key] = fmt.Sprintf("dahl maintenance: balance %d below threshold %d", result.balance, cfg.disableThreshold)
			}
		}
	}
	if firstBalanceError != "" {
		common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d balance check failed: %s", channel.Id, firstBalanceError))
	}

	// Idle probes are deliberately a second phase: only a key with a known,
	// sufficient balance is worth a real completion, and a key just marked for
	// disabling must not be probed.
	probeModel := dahlProbeModel(channel, cfg.probeModel)
	probeEntries := make([]dahlKeyEntry, 0)
	maxProbes := cfg.maxProbes
	if maxProbes <= 0 {
		// Clear an entire idle cohort in roughly two days while keeping the
		// steady-state pass tiny. A 1.2k pool probes ~25 keys/hour; a 10k pool
		// probes ~209/hour, still far below dahl's rate limits.
		maxProbes = min(max((enabledCount+47)/48, 30), 500)
	}
	seenProbes := make(map[string]struct{}, maxProbes)
	if cfg.probeEnabled && probeModel != "" && maxProbes > 0 {
		for index, rawKey := range keys {
			key := strings.TrimSpace(rawKey)
			if key == "" || dahlMultiKeyStatus(channel, index) != common.ChannelStatusEnabled {
				continue
			}
			if _, disabling := reasonsByKey[key]; disabling {
				continue
			}
			healthID := dahlKeyHealthID(key)
			if _, duplicate := seenProbes[healthID]; duplicate {
				continue
			}
			current := health[healthID]
			if dahlShouldProbe(current, now, cfg.idle, cfg.probeRetryEvery, cfg.disableThreshold) {
				seenProbes[healthID] = struct{}{}
				probeEntries = append(probeEntries, dahlKeyEntry{
					key:      key,
					healthID: healthID,
					health:   current,
				})
			}
		}
		sort.Slice(probeEntries, func(i, j int) bool {
			left := probeEntries[i].health
			right := probeEntries[j].health
			if left.LastUsed != right.LastUsed {
				return left.LastUsed < right.LastUsed
			}
			return left.Balance > right.Balance
		})
		if len(probeEntries) > maxProbes {
			probeEntries = probeEntries[:maxProbes]
		}
	}

	firstProbeError := ""
	if len(probeEntries) > 0 {
		results := runDahlProbes(ctx, client, baseURL, probeModel, probeEntries, cfg, func(processed int) {
			if report != nil {
				report(processed, len(probeEntries))
			}
		})
		for _, result := range results {
			summary.IdleProbes++
			entry := result.entry
			current := health[entry.healthID]
			current.LastProbe = now
			switch {
			case result.ok:
				summary.ProbeSucceeded++
				current.LastUsed = now
			case result.exhausted:
				summary.ProbeFailed++
				current.Balance = 0
				reasonsByKey[entry.key] = "dahl maintenance: probe returned available tokens exhausted"
			case result.authFailed:
				summary.ProbeFailed++
				summary.AuthFailedKeys++
				reasonsByKey[entry.key] = fmt.Sprintf("dahl maintenance: probe key unauthorized: %v", result.err)
			default:
				summary.ProbeErrors++
				if firstProbeError == "" && result.err != nil {
					firstProbeError = result.err.Error()
				}
			}
			health[entry.healthID] = current
		}
	}
	if firstProbeError != "" {
		common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d probe failed: %s", channel.Id, firstProbeError))
	}

	if len(reasonsByKey) > 0 {
		disabled, err := model.AutoDisableChannelKeys(channel.Id, reasonsByKey)
		if err != nil {
			summary.DisableErrors += len(reasonsByKey)
			common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d disable failed: %v", channel.Id, err))
		} else {
			summary.DisabledKeys += disabled
		}
	}

	// Keep only snapshots that still belong to an enabled key. Stale hashes are
	// removed here instead of accumulating forever after key edits.
	if fresh, err := model.GetChannelById(channel.Id, false); err == nil {
		channel = fresh
	}
	live := make(map[string]struct{}, enabledCount)
	for index, rawKey := range keys {
		key := strings.TrimSpace(rawKey)
		if key == "" || dahlMultiKeyStatus(channel, index) != common.ChannelStatusEnabled {
			continue
		}
		if _, disabledNow := reasonsByKey[key]; disabledNow {
			continue
		}
		live[dahlKeyHealthID(key)] = struct{}{}
	}
	for healthID := range health {
		if _, ok := live[healthID]; !ok {
			delete(health, healthID)
		}
	}
	if err := model.UpdateChannelKeyHealth(channel.Id, health); err != nil {
		summary.HealthWriteErrors++
		common.SysError(fmt.Sprintf("dahl key maintenance: channel #%d health write failed: %v", channel.Id, err))
	}

	common.SysLog(fmt.Sprintf(
		"dahl key maintenance: channel #%d enabled=%d checked=%d low=%d disabled=%d probes=%d ok=%d probe_errors=%d balance_errors=%d",
		channel.Id, enabledCount, summary.BalanceChecked-start.BalanceChecked, summary.LowBalanceKeys-start.LowBalanceKeys,
		summary.DisabledKeys-start.DisabledKeys, summary.IdleProbes-start.IdleProbes,
		summary.ProbeSucceeded-start.ProbeSucceeded, summary.ProbeErrors-start.ProbeErrors,
		summary.BalanceErrors-start.BalanceErrors,
	))
}

func runDahlBalanceChecks(ctx context.Context, client *http.Client, baseURL string, entries []dahlKeyEntry, cfg dahlMaintenanceConfig, progress func(processed int)) []dahlBalanceCheckResult {
	concurrency := min(max(cfg.scanConcurrency, 1), len(entries))
	jobs := make(chan dahlKeyEntry)
	results := make(chan dahlBalanceCheckResult, len(entries))

	var workers sync.WaitGroup
	for range concurrency {
		workers.Go(func() {
			for entry := range jobs {
				results <- checkDahlKeyBalance(ctx, client, baseURL, entry, cfg.requestTimeout)
			}
		})
	}
	go func() {
		workers.Wait()
		close(results)
	}()
	go func() {
		defer close(jobs)
		for _, entry := range entries {
			select {
			case jobs <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()

	collected := make([]dahlBalanceCheckResult, 0, len(entries))
	for result := range results {
		collected = append(collected, result)
		if progress != nil {
			progress(len(collected))
		}
	}
	return collected
}

func runDahlProbes(ctx context.Context, client *http.Client, baseURL string, probeModel string, entries []dahlKeyEntry, cfg dahlMaintenanceConfig, progress func(processed int)) []dahlProbeResult {
	concurrency := min(max(cfg.probeConcurrency, 1), len(entries))
	jobs := make(chan dahlKeyEntry)
	results := make(chan dahlProbeResult, len(entries))

	var workers sync.WaitGroup
	for range concurrency {
		workers.Go(func() {
			for entry := range jobs {
				results <- probeDahlKey(ctx, client, baseURL, probeModel, entry, cfg.probeMaxTokens, cfg.probeTimeout)
			}
		})
	}
	go func() {
		workers.Wait()
		close(results)
	}()
	go func() {
		defer close(jobs)
		for _, entry := range entries {
			select {
			case jobs <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()

	collected := make([]dahlProbeResult, 0, len(entries))
	for result := range results {
		collected = append(collected, result)
		if progress != nil {
			progress(len(collected))
		}
	}
	return collected
}

func checkDahlKeyBalance(ctx context.Context, client *http.Client, baseURL string, entry dahlKeyEntry, timeout time.Duration) dahlBalanceCheckResult {
	result := dahlBalanceCheckResult{entry: entry}
	const attempts = 2
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if !sleepWithContext(ctx, time.Duration(attempt)*500*time.Millisecond) {
				break
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+dahlBalancePath, nil)
		if err != nil {
			cancel()
			result.err = err
			return result
		}
		req.Header.Set("Authorization", "Bearer "+entry.key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if ctx.Err() != nil {
				result.err = ctx.Err()
				return result
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			balance, err := parseDahlAvailableTokens(body)
			if err != nil {
				result.err = err
				return result
			}
			result.balance = balance
			return result
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			result.authFailed = true
			result.err = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			return result
		case resp.StatusCode == http.StatusPaymentRequired:
			result.err = errDahlBalanceExhausted
			return result
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
			lastErr = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			continue
		default:
			result.err = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			return result
		}
	}
	result.err = lastErr
	if result.err == nil {
		result.err = errors.New("balance check failed")
	}
	return result
}

func probeDahlKey(ctx context.Context, client *http.Client, baseURL string, probeModel string, entry dahlKeyEntry, maxTokens int, timeout time.Duration) dahlProbeResult {
	result := dahlProbeResult{entry: entry}
	body, err := common.Marshal(map[string]any{
		"model": probeModel,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	if err != nil {
		result.err = err
		return result
	}

	const attempts = 2
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if !sleepWithContext(ctx, time.Duration(attempt)*500*time.Millisecond) {
				break
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, baseURL+dahlChatPath, bytes.NewReader(body))
		if err != nil {
			cancel()
			result.err = err
			return result
		}
		req.Header.Set("Authorization", "Bearer "+entry.key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "new-api-dahl-key-maintenance/1.0")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			if ctx.Err() != nil {
				result.err = ctx.Err()
				return result
			}
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			var decoded struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := common.Unmarshal(payload, &decoded); err == nil && decoded.Error != nil && strings.TrimSpace(decoded.Error.Message) != "" {
				result.err = fmt.Errorf("probe returned an error: %s", decoded.Error.Message)
				return result
			}
			result.ok = true
			return result
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			result.authFailed = true
			result.err = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			return result
		case resp.StatusCode == http.StatusPaymentRequired:
			result.exhausted = true
			result.err = errDahlBalanceExhausted
			return result
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
			lastErr = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			continue
		default:
			result.err = fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
			return result
		}
	}
	result.err = lastErr
	if result.err == nil {
		result.err = errors.New("probe failed")
	}
	return result
}

func parseDahlAvailableTokens(body []byte) (int64, error) {
	var payload struct {
		AvailableTokens *int64 `json:"available_tokens"`
	}
	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode dahl balance response: %w", err)
	}
	if payload.AvailableTokens == nil {
		return 0, errors.New("dahl balance response is missing available_tokens")
	}
	return *payload.AvailableTokens, nil
}

func dahlKeyHealthID(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:8])
}

func dahlBalanceDue(health model.ChannelKeyHealth, now int64, interval time.Duration, threshold int64) bool {
	if health.CheckedAt == 0 {
		return true
	}
	// Keys close to the threshold are checked on every pass. This is the only
	// way to make the "disable below 500k" guarantee meaningful without
	// scanning the whole pool every hour.
	if health.Balance < threshold*2 {
		return true
	}
	return now-health.CheckedAt >= int64(interval.Seconds())
}

func dahlBalancePriority(health model.ChannelKeyHealth, threshold int64) int {
	if health.CheckedAt > 0 && health.Balance < threshold*2 {
		return 0
	}
	if health.CheckedAt == 0 {
		return 1
	}
	return 2
}

func dahlShouldProbe(health model.ChannelKeyHealth, now int64, idle time.Duration, retry time.Duration, threshold int64) bool {
	if health.CheckedAt == 0 || health.Balance < threshold {
		return false
	}
	if health.LastProbe > 0 && now-health.LastProbe < int64(retry.Seconds()) {
		return false
	}
	if health.LastUsed == 0 {
		return true
	}
	return now-health.LastUsed >= int64(idle.Seconds())
}

func dahlMultiKeyStatus(channel *model.Channel, index int) int {
	if channel.ChannelInfo.MultiKeyStatusList == nil {
		return common.ChannelStatusEnabled
	}
	if status, ok := channel.ChannelInfo.MultiKeyStatusList[index]; ok {
		return status
	}
	return common.ChannelStatusEnabled
}

func isDahlMaintenanceChannel(channel *model.Channel, channelIDs map[int]struct{}) bool {
	if len(channelIDs) > 0 {
		_, ok := channelIDs[channel.Id]
		return ok
	}
	return strings.Contains(strings.ToLower(channel.GetBaseURL()), "inference.dahl.global")
}

// dahlProbeModel prefers an explicit model and otherwise picks the configured
// MiniMax M2.7 model, then applies the channel's model mapping. Falling back
// to the first configured model keeps the probe usable on other channel
// layouts.
func dahlProbeModel(channel *model.Channel, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	models := channel.GetModels()
	if len(models) == 0 {
		return ""
	}
	mapping := map[string]string{}
	if raw := channel.GetModelMapping(); raw != "" {
		_ = common.UnmarshalJsonStr(raw, &mapping)
	}
	chosen := models[0]
	for _, candidate := range models {
		candidate = strings.TrimSpace(candidate)
		base := candidate
		if _, after, ok := strings.Cut(base, "/"); ok {
			base = after
		}
		if strings.EqualFold(base, dahlDefaultProbeModelHint) {
			chosen = candidate
			break
		}
	}
	if mapped := strings.TrimSpace(mapping[chosen]); mapped != "" {
		return mapped
	}
	return chosen
}

func loadDahlMaintenanceConfig() dahlMaintenanceConfig {
	idleDays := min(max(envInt("DAHL_KEY_IDLE_DAYS", dahlDefaultIdleDays), 1), 30)
	balanceCheckMinutes := min(max(envInt("DAHL_KEY_BALANCE_CHECK_INTERVAL_MINUTES", dahlDefaultBalanceCheckMinutes), 60), 7*24*60)
	probeRetryMinutes := min(max(envInt("DAHL_KEY_PROBE_RETRY_MINUTES", dahlDefaultProbeRetryMinutes), 60), 7*24*60)
	cfg := dahlMaintenanceConfig{
		channelIDs:        parseDahlChannelIDs(common.GetEnvOrDefaultString("DAHL_KEY_MAINTENANCE_CHANNEL_IDS", "")),
		disableThreshold:  envInt64("DAHL_KEY_DISABLE_THRESHOLD_TOKENS", dahlDefaultDisableThreshold),
		idle:              time.Duration(idleDays) * 24 * time.Hour,
		balanceCheckEvery: time.Duration(balanceCheckMinutes) * time.Minute,
		probeRetryEvery:   time.Duration(probeRetryMinutes) * time.Minute,
		maxBalanceChecks:  min(max(envInt("DAHL_KEY_MAX_BALANCE_CHECKS_PER_RUN", 0), 0), 5000),
		maxProbes:         min(max(envInt("DAHL_KEY_MAX_PROBES_PER_RUN", dahlDefaultMaxProbesPerRun), 0), 500),
		probeEnabled:      common.GetEnvOrDefaultBool("DAHL_KEY_PROBE_ENABLED", true),
		scanConcurrency:   envInt("DAHL_KEY_SCAN_CONCURRENCY", dahlDefaultScanConcurrency),
		probeConcurrency:  envInt("DAHL_KEY_PROBE_CONCURRENCY", dahlDefaultProbeConcurrency),
		probeModel:        strings.TrimSpace(common.GetEnvOrDefaultString("DAHL_KEY_PROBE_MODEL", "")),
		probeMaxTokens:    envInt("DAHL_KEY_PROBE_MAX_TOKENS", dahlDefaultProbeMaxTokens),
		requestTimeout:    time.Duration(envInt("DAHL_KEY_REQUEST_TIMEOUT_SECONDS", dahlDefaultRequestTimeoutSeconds)) * time.Second,
		probeTimeout:      time.Duration(envInt("DAHL_KEY_PROBE_TIMEOUT_SECONDS", dahlDefaultProbeTimeoutSeconds)) * time.Second,
	}
	cfg.disableThreshold = min(max(cfg.disableThreshold, 1), 1_000_000_000_000)
	cfg.idle = max(cfg.idle, 24*time.Hour)
	cfg.balanceCheckEvery = max(cfg.balanceCheckEvery, time.Hour)
	cfg.probeRetryEvery = max(cfg.probeRetryEvery, time.Hour)
	cfg.maxProbes = max(cfg.maxProbes, 0)
	cfg.scanConcurrency = min(max(cfg.scanConcurrency, 1), 16)
	cfg.probeConcurrency = min(max(cfg.probeConcurrency, 1), 8)
	cfg.probeMaxTokens = min(max(cfg.probeMaxTokens, 1), 128)
	cfg.requestTimeout = max(cfg.requestTimeout, 5*time.Second)
	cfg.probeTimeout = max(cfg.probeTimeout, 10*time.Second)
	return cfg
}

func parseDahlChannelIDs(raw string) map[int]struct{} {
	result := make(map[int]struct{})
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil || id <= 0 {
			common.SysError(fmt.Sprintf("dahl key maintenance: invalid channel id %q", part))
			continue
		}
		result[id] = struct{}{}
	}
	return result
}

func envInt(name string, fallback int) int {
	return common.GetEnvOrDefault(name, fallback)
}

func envInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString(name, ""))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		common.SysError(fmt.Sprintf("dahl key maintenance: invalid %s=%q, using %d", name, raw, fallback))
		return fallback
	}
	return value
}

func sleepWithContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
