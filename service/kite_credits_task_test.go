package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRunKiteCreditsScanDisablesOnlyLowBalanceKeys(t *testing.T) {
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
	t.Setenv("KITE_CREDITS_DISABLE_THRESHOLD", "0.10")
	t.Setenv("KITE_CREDITS_TASK_CONCURRENCY", "3")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/credits", r.URL.Path)
		switch r.Header.Get("Authorization") {
		case "Bearer low-key":
			_, _ = w.Write([]byte(`{"currency":"USD","balance":"0.05"}`))
		case "Bearer low-key-2":
			_, _ = w.Write([]byte(`{"currency":"USD","balance":"0.10"}`))
		case "Bearer healthy-key":
			_, _ = w.Write([]byte(`{"currency":"USD","balance":"4.304734"}`))
		case "Bearer error-key":
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		case "Bearer dead-key":
			http.Error(w, "invalid key", http.StatusUnauthorized)
		default:
			http.Error(w, "unexpected key", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	httpClient = server.Client()

	autoBan := 1
	channel := &model.Channel{
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "marathon",
		Key:     "low-key\nlow-key-2\nhealthy-key\nerror-key\ndead-key",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       5,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunKiteCreditsScan(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Channels)
	assert.Equal(t, 5, summary.KeysChecked)
	assert.Equal(t, 2, summary.LowBalanceKeys)
	assert.Equal(t, 1, summary.AuthFailedKeys)
	assert.Equal(t, 3, summary.DisabledKeys)
	assert.Equal(t, 1, summary.RequestErrors)
	assert.Zero(t, summary.DisableErrors)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[1])
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[4])
	assert.Contains(t, reloaded.ChannelInfo.MultiKeyDisabledReason[4], "401")
	_, healthyDisabled := reloaded.ChannelInfo.MultiKeyStatusList[2]
	_, erroredDisabled := reloaded.ChannelInfo.MultiKeyStatusList[3]
	assert.False(t, healthyDisabled)
	assert.False(t, erroredDisabled)
}

func TestRunKiteCreditsScanAuthFailGuardSkipsMassDisable(t *testing.T) {
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
	t.Setenv("KITE_CREDITS_DISABLE_THRESHOLD", "0.10")
	t.Setenv("KITE_CREDITS_TASK_CONCURRENCY", "3")

	// Every key gets a 401: the upstream auth endpoint is broken, so the guard
	// must prevent the scan from wiping out the whole pool.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/credits", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	httpClient = server.Client()

	keys := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		keys = append(keys, fmt.Sprintf("key-%d", i))
	}
	autoBan := 1
	channel := &model.Channel{
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "marathon",
		Key:     strings.Join(keys, "\n"),
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       10,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunKiteCreditsScan(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 10, summary.KeysChecked)
	assert.Equal(t, 10, summary.AuthFailedKeys)
	assert.Equal(t, 10, summary.RequestErrors)
	assert.Zero(t, summary.LowBalanceKeys)
	assert.Zero(t, summary.DisabledKeys)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.Empty(t, reloaded.ChannelInfo.MultiKeyStatusList)
}

func TestKiteAuthFailGuardTripped(t *testing.T) {
	tests := []struct {
		name       string
		authFailed int
		checked    int
		maxRatio   float64
		want       bool
	}{
		{name: "no failures", authFailed: 0, checked: 100, maxRatio: 0.2, want: false},
		{name: "single dead key in large pool", authFailed: 1, checked: 1803, maxRatio: 0.2, want: false},
		{name: "small pool exempt", authFailed: 1, checked: 1, maxRatio: 0.2, want: false},
		{name: "at ratio boundary", authFailed: 2, checked: 10, maxRatio: 0.2, want: false},
		{name: "above ratio", authFailed: 3, checked: 10, maxRatio: 0.2, want: true},
		{name: "all keys fail", authFailed: 10, checked: 10, maxRatio: 0.2, want: true},
		{name: "guard disabled", authFailed: 100, checked: 100, maxRatio: 0, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, kiteAuthFailGuardTripped(test.authFailed, test.checked, test.maxRatio))
		})
	}
}

func TestParseKiteCreditsBalanceRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "missing", value: nil},
		{name: "malformed", value: "not-a-number"},
		{name: "NaN", value: "NaN"},
		{name: "positive infinity", value: "+Inf"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseKiteCreditsBalance(test.value)
			require.Error(t, err)
		})
	}
}
