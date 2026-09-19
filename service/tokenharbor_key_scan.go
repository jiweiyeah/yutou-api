package service

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	tokenHarborProbePath          = "/v1/models"
	tokenHarborProbeTimeout       = 20 * time.Second
	tokenHarborDefaultConcurrency = 16
	tokenHarborProbeBodyLimit     = 8 << 10
	// tokenHarborDefaultDisableMaxRatio caps the share of a channel's probed
	// keys that may be disabled in one pass. Above it the upstream is assumed
	// to be having a bad day (auth outage, gateway meltdown) rather than the
	// keys having gone bad, so the pass only re-enables and never disables.
	tokenHarborDefaultDisableMaxRatio = 0.5
)

// TokenHarborKeyScanSummary reports one TokenHarbor key-pool health pass. The
// pass is authoritative for every key it probes: a key that answers the
// read-only /v1/models probe is enabled, one the upstream rejects is disabled,
// so the pool state always reflects the last probe rather than accumulated
// traffic history.
type TokenHarborKeyScanSummary struct {
	Channels         int  `json:"channels"`
	KeysChecked      int  `json:"keys_checked"`
	KeysEnabled      int  `json:"keys_enabled"`
	KeysDisabled     int  `json:"keys_disabled"`
	UnauthorizedKeys int  `json:"unauthorized_keys"`
	UnavailableKeys  int  `json:"unavailable_keys"`
	RequestErrors    int  `json:"request_errors"`
	SkippedKeys      int  `json:"skipped_keys"`
	DisableGuardHit  bool `json:"disable_guard_tripped"`
	EnableErrors     int  `json:"enable_errors"`
	DisableErrors    int  `json:"disable_errors"`
}

type tokenHarborProbe struct {
	channel *model.Channel
	key     string
	index   int
}

// tokenHarborVerdict is the probe outcome for a single key. Unauthorized and
// Unavailable are kept apart so the summary can tell a revoked key from a key
// the upstream could not serve right now.
type tokenHarborVerdict int

const (
	tokenHarborVerdictHealthy tokenHarborVerdict = iota
	tokenHarborVerdictUnauthorized
	tokenHarborVerdictUnavailable
)

type tokenHarborProbeResult struct {
	probe   tokenHarborProbe
	verdict tokenHarborVerdict
	detail  string
}

// RunTokenHarborKeyScan probes every key of every TokenHarbor channel with a
// read-only GET /v1/models call and rebuilds each key's enabled/disabled state
// from the result. It never consumes upstream quota, so it can cover the whole
// pool instead of waiting for real traffic to expose a dead key.
func RunTokenHarborKeyScan(ctx context.Context, report func(processed, total int)) (TokenHarborKeyScanSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	summary := TokenHarborKeyScanSummary{}

	channels, err := model.GetChannelsWithKeys()
	if err != nil {
		return summary, err
	}

	jobs := make([]tokenHarborProbe, 0)
	for _, channel := range channels {
		if !IsTokenHarborChannel(channel) {
			continue
		}
		summary.Channels++
		if !channel.GetAutoBan() {
			summary.SkippedKeys += len(channel.GetKeys())
			continue
		}
		for index, key := range channel.GetKeys() {
			key = strings.TrimSpace(key)
			if key == "" {
				summary.SkippedKeys++
				continue
			}
			jobs = append(jobs, tokenHarborProbe{channel: channel, key: key, index: index})
		}
	}

	summary.KeysChecked = len(jobs)
	if len(jobs) == 0 {
		if report != nil {
			report(0, 0)
		}
		return summary, nil
	}

	concurrency := common.GetEnvOrDefault("TOKENHARBOR_SCAN_CONCURRENCY", tokenHarborDefaultConcurrency)
	if concurrency < 1 {
		concurrency = tokenHarborDefaultConcurrency
	}
	if concurrency > 128 {
		concurrency = 128
	}
	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}

	jobCh := make(chan tokenHarborProbe, len(jobs))
	resultCh := make(chan tokenHarborProbeResult, len(jobs))
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
				resultCh <- probeTokenHarborKey(ctx, job)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(resultCh)
	}()

	checkedByChannel := make(map[int]int)
	enableByChannel := make(map[int][]string)
	disableReasonsByChannel := make(map[int]map[string]string)
	processed := 0
	for result := range resultCh {
		processed++
		if report != nil {
			report(processed, len(jobs))
		}
		channelID := result.probe.channel.Id
		checkedByChannel[channelID]++

		switch result.verdict {
		case tokenHarborVerdictHealthy:
			if !tokenHarborKeyIsEnabled(result.probe) {
				enableByChannel[channelID] = append(enableByChannel[channelID], result.probe.key)
			}
			continue
		case tokenHarborVerdictUnauthorized:
			summary.UnauthorizedKeys++
		default:
			summary.UnavailableKeys++
		}
		if disableReasonsByChannel[channelID] == nil {
			disableReasonsByChannel[channelID] = make(map[string]string)
		}
		disableReasonsByChannel[channelID][result.probe.key] = result.detail
	}

	if err := ctx.Err(); err != nil {
		return summary, err
	}

	maxRatio := tokenHarborDisableMaxRatio()
	for channelID, reasonsByKey := range disableReasonsByChannel {
		if tokenHarborDisableGuardTripped(len(reasonsByKey), checkedByChannel[channelID], maxRatio) {
			summary.DisableGuardHit = true
			common.SysError(fmt.Sprintf("TokenHarbor disable skipped for channel #%d: %d/%d probed keys failed, exceeding guard ratio %.2f", channelID, len(reasonsByKey), checkedByChannel[channelID], maxRatio))
			continue
		}
		disabled, err := model.AutoDisableChannelKeys(channelID, reasonsByKey)
		if err != nil {
			summary.DisableErrors += len(reasonsByKey)
			common.SysError(fmt.Sprintf("TokenHarbor failed to disable unhealthy keys in channel #%d: %v", channelID, err))
			continue
		}
		summary.KeysDisabled += disabled
	}

	for channelID, keys := range enableByChannel {
		enabled, err := model.EnableChannelKeys(channelID, keys)
		if err != nil {
			summary.EnableErrors += len(keys)
			common.SysError(fmt.Sprintf("TokenHarbor failed to re-enable healthy keys in channel #%d: %v", channelID, err))
			continue
		}
		summary.KeysEnabled += enabled
	}

	return summary, nil
}

