package service

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		default:
			http.Error(w, "unexpected key", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	httpClient = server.Client()

	autoBan := 1
	channel := &model.Channel{
		Type:    constant.ChannelTypeKiteDelayed,
		Name:    "marathon",
		Key:     "low-key\nlow-key-2\nhealthy-key\nerror-key",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       4,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	summary, err := RunKiteCreditsScan(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Channels)
	assert.Equal(t, 4, summary.KeysChecked)
	assert.Equal(t, 2, summary.LowBalanceKeys)
	assert.Equal(t, 2, summary.DisabledKeys)
	assert.Equal(t, 1, summary.RequestErrors)
	assert.Zero(t, summary.DisableErrors)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[1])
	_, healthyDisabled := reloaded.ChannelInfo.MultiKeyStatusList[2]
	_, erroredDisabled := reloaded.ChannelInfo.MultiKeyStatusList[3]
	assert.False(t, healthyDisabled)
	assert.False(t, erroredDisabled)
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
