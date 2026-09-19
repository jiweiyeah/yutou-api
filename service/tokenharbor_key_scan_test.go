package service

import (
	"context"
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

func TestTokenHarborModelsURL(t *testing.T) {
	cases := map[string]string{
		"https://tokenharbor.ai/v1/chat/completions": "https://tokenharbor.ai/v1/models",
		"https://tokenharbor.ai/v1":                  "https://tokenharbor.ai/v1/models",
		"https://tokenharbor.ai":                     "https://tokenharbor.ai/v1/models",
		"https://tokenharbor.ai/":                    "https://tokenharbor.ai/v1/models",
		"https://tokenharbor.ai/v1/models":           "https://tokenharbor.ai/v1/models",
		"https://tokenharbor.ai/api/v1/completions":  "https://tokenharbor.ai/api/v1/models",
	}
	for input, want := range cases {
		got, err := tokenHarborModelsURL(input)
		require.NoError(t, err, input)
		assert.Equal(t, want, got, input)
	}
}

func TestTokenHarborModelsURLRejectsHostlessBaseURL(t *testing.T) {
	_, err := tokenHarborModelsURL("tokenharbor.ai/v1")
	require.Error(t, err)
}

func TestIsTokenHarborChannelMatchesOnlyProviderHost(t *testing.T) {
	channelWithBaseURL := func(baseURL string) *model.Channel {
		value := baseURL
		return &model.Channel{BaseURL: &value}
	}

	assert.True(t, IsTokenHarborChannel(channelWithBaseURL("https://tokenharbor.ai/v1/chat/completions")))
	assert.True(t, IsTokenHarborChannel(channelWithBaseURL("https://api.tokenharbor.ai/v1")))
	assert.True(t, IsTokenHarborChannel(channelWithBaseURL("https://TOKENHARBOR.AI/v1")))

	assert.False(t, IsTokenHarborChannel(channelWithBaseURL("https://tokenharbor.ai.evil.example/v1")))
	assert.False(t, IsTokenHarborChannel(channelWithBaseURL("https://gorouter.example/v1")))
	assert.False(t, IsTokenHarborChannel(channelWithBaseURL("")))
}

func TestTokenHarborDisableGuardTripped(t *testing.T) {
	assert.False(t, tokenHarborDisableGuardTripped(0, 100, 0.5))
	assert.False(t, tokenHarborDisableGuardTripped(50, 100, 0.5))
	assert.True(t, tokenHarborDisableGuardTripped(51, 100, 0.5))
	assert.False(t, tokenHarborDisableGuardTripped(59, 10525, 0.5))
	assert.False(t, tokenHarborDisableGuardTripped(10, 100, 0), "a zero ratio disables the guard")
}

// tokenHarborRedirectTransport sends every request to the test server while
// leaving the channel's real base URL (and therefore the provider-host match)
// untouched.
type tokenHarborRedirectTransport struct {
	target *url.URL
}

func (transport tokenHarborRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = transport.target.Scheme
	cloned.URL.Host = transport.target.Host
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestRunTokenHarborKeyScanRebuildsKeyStateFromProbe(t *testing.T) {
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
	t.Setenv("TOKENHARBOR_SCAN_CONCURRENCY", "2")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		switch r.Header.Get("Authorization") {
		case "Bearer healthy-key", "Bearer revived-key":
			w.WriteHeader(http.StatusOK)
		case "Bearer dead-key":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer broken-key":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	httpClient = &http.Client{Transport: tokenHarborRedirectTransport{target: target}}

	autoBan := 1
	baseURL := "https://tokenharbor.ai/v1/chat/completions"
	channel := &model.Channel{
		Id:      1,
		Type:    constant.ChannelTypeCustom,
		Name:    "tokenharbor",
		Key:     "healthy-key\ndead-key\nbroken-key\nrevived-key",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
		AutoBan: &autoBan,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           4,
			MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled, 3: common.ChannelStatusAutoDisabled},
			MultiKeyDisabledReason: map[int]string{1: "status_code=401, stale", 3: "status_code=408, stale"},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunTokenHarborKeyScan(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, summary.Channels)
	assert.Equal(t, 4, summary.KeysChecked)
	assert.Equal(t, 1, summary.KeysEnabled, "the healthy but disabled key comes back")
	assert.Equal(t, 1, summary.KeysDisabled, "the key the upstream cannot serve goes out")
	assert.Equal(t, 1, summary.UnauthorizedKeys)
	assert.Equal(t, 1, summary.UnavailableKeys)
	assert.False(t, summary.DisableGuardHit)
	assert.Zero(t, summary.EnableErrors)
	assert.Zero(t, summary.DisableErrors)

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", 1).Error)
	statusList := stored.ChannelInfo.MultiKeyStatusList
	_, healthyDisabled := statusList[0]
	_, deadDisabled := statusList[1]
	_, brokenDisabled := statusList[2]
	_, revivedDisabled := statusList[3]
	assert.False(t, healthyDisabled, "an already healthy key stays enabled")
	assert.True(t, deadDisabled, "a revoked key stays disabled")
	assert.True(t, brokenDisabled, "an unavailable key gets disabled")
	assert.False(t, revivedDisabled, "a recovered key gets re-enabled")
	assert.Contains(t, stored.ChannelInfo.MultiKeyDisabledReason[1], "401")
	assert.Contains(t, stored.ChannelInfo.MultiKeyDisabledReason[2], "503")
	assert.NotContains(t, stored.ChannelInfo.MultiKeyDisabledReason, 3)
}

func TestRunTokenHarborKeyScanSkipsDisableWhenUpstreamIsDown(t *testing.T) {
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
	t.Setenv("TOKENHARBOR_SCAN_CONCURRENCY", "4")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	httpClient = &http.Client{Transport: tokenHarborRedirectTransport{target: target}}

	autoBan := 1
	baseURL := "https://tokenharbor.ai/v1/chat/completions"
	channel := &model.Channel{
		Id:          2,
		Type:        constant.ChannelTypeCustom,
		Name:        "tokenharbor",
		Key:         "key-a\nkey-b\nkey-c\nkey-d",
		Status:      common.ChannelStatusEnabled,
		BaseURL:     &baseURL,
		AutoBan:     &autoBan,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 4},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunTokenHarborKeyScan(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 4, summary.KeysChecked)
	assert.True(t, summary.DisableGuardHit)
	assert.Zero(t, summary.KeysDisabled, "an upstream-wide outage must not empty the pool")
	assert.Equal(t, 4, summary.UnavailableKeys)

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", 2).Error)
	assert.Empty(t, stored.ChannelInfo.MultiKeyStatusList)
}
