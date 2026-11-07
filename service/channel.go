package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

func shouldCloseActiveWebSocketsAfterDisable(channelId int) bool {
	channel, err := model.GetChannelById(channelId, true)
	if err != nil {
		common.SysLog(common.LogText("failed to check channel status before closing active websockets: channel_id=%d, error=%v", channelId, err))
		return true
	}
	return channel.Status != common.ChannelStatusEnabled
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	DisableChannelUntil(channelError, reason, 0)
}

// ===== CUSTOM START: 临时禁用支持（until>0 到期自动恢复）=====
func DisableChannelUntil(channelError types.ChannelError, reason string, until int64) {
	common.SysLog(common.LogText("channel %q (#%d) failed, disabling it, reason: %s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(common.LogText("channel %q (#%d) has automatic disabling turned off, skipping", channelError.ChannelName, channelError.ChannelId))
		return
	}

	var success bool
	if until > 0 {
		changed, err := model.DisableChannelKeyUntil(channelError.ChannelId, channelError.UsingKey, reason, until)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to temporarily disable channel #%d: %v", channelError.ChannelId, err))
		}
		success = changed
	} else {
		success = model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	}
	if success {
		if shouldCloseActiveWebSocketsAfterDisable(channelError.ChannelId) {
			CloseActiveWebSocketsForChannel(channelError.ChannelId, ChannelDisabledCloseReason)
		}
		params := map[string]any{"Name": channelError.ChannelName, "Id": channelError.ChannelId, "Reason": reason}
		NotifyChannelWatchers(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), func(lang string) (string, string) {
			subject := i18n.Translate(lang, i18n.MsgChannelNotifyDisabledSubject, params)
			content := i18n.Translate(lang, i18n.MsgChannelNotifyDisabledContent, params)
			// 临时禁用时在通知里附带预计恢复时间
			if until > 0 {
				content = fmt.Sprintf("%s，预计恢复时间：%s", content, time.Unix(until, 0).Format(time.RFC3339))
			}
			return subject, content
		})
	}
}

// ===== CUSTOM END =====

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		params := map[string]any{"Name": channelName, "Id": channelId}
		NotifyChannelWatchers(formatNotifyType(channelId, common.ChannelStatusEnabled), func(lang string) (string, string) {
			return i18n.Translate(lang, i18n.MsgChannelNotifyEnabledSubject, params), i18n.Translate(lang, i18n.MsgChannelNotifyEnabledContent, params)
		})
	}
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	// Explicit provider-scoped disable signals are independent of the global
	// status-code/keyword auto-disable switch.
	if types.IsChannelAutoDisableError(err) {
		return true
	}
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	keywords := operation_setting.AutomaticDisableKeywords
	lowerMessage := strings.ToLower(err.Error())
	if search, _ := AcSearch(lowerMessage, keywords, true); search {
		return true
	}
	// Upstreams answering with a non-JSON body degrade err.Error() to a generic
	// "bad response status code N" that carries no keyword at all. Fall back to the
	// captured upstream body before concluding the channel must stay enabled.
	if body := err.UpstreamBody(); body != "" {
		search, _ := AcSearch(strings.ToLower(body), keywords, true)
		return search
	}
	return false
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
