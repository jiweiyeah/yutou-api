package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAtriaProbeURL(t *testing.T) {
	cases := map[string]string{
		"https://api.atria-asi.ai":                    "https://api.atria-asi.ai/v1/chat/completions",
		"https://api.atria-asi.ai/":                   "https://api.atria-asi.ai/v1/chat/completions",
		"https://api.atria-asi.ai/v1":                 "https://api.atria-asi.ai/v1/chat/completions",
		"https://api.atria-asi.ai/v1/chat/completions": "https://api.atria-asi.ai/v1/chat/completions",
		"https://api.atria-asi.ai/prefix/v1/models":   "https://api.atria-asi.ai/prefix/v1/chat/completions",
	}
	for input, want := range cases {
		got, err := atriaProbeURL(input)
		require.NoError(t, err, input)
		assert.Equal(t, want, got, input)
	}
}

func TestAtriaProbeURLRejectsHostlessBaseURL(t *testing.T) {
	_, err := atriaProbeURL("api.atria-asi.ai/v1")
	require.Error(t, err)
}

func TestIsAtriaChannelMatchesOnlyProviderHost(t *testing.T) {
	channelWithBaseURL := func(baseURL string) *model.Channel {
		value := baseURL
		return &model.Channel{BaseURL: &value}
	}

	assert.True(t, IsAtriaChannel(channelWithBaseURL("https://api.atria-asi.ai")))
	assert.True(t, IsAtriaChannel(channelWithBaseURL("https://api.atria-asi.ai/v1/chat/completions")))
	assert.True(t, IsAtriaChannel(channelWithBaseURL("https://ATRIA-ASI.AI")))

	assert.False(t, IsAtriaChannel(channelWithBaseURL("https://atria-asi.ai.evil.example/v1")))
	assert.False(t, IsAtriaChannel(channelWithBaseURL("https://tokenharbor.ai/v1")))
	assert.False(t, IsAtriaChannel(channelWithBaseURL("")))
}

func TestAtriaProbeModelPrefersOverrideThenMapping(t *testing.T) {
	mapping := `{"deepseek/deepseek-v4-flash":"Atria-Dawn-Preview","tencent/hy3":"Atria-Dawn-Preview"}`
	channel := &model.Channel{ModelMapping: &mapping}

	t.Setenv("ATRIA_KEY_RECOVERY_PROBE_MODEL", "Atria-Custom")
	assert.Equal(t, "Atria-Custom", atriaProbeModel(channel))

	t.Setenv("ATRIA_KEY_RECOVERY_PROBE_MODEL", "")
	assert.Equal(t, "Atria-Dawn-Preview", atriaProbeModel(channel))

	empty := ""
	assert.Equal(t, atriaDefaultProbeModel, atriaProbeModel(&model.Channel{ModelMapping: &empty}))
}

// atriaProbeRedirectTransport rewrites probe requests to a local test server so
// the recovery pass can be exercised without touching the real upstream.
type atriaProbeRedirectTransport struct {
	target *url.URL
}

func (transport atriaProbeRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = transport.target.Scheme
	cloned.URL.Host = transport.target.Host
	return http.DefaultTransport.RoundTrip(cloned)
}

// atriaProbeFailingTransport fails every request at the transport layer,
// simulating DNS/connect/timeout failures that never reach the upstream.
type atriaProbeFailingTransport struct{}

func (atriaProbeFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network failure")
}

