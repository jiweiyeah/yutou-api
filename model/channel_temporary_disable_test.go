package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTemporaryDisableTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))

	oldDB := DB
	oldMemoryCacheEnabled := common.MemoryCacheEnabled
	oldMainDatabaseType := common.MainDatabaseType()
	t.Cleanup(func() {
		DB = oldDB
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
		common.SetMainDatabaseType(oldMainDatabaseType)
	})
	DB = db
	common.MemoryCacheEnabled = false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	return db
}

func TestTemporaryDisableAndRecoverMultiKeyChannel(t *testing.T) {
	db := setupTemporaryDisableTestDB(t)
	autoBan := 1
	channel := &Channel{
		Type:    constant.ChannelTypeDeepSeek,
		Name:    "deepseek-free",
		Key:     "key-a\nkey-b",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		Models:  "deepseek-v4-flash",
		Group:   "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       2,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	resetA := common.GetTimestamp() + 300
	resetB := resetA + 300
	changed, err := DisableChannelKeyUntil(channel.Id, "key-a", "free tier exhausted", resetA)
	require.NoError(t, err)
	require.True(t, changed)

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, resetA, reloaded.ChannelInfo.MultiKeyDisabledUntil[0])

	changed, err = DisableChannelKeyUntil(channel.Id, "key-b", "free tier exhausted", resetB)
	require.NoError(t, err)
	require.True(t, changed)
	reloaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)

	recovered, err := RecoverExpiredChannelKeys(channel.Id, resetA-1)
	require.NoError(t, err)
	assert.Zero(t, recovered)

	recovered, err = RecoverExpiredChannelKeys(channel.Id, resetA)
	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	reloaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.NotContains(t, reloaded.ChannelInfo.MultiKeyStatusList, 0)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.ChannelInfo.MultiKeyStatusList[1])

	recovered, err = RecoverExpiredChannelKeys(channel.Id, resetB)
	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	reloaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Empty(t, reloaded.ChannelInfo.MultiKeyStatusList)
	assert.Empty(t, reloaded.ChannelInfo.MultiKeyDisabledUntil)
}

func TestTemporaryRecoveryPreservesManualDisable(t *testing.T) {
	db := setupTemporaryDisableTestDB(t)
	autoBan := 1
	channel := &Channel{
		Type:    constant.ChannelTypeDeepSeek,
		Name:    "manual-key",
		Key:     "key-a\nkey-b",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		ChannelInfo: ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           2,
			MultiKeyStatusList:     map[int]int{0: common.ChannelStatusManuallyDisabled},
			MultiKeyDisabledUntil:  map[int]int64{0: common.GetTimestamp() - 1},
			MultiKeyDisabledReason: map[int]string{0: "manual"},
		},
	}
	require.NoError(t, db.Create(channel).Error)

	recovered, err := RecoverExpiredChannelKeys(channel.Id, common.GetTimestamp())
	require.NoError(t, err)
	assert.Zero(t, recovered)

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, reloaded.ChannelInfo.MultiKeyStatusList[0])
}

func TestTemporaryDisableAndRecoverSingleKeyChannel(t *testing.T) {
	db := setupTemporaryDisableTestDB(t)
	autoBan := 1
	channel := &Channel{
		Type:    constant.ChannelTypeDeepSeek,
		Name:    "single-key",
		Key:     "key-a",
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
	}
	require.NoError(t, db.Create(channel).Error)
	resetAt := common.GetTimestamp() + 300
	changed, err := DisableChannelKeyUntil(channel.Id, "old-key", "stale request", resetAt)
	require.NoError(t, err)
	assert.False(t, changed)

	changed, err = DisableChannelKeyUntil(channel.Id, "key-a", "free tier exhausted", resetAt)
	require.NoError(t, err)
	require.True(t, changed)
	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.Status)
	assert.Equal(t, resetAt, reloaded.ChannelInfo.AutoDisabledUntil)

	recovered, err := RecoverExpiredChannelKeys(channel.Id, resetAt)
	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	reloaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, reloaded.Status)
	assert.Zero(t, reloaded.ChannelInfo.AutoDisabledUntil)
}
