package dto

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/require"
)

func TestValidateErrorRewrite(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		settings  *ChannelOtherSettings
		expectErr string
	}{
		{
			name:     "nil settings",
			settings: nil,
		},
		{
			name:     "no rules",
			settings: &ChannelOtherSettings{},
		},
		{
			name: "valid rule",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match:      "is not supported by TokenPlan",
				StatusCode: 413,
				Message:    "请求体过大",
				Code:       "request_too_large",
				SkipRetry:  true,
			}}},
		},
		{
			name: "status code zero keeps upstream status",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match: "TokenPlan",
			}}},
		},
		{
			name: "empty match is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				StatusCode: 413,
			}}},
			expectErr: "match is required",
		},
		{
			name: "whitespace match is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match: "   ",
			}}},
			expectErr: "match is required",
		},
		{
			name: "status code below 400 is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match:      "TokenPlan",
				StatusCode: 200,
			}}},
			expectErr: "status_code must be 0 or between 400 and 599",
		},
		{
			name: "status code above 599 is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match:      "TokenPlan",
				StatusCode: 600,
			}}},
			expectErr: "status_code must be 0 or between 400 and 599",
		},
		{
			name: "overlong match is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match: strings.Repeat("a", ChannelErrorRewriteMatchLimit+1),
			}}},
			expectErr: "match is too long",
		},
		{
			name: "overlong message is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match:   "TokenPlan",
				Message: strings.Repeat("a", ChannelErrorRewriteMessageLimit+1),
			}}},
			expectErr: "message is too long",
		},
		{
			name: "overlong code is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{{
				Match: "TokenPlan",
				Code:  strings.Repeat("a", ChannelErrorRewriteCodeLimit+1),
			}}},
			expectErr: "code is too long",
		},
		{
			name: "duplicate match is rejected",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{
				{Match: "TokenPlan"},
				{Match: "tokenplan"},
			}},
			expectErr: "duplicates an earlier rule",
		},
		{
			name: "second rule reports its own index",
			settings: &ChannelOtherSettings{ErrorRewrite: []ChannelErrorRewriteRule{
				{Match: "TokenPlan"},
				{Match: ""},
			}},
			expectErr: "error_rewrite[1].match is required",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.settings.ValidateErrorRewrite()
			if tc.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectErr)
		})
	}
}

func TestErrorRewriteRulesSurviveSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	original := ChannelOtherSettings{
		DisableTaskPollingSleep: true,
		ErrorRewrite: []ChannelErrorRewriteRule{{
			Match:      "is not supported by TokenPlan",
			StatusCode: 413,
			Message:    "请求体过大（{body_kb} KB）",
			Code:       "request_too_large",
			SkipRetry:  true,
		}},
	}

	encoded, err := common.Marshal(original)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "error_rewrite")

	var decoded ChannelOtherSettings
	require.NoError(t, common.UnmarshalJsonStr(string(encoded), &decoded))
	require.Equal(t, original.ErrorRewrite, decoded.ErrorRewrite)
	require.True(t, decoded.DisableTaskPollingSleep)
}
