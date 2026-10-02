package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	atriaProbePath              = "/v1/chat/completions"
	atriaDefaultProbeModel      = "Atria-Dawn-Preview"
	atriaProbeTimeout           = 40 * time.Second
	atriaProbeBodyLimit         = 4 << 10
	atriaProbeMaxTokens         = 4
	atriaProbeIntervalDefaultMS = 3000
	atriaMaxProbesDefault       = 20
	// atriaRateLimitRetryDelay 是 429 假阴性的补测等待窗口。上游限流按分钟计，
	// 探活自己打出来的 429 越过这个窗口后往往就能通过，直接放弃会白丢好 key。
	atriaRateLimitRetryDelay = 70 * time.Second
)

// AtriaKeyRecoverySummary reports one atria key-pool recovery pass.
type AtriaKeyRecoverySummary struct {
	Channels          int  `json:"channels"`
	AutoDisabledKeys  int  `json:"auto_disabled_keys"`
	KeysProbed        int  `json:"keys_probed"`
	KeysEnabled       int  `json:"keys_enabled"`
	RateLimitedKeys   int  `json:"rate_limited_keys"`
	RetriedKeys       int  `json:"retried_keys"`
	RejectedKeys      int  `json:"rejected_keys"`
	UnavailableKeys   int  `json:"unavailable_keys"`
	IndeterminateKeys int  `json:"indeterminate_keys"`
	ChannelErrors     int  `json:"channel_errors"`
	EnableErrors      int  `json:"enable_errors"`
	Truncated         bool `json:"truncated"`
}

type atriaRecoveryCandidate struct {
	channel  *model.Channel
	key      string
	probeURL string
	model    string
	client   *http.Client
}

type atriaProbeVerdict int

const (
	atriaVerdictHealthy atriaProbeVerdict = iota
	atriaVerdictRateLimited
	// atriaVerdictRejected 是上游说 key 本身无效（401/403），重试没有意义。
	atriaVerdictRejected
	// atriaVerdictUnavailable 覆盖其它非 2xx：可能是模型名配错了，也可能
	// 只是上游抖动，一律保守不启用。
	atriaVerdictUnavailable
	// atriaVerdictIndeterminate 表示根本没拿到响应（DNS、连接、超时），
	// 它对 key 的健康状况没有任何信息量。
	atriaVerdictIndeterminate
)

type atriaProbeResult struct {
	verdict atriaProbeVerdict
	detail  string
}