// IsTokenHarborChannel reports whether the channel points at TokenHarbor. The
// provider is reached through the generic custom-channel type, so the base URL
// host is the only stable identifier — matching on it also picks up channels
// added later without another code change.
func IsTokenHarborChannel(channel *model.Channel) bool {
	parsed, err := url.Parse(strings.TrimSpace(channel.GetBaseURL()))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "tokenharbor.ai" || strings.HasSuffix(host, ".tokenharbor.ai")
}

func probeTokenHarborKey(ctx context.Context, job tokenHarborProbe) tokenHarborProbeResult {
	result := tokenHarborProbeResult{probe: job}

	probeURL, err := tokenHarborModelsURL(job.channel.GetBaseURL())
	if err != nil {
		result.verdict = tokenHarborVerdictUnavailable
		result.detail = fmt.Sprintf("TokenHarbor probe URL invalid: %v", err)
		return result
	}

	requestCtx, cancel := context.WithTimeout(ctx, tokenHarborProbeTimeout)
	defer cancel()

	client, err := GetHttpClientWithProxy(job.channel.GetSetting().Proxy)
	if err != nil {
		result.verdict = tokenHarborVerdictUnavailable
		result.detail = fmt.Sprintf("TokenHarbor probe client error: %v", err)
		return result
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		result.verdict = tokenHarborVerdictUnavailable
		result.detail = fmt.Sprintf("TokenHarbor probe request error: %v", err)
		return result
	}
	req.Header.Set("Authorization", "Bearer "+job.key)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		result.verdict = tokenHarborVerdictUnavailable
		result.detail = fmt.Sprintf("TokenHarbor probe failed: %v", err)
		return result
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, tokenHarborProbeBodyLimit))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		result.verdict = tokenHarborVerdictHealthy
	case resp.StatusCode == http.StatusUnauthorized:
		// 401 is the upstream saying the key itself is revoked, which is a
		// permanent condition until someone rotates it in the dashboard.
		result.verdict = tokenHarborVerdictUnauthorized
		result.detail = fmt.Sprintf("TokenHarbor key unauthorized (HTTP %d)", resp.StatusCode)
	default:
		result.verdict = tokenHarborVerdictUnavailable
		result.detail = fmt.Sprintf("TokenHarbor probe returned HTTP %d", resp.StatusCode)
	}
	return result
}

// tokenHarborModelsURL derives the read-only model list endpoint from the
// channel's base URL, which may point at either the host root or a full
// completions path (the TokenHarbor channels store ".../v1/chat/completions").
func tokenHarborModelsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL %q has no scheme or host", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if index := strings.Index(path, "/v1"); index >= 0 {
		path = path[:index] + "/v1"
	} else {
		path = "/v1"
	}
	parsed.Path = path + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// tokenHarborKeyIsEnabled reports whether the probed key currently takes
// traffic. A key missing from the status list counts as enabled, matching how
// the relay picks keys.
func tokenHarborKeyIsEnabled(probe tokenHarborProbe) bool {
	if !probe.channel.ChannelInfo.IsMultiKey {
		return probe.channel.Status == common.ChannelStatusEnabled
	}
	status, ok := probe.channel.ChannelInfo.MultiKeyStatusList[probe.index]
	return !ok || status == common.ChannelStatusEnabled
}

// tokenHarborDisableGuardTripped reports whether this pass should stop
// disabling keys for a channel: when most of the probed pool fails at once the
// upstream is the problem, not the keys.
func tokenHarborDisableGuardTripped(failed, checked int, maxRatio float64) bool {
	if failed == 0 || checked == 0 || maxRatio <= 0 {
		return false
	}
	return float64(failed) > float64(checked)*maxRatio
}

func tokenHarborDisableMaxRatio() float64 {
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("TOKENHARBOR_DISABLE_MAX_RATIO", strconv.FormatFloat(tokenHarborDefaultDisableMaxRatio, 'f', -1, 64)))
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		common.SysError(fmt.Sprintf("invalid TOKENHARBOR_DISABLE_MAX_RATIO=%q, using %.2f", raw, tokenHarborDefaultDisableMaxRatio))
		return tokenHarborDefaultDisableMaxRatio
	}
	return ratio
}
