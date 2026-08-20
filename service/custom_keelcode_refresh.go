package service

// ===== CUSTOM START: keelcode token 自动续期 =====
// keelcode 用 better-auth 的 device authorization 插件，approve 端点不在 captcha
// 插件的保护列表里，并接受 `Authorization: Bearer <access_token>` 作为身份。
// 于是一把未过期的旧 token 本身就是续期凭据，整条链路只要 4 个 HTTP 请求：
//
//	1. POST /api/auth/device/code           → device_code + user_code
//	2. GET  /api/auth/device?user_code=...  → 用旧 token 认领（claim）该 user_code
//	3. POST /api/auth/device/approve        → 用旧 token 批准
//	4. POST /api/auth/device/token          → 换出新 access_token（约 7 天）
//
// 无浏览器、无 Turnstile、无代理池，单把 key 约 2~5 秒。前提是旧 token 尚未过期；
// 一旦过期这条链就断了，只能回到浏览器注册流程重新取 token。
// 移植自 cloudflare-register 的 refresh_token_via_api。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	keelcodeDefaultAPIBase    = "https://api.keelcode.ai"
	keelcodeDefaultOrigin     = "https://keelcode.ai"
	keelcodeDeviceClientID    = "keelcode-cli"
	keelcodeDeviceScope       = "inference models usage"
	keelcodeDefaultChannelIDs = "10824,10825,10826,10827,10828,10829,10830"
	// keelcode token 寿命约 7 天，提前 2 天续期，留出新旧 key 的重叠窗口。
	keelcodeDefaultLifetimeSec  = 604799
	keelcodeDefaultLeadHours    = 48
	keelcodeRequestTimeout      = 30 * time.Second
	keelcodeExchangeTimeout     = 30 * time.Second
	keelcodeExchangePollBackoff = 2 * time.Second
	keelcodeMaxResponseBytes    = 1 << 20
)

// errKeelcodeTokenExpired 表示旧 token 已被上游拒绝（401/403），纯 HTTP 续期
// 这条路已经断了，必须回到浏览器流程重新注册取 token。
var errKeelcodeTokenExpired = errors.New("keelcode access token rejected by upstream")

type KeelcodeTokenRefreshSummary struct {
	Channels       int    `json:"channels"`
	KeysScanned    int    `json:"keys_scanned"`
	KeysDue        int    `json:"keys_due"`
	KeysRefreshed  int    `json:"keys_refreshed"`
	KeysReplaced   int    `json:"keys_replaced"`
	ExpiredKeys    int    `json:"expired_keys"`
	RefreshErrors  int    `json:"refresh_errors"`
	ChannelErrors  int    `json:"channel_errors"`
	PrunedRecords  int    `json:"pruned_records"`
	LeadTimeHours  int    `json:"lead_time_hours"`
	FirstErrorText string `json:"first_error,omitempty"`
}

type keelcodeDeviceCodeResponse struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
}

type keelcodeTokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RunKeelcodeTokenRefresh 扫描配置的 keelcode 渠道，把 lead 时间内即将过期的
// key 用旧 token 换成新 token，并在渠道 key 列表里原地替换。
//
// 台账里没有记录的 key（首次运行、手工添加）一律视为「到期」并立即续一次：
// 续期对旧 token 无副作用（旧 token 仍有效到自身过期），换来的新 token 带回
// 准确的 expires_in，一轮就把台账 bootstrap 起来。
func RunKeelcodeTokenRefresh(ctx context.Context, report func(processed, total int)) (KeelcodeTokenRefreshSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	channelIDs := keelcodeChannelIDs()
	leadHours := keelcodeLeadHours()
	summary := KeelcodeTokenRefreshSummary{
		Channels:      len(channelIDs),
		LeadTimeHours: leadHours,
	}
	if len(channelIDs) == 0 {
		if report != nil {
			report(0, 0)
		}
		return summary, nil
	}

	apiBase := strings.TrimRight(common.GetEnvOrDefaultString("KEELCODE_API_BASE", keelcodeDefaultAPIBase), "/")
	origin := strings.TrimRight(common.GetEnvOrDefaultString("KEELCODE_ORIGIN", keelcodeDefaultOrigin), "/")
	deadline := time.Now().Add(time.Duration(leadHours) * time.Hour).Unix()

	for index, channelID := range channelIDs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if report != nil {
			report(index, len(channelIDs))
		}
		if err := refreshKeelcodeChannel(ctx, channelID, apiBase, origin, deadline, &summary); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return summary, ctxErr
			}
			summary.ChannelErrors++
			recordKeelcodeError(&summary, fmt.Sprintf("channel #%d: %v", channelID, err))
			common.SysError(fmt.Sprintf("keelcode token refresh failed for channel #%d: %v", channelID, err))
		}
	}
	if report != nil {
		report(len(channelIDs), len(channelIDs))
	}
	return summary, nil
}