// RunAtriaKeyRecovery re-enables atria keys that the upstream is willing to
// serve again but that are still sitting in the auto-disabled state.
//
// 背景：atria 的 429 大多是每分钟限流或临时配额（`Token quota exhausted.` 只是
// 网关对 429 的通用标签），被自动摘掉的 key 里相当一部分过一会儿就又能用了。
// 但请求路径触发的自动禁用不带 multi_key_disabled_until，自愈扫描碰不到它，
// 池子只会单调萎缩。
//
// 本任务刻意做成「只启用、从不禁用」，并且只探活当前 status=3 的 key：
//   - 全池探活会打上百次上游请求，反而把渠道自己打成 429（实测会污染指标）；
//   - 启用错了几乎没有代价（真实流量会在几分钟内把坏 key 再摘掉，这是自愈），
//     误禁用却有代价（key 要等到下一轮才可能被放回来）。
//
// 200 才启用；401/403 视为 key 本身已死；429 先记账、越过限流窗口后补测一次。
// 手动禁用（status=2）的 key 永远不碰，还在自愈冷却期（until 未到期）的也不碰。
func RunAtriaKeyRecovery(ctx context.Context, report func(processed, total int)) (AtriaKeyRecoverySummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	summary := AtriaKeyRecoverySummary{}

	channels, err := model.GetChannelsWithKeys()
	if err != nil {
		return summary, err
	}

	now := common.GetTimestamp()
	candidates := make([]atriaRecoveryCandidate, 0)
	for _, channel := range channels {
		if !IsAtriaChannel(channel) {
			continue
		}
		summary.Channels++
		if !channel.ChannelInfo.IsMultiKey || !channel.GetAutoBan() {
			continue
		}

		keys := channel.GetKeys()
		pending := make([]string, 0)
		for index, key := range keys {
			if channel.ChannelInfo.MultiKeyStatusList[index] != common.ChannelStatusAutoDisabled {
				continue
			}
			// 还在自愈冷却期里的 key 交给原机制到期回收，别提前放回来。
			if until, ok := channel.ChannelInfo.MultiKeyDisabledUntil[index]; ok && until > now {
				continue
			}
			if key = strings.TrimSpace(key); key == "" {
				continue
			}
			pending = append(pending, key)
		}
		summary.AutoDisabledKeys += len(pending)
		if len(pending) == 0 {
			continue
		}

		// 端点与 client 都是渠道级的，配置错一次只报一个渠道错误，
		// 而不是被误读成「这个渠道的 key 全坏了」。
		probeURL, err := atriaProbeURL(channel.GetBaseURL())
		if err != nil {
			summary.ChannelErrors++
			common.SysError(fmt.Sprintf("atria key recovery: probe URL unavailable for channel #%d: %v", channel.Id, err))
			continue
		}
		client, err := GetHttpClientWithProxy(channel.GetSetting().Proxy)
		if err != nil {
			summary.ChannelErrors++
			common.SysError(fmt.Sprintf("atria key recovery: probe client unavailable for channel #%d: %v", channel.Id, err))
			continue
		}
		probeModel := atriaProbeModel(channel)
		for _, key := range pending {
			candidates = append(candidates, atriaRecoveryCandidate{
				channel:  channel,
				key:      key,
				probeURL: probeURL,
				model:    probeModel,
				client:   client,
			})
		}
	}

	// 没有待回收的 key 就立刻结束：先探活再判断会白打几十次上游请求。
	if len(candidates) == 0 {
		if report != nil {
			report(0, 0)
		}
		return summary, nil
	}

	if maxProbes := atriaMaxProbes(); len(candidates) > maxProbes {
		candidates = candidates[:maxProbes]
		summary.Truncated = true
	}
	summary.KeysProbed = len(candidates)

	// 串行 + 间隔，不并发：这一轮打的是同一批上游额度，突发只会自伤出 429，
	// 把本来健康的 key 判成限流。
	interval := atriaProbeInterval()
	enableByChannel := make(map[int][]string)
	rateLimited := make([]atriaRecoveryCandidate, 0)
	for index, job := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		result := probeAtriaKey(ctx, job)
		if report != nil {
			report(index+1, len(candidates))
		}
		switch result.verdict {
		case atriaVerdictHealthy:
			enableByChannel[job.channel.Id] = append(enableByChannel[job.channel.Id], job.key)
		case atriaVerdictRateLimited:
			summary.RateLimitedKeys++
			rateLimited = append(rateLimited, job)
		case atriaVerdictRejected:
			summary.RejectedKeys++
			common.SysLog(fmt.Sprintf("atria key recovery: channel #%d key index skipped: %s", job.channel.Id, result.detail))
		case atriaVerdictIndeterminate:
			summary.IndeterminateKeys++
		default:
			summary.UnavailableKeys++
			common.SysLog(fmt.Sprintf("atria key recovery: channel #%d key probe inconclusive: %s", job.channel.Id, result.detail))
		}
		if index < len(candidates)-1 {
			if err := sleepCtx(ctx, interval); err != nil {
				return summary, err
			}
		}
	}

	if len(rateLimited) > 0 && atriaRateLimitRetryEnabled() {
		common.SysLog(fmt.Sprintf("atria key recovery: %d key(s) rate limited, retrying after %s", len(rateLimited), atriaRateLimitRetryDelay))
		if err := sleepCtx(ctx, atriaRateLimitRetryDelay); err != nil {
			return summary, err
		}
		for index, job := range rateLimited {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			result := probeAtriaKey(ctx, job)
			summary.RetriedKeys++
			if result.verdict == atriaVerdictHealthy {
				summary.RateLimitedKeys--
				enableByChannel[job.channel.Id] = append(enableByChannel[job.channel.Id], job.key)
			}
			if index < len(rateLimited)-1 {
				if err := sleepCtx(ctx, interval); err != nil {
					return summary, err
				}
			}
		}
	}

	// 同一个上游 key 池常常挂在多个 atria 渠道上，而禁停状态是按渠道存的，
	// 所以判定先并成一份再逐渠道下发：不持有该 key 的渠道是空操作。
	merged := make(map[string]struct{})
	for _, keys := range enableByChannel {
		for _, key := range keys {
			merged[key] = struct{}{}
		}
	}
	if len(merged) == 0 {
		return summary, nil
	}
	keysToEnable := make([]string, 0, len(merged))
	for key := range merged {
		keysToEnable = append(keysToEnable, key)
	}
	for _, channel := range channels {
		if !IsAtriaChannel(channel) || !channel.ChannelInfo.IsMultiKey || !channel.GetAutoBan() {
			continue
		}
		held := 0
		for _, key := range channel.GetKeys() {
			if _, ok := merged[strings.TrimSpace(key)]; ok {
				held++
			}
		}
		if held == 0 {
			continue
		}
		// EnableAutoDisabledChannelKeys 只翻 status=3 那一档，人工禁用的绝不碰。
		enabled, err := model.EnableAutoDisabledChannelKeys(channel.Id, keysToEnable)
		if err != nil {
			summary.EnableErrors += held
			common.SysError(fmt.Sprintf("atria key recovery: failed to re-enable recovered keys in channel #%d: %v", channel.Id, err))
			continue
		}
		summary.KeysEnabled += enabled
	}

	return summary, nil
}

