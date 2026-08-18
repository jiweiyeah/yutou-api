package service

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
)

const (
	DeepSeekFreeTierLimitCode types.ErrorCode = "free_tier_limit_reached"
	deepSeekFreeTierFallback                  = 7 * 24 * time.Hour
	deepSeekFreeTierMaxFuture                 = 8 * 24 * time.Hour
)

var deepSeekResetTimePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

// AnnotateDeepSeekFreeTierLimit marks the provider-specific exhaustion error
// for synchronous key disablement. It intentionally matches the structured
// error code instead of every HTTP 429 response.
func AnnotateDeepSeekFreeTierLimit(channelType int, err *types.NewAPIError) bool {
	if channelType != constant.ChannelTypeDeepSeek || err == nil || err.GetErrorCode() != DeepSeekFreeTierLimitCode {
		return false
	}

	now := time.Now()
	resetAt := parseDeepSeekResetTime(err.Error())
	if resetAt.IsZero() || !resetAt.After(now) || resetAt.After(now.Add(deepSeekFreeTierMaxFuture)) {
		resetAt = now.Add(deepSeekFreeTierFallback).Truncate(time.Minute)
	}
	types.ErrOptionWithChannelAutoDisableUntil(resetAt.Unix())(err)
	return true
}

func parseDeepSeekResetTime(message string) time.Time {
	match := deepSeekResetTimePattern.FindString(message)
	if match == "" {
		return time.Time{}
	}
	resetAt, err := time.Parse(time.RFC3339Nano, match)
	if err != nil {
		return time.Time{}
	}
	return resetAt
}

type DeepSeekFreeTierRecoverySummary struct {
	Channels      int `json:"channels"`
	KeysRecovered int `json:"keys_recovered"`
	Errors        int `json:"errors"`
}

func RunDeepSeekFreeTierRecovery(ctx context.Context, report func(processed, total int)) (DeepSeekFreeTierRecoverySummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	channels, err := model.GetChannelsByTypeWithKeys(constant.ChannelTypeDeepSeek)
	if err != nil {
		return DeepSeekFreeTierRecoverySummary{}, err
	}
	summary := DeepSeekFreeTierRecoverySummary{Channels: len(channels)}
	for index, channel := range channels {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if report != nil {
			report(index, len(channels))
		}
		recovered, recoverErr := model.RecoverExpiredChannelKeys(channel.Id, time.Now().Unix())
		if recoverErr != nil {
			summary.Errors++
			common.SysError(fmt.Sprintf("DeepSeek free-tier recovery failed for channel #%d: %v", channel.Id, recoverErr))
			continue
		}
		summary.KeysRecovered += recovered
		if recovered > 0 {
			common.SysLog(fmt.Sprintf("DeepSeek free-tier recovery enabled %d key(s) for channel #%d", recovered, channel.Id))
		}
	}
	if report != nil {
		report(len(channels), len(channels))
	}
	return summary, nil
}