func refreshKeelcodeChannel(ctx context.Context, channelID int, apiBase string, origin string, deadline int64, summary *KeelcodeTokenRefreshSummary) error {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	records, err := model.GetKeelcodeTokensByChannel(channelID)
	if err != nil {
		return err
	}

	client, err := GetHttpClientWithProxy(channel.GetSetting().Proxy)
	if err != nil {
		return err
	}

	keys := channel.GetKeys()
	replacements := make(map[string]string)
	refreshed := make(map[string]keelcodeTokenResponse)
	for _, rawKey := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		summary.KeysScanned++

		if record, ok := records[key]; ok && record.ExpiresAt > deadline {
			continue
		}
		summary.KeysDue++

		token, refreshErr := refreshKeelcodeToken(ctx, client, apiBase, origin, key)
		if refreshErr != nil {
			if errors.Is(refreshErr, errKeelcodeTokenExpired) {
				summary.ExpiredKeys++
			} else {
				summary.RefreshErrors++
			}
			recordKeelcodeError(summary, fmt.Sprintf("channel #%d key %s: %v", channelID, keelcodePreview(key), refreshErr))
			common.SysError(fmt.Sprintf("keelcode token refresh failed for channel #%d key %s: %v", channelID, keelcodePreview(key), refreshErr))
			if markErr := model.MarkKeelcodeTokenError(key, refreshErr.Error()); markErr != nil {
				common.SysError(fmt.Sprintf("keelcode token refresh failed to record error for channel #%d: %v", channelID, markErr))
			}
			continue
		}

		summary.KeysRefreshed++
		replacements[key] = token.AccessToken
		refreshed[key] = token
	}

	if len(replacements) > 0 {
		replaced, replaceErr := model.ReplaceChannelKeysInPlace(channelID, replacements)
		if replaceErr != nil {
			// 新 token 已经签发但没能写进渠道：旧 token 仍在重叠有效期内，
			// 渠道不会中断，下一轮会重新续一次。
			return replaceErr
		}
		summary.KeysReplaced += replaced
		persistKeelcodeRecords(channelID, replacements, refreshed, records, summary)
	}

	// 台账清理放在替换之后，用替换后的 key 列表做基准。
	current, err := model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	pruned, err := model.PruneKeelcodeTokens(channelID, keelcodeTrimmedKeys(current.GetKeys()))
	if err != nil {
		return err
	}
	summary.PrunedRecords += pruned
	return nil
}

// persistKeelcodeRecords 在渠道 key 替换成功后建档新 token。
// 建档失败不影响渠道可用性：下一轮扫描会把这把未知 token 当成到期再续一次。
func persistKeelcodeRecords(
	channelID int,
	replacements map[string]string,
	refreshed map[string]keelcodeTokenResponse,
	previous map[string]*model.KeelcodeToken,
	summary *KeelcodeTokenRefreshSummary,
) {
	now := common.GetTimestamp()
	for oldKey, newKey := range replacements {
		token := refreshed[oldKey]
		refreshCount := 1
		if record, ok := previous[oldKey]; ok {
			refreshCount = record.RefreshCount + 1
		}
		err := model.UpsertKeelcodeToken(model.KeelcodeToken{
			ChannelId:    channelID,
			AccessToken:  newKey,
			ObtainedAt:   now,
			ExpiresAt:    now + token.ExpiresIn,
			RefreshCount: refreshCount,
		})
		if err != nil {
			recordKeelcodeError(summary, fmt.Sprintf("channel #%d: persist token failed: %v", channelID, err))
			common.SysError(fmt.Sprintf("keelcode token refresh failed to persist token for channel #%d: %v", channelID, err))
		}
	}
}

// refreshKeelcodeToken 用一把未过期的旧 token 换出新 token。
func refreshKeelcodeToken(ctx context.Context, client *http.Client, apiBase string, origin string, oldToken string) (keelcodeTokenResponse, error) {
	var deviceCode keelcodeDeviceCodeResponse
	err := keelcodeRequest(ctx, client, http.MethodPost, apiBase+"/api/auth/device/code", "", origin, map[string]any{
		"client_id": keelcodeDeviceClientID,
		"scope":     keelcodeDeviceScope,
	}, &deviceCode)
	if err != nil {
		return keelcodeTokenResponse{}, fmt.Errorf("request device code: %w", err)
	}
	if deviceCode.DeviceCode == "" || deviceCode.UserCode == "" {
		return keelcodeTokenResponse{}, errors.New("device code response is missing device_code/user_code")
	}

	// 认领 user_code：不认领直接 approve 会被拒。
	claimURL := apiBase + "/api/auth/device?user_code=" + url.QueryEscape(deviceCode.UserCode)
	if err := keelcodeRequest(ctx, client, http.MethodGet, claimURL, oldToken, origin, nil, nil); err != nil {
		return keelcodeTokenResponse{}, fmt.Errorf("claim user code: %w", err)
	}
	if err := keelcodeRequest(ctx, client, http.MethodPost, apiBase+"/api/auth/device/approve", oldToken, origin, map[string]any{
		"userCode": deviceCode.UserCode,
	}, nil); err != nil {
		return keelcodeTokenResponse{}, fmt.Errorf("approve device code: %w", err)
	}

	return exchangeKeelcodeDeviceToken(ctx, client, apiBase, origin, deviceCode.DeviceCode)
}