// IsAtriaChannel reports whether the channel points at the atria-asi upstream.
// The provider is reached through a custom channel type, so the base URL host is
// the only stable identifier — matching on it also picks up channels added later
// without another code change.
func IsAtriaChannel(channel *model.Channel) bool {
	parsed, err := url.Parse(strings.TrimSpace(channel.GetBaseURL()))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "atria-asi.ai" || strings.HasSuffix(host, ".atria-asi.ai")
}

// atriaProbeURL derives the probe endpoint from the channel base URL, which may
// point at either the host root or some deeper path.
func atriaProbeURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL %q has no scheme or host", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if index := strings.Index(path, "/v1"); index >= 0 {
		path = path[:index]
	}
	parsed.Path = path + atriaProbePath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// atriaProbeModel picks a model name the upstream accepts for probing: the
// environment override wins, then the channel's own model_mapping target (every
// atria model maps to the same upstream model), then the built-in default.
func atriaProbeModel(channel *model.Channel) string {
	if configured := strings.TrimSpace(common.GetEnvOrDefaultString("ATRIA_KEY_RECOVERY_PROBE_MODEL", "")); configured != "" {
		return configured
	}
	if raw := strings.TrimSpace(channel.GetModelMapping()); raw != "" {
		var mapping map[string]string
		if err := common.UnmarshalJsonStr(raw, &mapping); err == nil {
			targets := make([]string, 0, len(mapping))
			for _, target := range mapping {
				if target = strings.TrimSpace(target); target != "" {
					targets = append(targets, target)
				}
			}
			if len(targets) > 0 {
				sort.Strings(targets)
				return targets[0]
			}
		}
	}
	return atriaDefaultProbeModel
}

func probeAtriaKey(ctx context.Context, job atriaRecoveryCandidate) atriaProbeResult {
	requestCtx, cancel := context.WithTimeout(ctx, atriaProbeTimeout)
	defer cancel()

	payload, err := common.Marshal(map[string]any{
		"model": job.model,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
		"max_tokens": atriaProbeMaxTokens,
	})
	if err != nil {
		return atriaProbeResult{verdict: atriaVerdictIndeterminate, detail: fmt.Sprintf("marshal probe body: %v", err)}
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, job.probeURL, bytes.NewReader(payload))
	if err != nil {
		return atriaProbeResult{verdict: atriaVerdictIndeterminate, detail: fmt.Sprintf("probe request error: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+job.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := job.client.Do(req)
	if err != nil {
		return atriaProbeResult{verdict: atriaVerdictIndeterminate, detail: fmt.Sprintf("probe never reached the upstream: %v", err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, atriaProbeBodyLimit))
		_ = resp.Body.Close()
	}()

	// 只看状态码：atria 是 reasoning 模型，200 的 content 常为 null、答案在
	// reasoning_content 里，拿 body 判成败会误杀健康 key。
	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		return atriaProbeResult{verdict: atriaVerdictHealthy}
	case resp.StatusCode == http.StatusTooManyRequests:
		return atriaProbeResult{verdict: atriaVerdictRateLimited, detail: fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return atriaProbeResult{verdict: atriaVerdictRejected, detail: fmt.Sprintf("upstream rejected the key (HTTP %d)", resp.StatusCode)}
	default:
		return atriaProbeResult{verdict: atriaVerdictUnavailable, detail: fmt.Sprintf("probe returned HTTP %d", resp.StatusCode)}
	}
}

func atriaProbeInterval() time.Duration {
	ms := common.GetEnvOrDefault("ATRIA_KEY_RECOVERY_PROBE_INTERVAL_MS", atriaProbeIntervalDefaultMS)
	if ms < 0 {
		ms = atriaProbeIntervalDefaultMS
	}
	return time.Duration(ms) * time.Millisecond
}

func atriaMaxProbes() int {
	maxProbes := common.GetEnvOrDefault("ATRIA_KEY_RECOVERY_MAX_PROBES", atriaMaxProbesDefault)
	if maxProbes < 1 {
		maxProbes = atriaMaxProbesDefault
	}
	return maxProbes
}

func atriaRateLimitRetryEnabled() bool {
	return common.GetEnvOrDefaultBool("ATRIA_KEY_RECOVERY_RETRY_RATE_LIMITED", true)
}

func sleepCtx(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