func TestRunAtriaKeyRecoveryOnlyTouchesRecoverableAutoDisabledKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	oldDB := model.DB
	oldHTTPClient := httpClient
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	t.Cleanup(func() {
		model.DB = oldDB
		httpClient = oldHTTPClient
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Setenv("ATRIA_KEY_RECOVERY_PROBE_INTERVAL_MS", "0")
	t.Setenv("ATRIA_KEY_RECOVERY_RETRY_RATE_LIMITED", "false")
	t.Setenv("ATRIA_KEY_RECOVERY_PROBE_MODEL", "Atria-Dawn-Preview")

	probedKeys := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		key := r.Header.Get("Authorization")
		probedKeys = append(probedKeys, key)
		switch key {
		case "Bearer revived-key", "Bearer limited-key":
			w.WriteHeader(http.StatusOK)
		case "Bearer dead-key":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	httpClient = &http.Client{Transport: atriaProbeRedirectTransport{target: target}}

	autoBan := 1
	baseURL := "https://api.atria-asi.ai"
	channel := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCustom,
		Name:    "atria-asi",
		Key:     "enabled-key\nrevived-key\ndead-key\ncooling-key\nlimited-key\nmanual-key",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
		AutoBan: &autoBan,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 6,
			MultiKeyStatusList: map[int]int{
				1: common.ChannelStatusAutoDisabled, // 已恢复，可回收
				2: common.ChannelStatusAutoDisabled, // 上游 401，真死
				3: common.ChannelStatusAutoDisabled, // 仍在自愈冷却期
				4: common.ChannelStatusAutoDisabled, // 429 后已恢复
				5: common.ChannelStatusManuallyDisabled,
			},
			MultiKeyDisabledReason: map[int]string{
				1: "status_code=429, Token quota exhausted.",
				2: "status_code=401, revoked",
				3: "status_code=429, Token quota exhausted.",
				4: "status_code=429, Token quota exhausted.",
			},
			MultiKeyDisabledUntil: map[int]int64{
				3: common.GetTimestamp() + 3600,
			},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunAtriaKeyRecovery(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, summary.Channels)
	assert.Equal(t, 3, summary.AutoDisabledKeys, "冷却期内的 key 不计入待回收")
	assert.Equal(t, 3, summary.KeysProbed)
	assert.Equal(t, 2, summary.KeysEnabled)
	assert.Equal(t, 1, summary.RejectedKeys)
	assert.Zero(t, summary.EnableErrors)
	assert.Zero(t, summary.ChannelErrors)
	assert.False(t, summary.Truncated)

	assert.ElementsMatch(t,
		[]string{"Bearer revived-key", "Bearer dead-key", "Bearer limited-key"},
		probedKeys,
		"只探活可回收的自动禁用 key：冷却期内与手动禁用的都不碰")

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", 1).Error)
	statusList := stored.ChannelInfo.MultiKeyStatusList
	_, enabledKeyDisabled := statusList[0]
	_, revivedDisabled := statusList[1]
	_, deadDisabled := statusList[2]
	_, coolingDisabled := statusList[3]
	_, limitedDisabled := statusList[4]
	manualStatus, manualExists := statusList[5]
	assert.False(t, enabledKeyDisabled, "本来就启用的 key 不受影响")
	assert.False(t, revivedDisabled, "恢复的 key 被放回")
	assert.False(t, limitedDisabled, "429 后恢复的 key 被放回")
	assert.True(t, deadDisabled, "被上游拒绝的 key 保持禁用")
	assert.True(t, coolingDisabled, "冷却期内的 key 保持禁用")
	assert.True(t, manualExists, "手动禁用的 key 仍在列表里")
	assert.Equal(t, common.ChannelStatusManuallyDisabled, manualStatus, "手动禁用绝不被推翻")
}

func TestRunAtriaKeyRecoveryKeepsKeysOnIndeterminateProbe(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	oldDB := model.DB
	oldHTTPClient := httpClient
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	t.Cleanup(func() {
		model.DB = oldDB
		httpClient = oldHTTPClient
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Setenv("ATRIA_KEY_RECOVERY_PROBE_INTERVAL_MS", "0")
	t.Setenv("ATRIA_KEY_RECOVERY_RETRY_RATE_LIMITED", "false")

	httpClient = &http.Client{Transport: atriaProbeFailingTransport{}}

	autoBan := 1
	baseURL := "https://api.atria-asi.ai"
	channel := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCustom,
		Name:    "atria-asi",
		Key:     "unreachable-key",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
		AutoBan: &autoBan,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       1,
			MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunAtriaKeyRecovery(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, summary.IndeterminateKeys)
	assert.Zero(t, summary.KeysEnabled, "网络层失败不改变任何 key 的状态")

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", 1).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
}

func TestRunAtriaKeyRecoveryNoopWithoutCandidates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))

	oldDB := model.DB
	oldHTTPClient := httpClient
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	t.Cleanup(func() {
		model.DB = oldDB
		httpClient = oldHTTPClient
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
	})
	model.DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)

	httpClient = &http.Client{Transport: atriaProbeFailingTransport{}}

	autoBan := 1
	baseURL := "https://api.atria-asi.ai"
	channel := &model.Channel{
		Id:          1,
		Type:        constant.ChannelTypeCustom,
		Name:        "atria-asi",
		Key:         "healthy-key",
		Status:      common.ChannelStatusEnabled,
		BaseURL:     &baseURL,
		AutoBan:     &autoBan,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 1},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunAtriaKeyRecovery(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, summary.Channels)
	assert.Zero(t, summary.AutoDisabledKeys)
	assert.Zero(t, summary.KeysProbed, "没有待回收的 key 就不该发起任何探活")
}