// exchangeKeelcodeDeviceToken 在 approve 之后短轮询换取新 access_token。
func exchangeKeelcodeDeviceToken(ctx context.Context, client *http.Client, apiBase string, origin string, deviceCode string) (keelcodeTokenResponse, error) {
	pollCtx, cancel := context.WithTimeout(ctx, keelcodeExchangeTimeout)
	defer cancel()

	lastDetail := "poll timed out"
	for {
		var token keelcodeTokenResponse
		err := keelcodeRequest(pollCtx, client, http.MethodPost, apiBase+"/api/auth/device/token", "", origin, map[string]any{
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
			"device_code": deviceCode,
			"client_id":   keelcodeDeviceClientID,
		}, &token)
		switch {
		case err == nil && token.AccessToken != "":
			if token.ExpiresIn <= 0 {
				token.ExpiresIn = keelcodeDefaultLifetimeSec
			}
			return token, nil
		case err == nil:
			lastDetail = "response is missing access_token"
		case errors.Is(err, errKeelcodeTokenExpired):
			return keelcodeTokenResponse{}, err
		default:
			lastDetail = err.Error()
			// authorization_pending / slow_down 属于正常轮询状态，继续等。
			if !keelcodeIsPendingError(err) {
				return keelcodeTokenResponse{}, fmt.Errorf("exchange device token: %w", err)
			}
		}

		select {
		case <-pollCtx.Done():
			return keelcodeTokenResponse{}, fmt.Errorf("exchange device token: %s", lastDetail)
		case <-time.After(keelcodeExchangePollBackoff):
		}
	}
}

// keelcodeRequest 发一个 keelcode API 请求。bearer 非空时带旧 token 作为身份；
// Origin 必须带上，approve 端点有 CSRF 校验（缺 Origin 会返回 MISSING_OR_NULL_ORIGIN）。
// out 非空时把响应体解码进去。
func keelcodeRequest(ctx context.Context, client *http.Client, method string, requestURL string, bearer string, origin string, body any, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, keelcodeRequestTimeout)
	defer cancel()

	var payload io.Reader
	if body != nil {
		encoded, err := common.Marshal(body)
		if err != nil {
			return err
		}
		payload = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(requestCtx, method, requestURL, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w (HTTP %d)", errKeelcodeTokenExpired, resp.StatusCode)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return keelcodeUpstreamError(resp)
	}
	if out == nil {
		return nil
	}
	if err := common.DecodeJson(io.LimitReader(resp.Body, keelcodeMaxResponseBytes), out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// keelcodeUpstreamError 把非 2xx 响应转成带 OAuth error code 的错误，
// 供轮询逻辑区分「还在等待批准」和「彻底失败」。
func keelcodeUpstreamError(resp *http.Response) error {
	var payload keelcodeTokenResponse
	if err := common.DecodeJson(io.LimitReader(resp.Body, keelcodeMaxResponseBytes), &payload); err != nil || payload.Error == "" {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	if payload.ErrorDescription == "" {
		return fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, payload.Error)
	}
	return fmt.Errorf("upstream returned HTTP %d: %s (%s)", resp.StatusCode, payload.Error, payload.ErrorDescription)
}

func keelcodeIsPendingError(err error) bool {
	message := err.Error()
	return strings.Contains(message, "authorization_pending") || strings.Contains(message, "slow_down")
}

func keelcodeTrimmedKeys(keys []string) []string {
	trimmed := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" {
			trimmed = append(trimmed, key)
		}
	}
	return trimmed
}

// keelcodePreview 只暴露 key 前缀，日志与任务详情里不落完整密钥。
func keelcodePreview(key string) string {
	if len(key) <= 10 {
		return key
	}
	return key[:10] + "..."
}

func recordKeelcodeError(summary *KeelcodeTokenRefreshSummary, message string) {
	if summary.FirstErrorText == "" {
		summary.FirstErrorText = message
	}
}

func keelcodeChannelIDs() []int {
	raw := common.GetEnvOrDefaultString("KEELCODE_CHANNEL_IDS", keelcodeDefaultChannelIDs)
	ids := make([]int, 0, 8)
	seen := make(map[int]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil || id <= 0 {
			common.SysError(fmt.Sprintf("invalid channel id %q in KEELCODE_CHANNEL_IDS, skipped", part))
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func keelcodeLeadHours() int {
	hours := common.GetEnvOrDefault("KEELCODE_TOKEN_REFRESH_LEAD_HOURS", keelcodeDefaultLeadHours)
	if hours < 1 {
		return keelcodeDefaultLeadHours
	}
	return hours
}

// ===== CUSTOM END =====
