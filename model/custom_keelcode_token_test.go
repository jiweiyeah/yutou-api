package model

// ===== CUSTOM START: keelcode token 续期台账 =====

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupKeelcodeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}, &KeelcodeToken{}))

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

func createKeelcodeChannel(t *testing.T, db *gorm.DB, key string, statusList map[int]int) *Channel {
	t.Helper()
	autoBan := 1
	channel := &Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "keelcode",
		Key:     key,
		Status:  common.ChannelStatusEnabled,
		AutoBan: &autoBan,
		Models:  "gpt-4o",
		Group:   "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       3,
			MultiKeyStatusList: statusList,
		},
	}
	require.NoError(t, db.Create(channel).Error)
	return channel
}

// 原地替换的核心契约：新 key 落在旧 key 的同一索引上，因此以索引为键的多密钥
// 状态映射（禁用状态、禁用原因等）在续期后依然指向正确的 key。
func TestReplaceChannelKeysInPlacePreservesKeyIndexes(t *testing.T) {
	db := setupKeelcodeTestDB(t)
	channel := createKeelcodeChannel(t, db, "key-a\nkey-b\nkey-c", map[int]int{
		1: common.ChannelStatusAutoDisabled,
	})

	replaced, err := ReplaceChannelKeysInPlace(channel.Id, map[string]string{
		"key-a": "key-a-new",
		"key-c": "key-c-new",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, replaced)

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-a-new", "key-b", "key-c-new"}, reloaded.GetKeys())
	assert.Equal(t,
		map[int]int{1: common.ChannelStatusAutoDisabled},
		reloaded.ChannelInfo.MultiKeyStatusList,
	)
}

func TestReplaceChannelKeysInPlaceSkipsUnknownAndDuplicateKeys(t *testing.T) {
	db := setupKeelcodeTestDB(t)
	channel := createKeelcodeChannel(t, db, "key-a\nkey-b", map[int]int{})

	replaced, err := ReplaceChannelKeysInPlace(channel.Id, map[string]string{
		"key-gone": "key-new", // 渠道里已不存在
		"key-a":    "key-b",   // 会与现有 key 重复
	})
	require.NoError(t, err)
	assert.Equal(t, 0, replaced)

	reloaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-a", "key-b"}, reloaded.GetKeys())
}

func TestKeelcodeTokenLedgerUpsertAndPrune(t *testing.T) {
	setupKeelcodeTestDB(t)

	require.NoError(t, UpsertKeelcodeToken(KeelcodeToken{
		ChannelId:    10824,
		AccessToken:  "token-a",
		ObtainedAt:   100,
		ExpiresAt:    200,
		RefreshCount: 1,
	}))
	require.NoError(t, UpsertKeelcodeToken(KeelcodeToken{
		ChannelId:    10824,
		AccessToken:  "token-a",
		ObtainedAt:   300,
		ExpiresAt:    400,
		RefreshCount: 2,
	}))
	require.NoError(t, UpsertKeelcodeToken(KeelcodeToken{
		ChannelId:   10824,
		AccessToken: "token-b",
		ExpiresAt:   500,
	}))

	records, err := GetKeelcodeTokensByChannel(10824)
	require.NoError(t, err)
	require.Len(t, records, 2)
	// 同一 token 重复写入是更新而非插入。
	assert.Equal(t, int64(400), records["token-a"].ExpiresAt)
	assert.Equal(t, 2, records["token-a"].RefreshCount)

	require.NoError(t, MarkKeelcodeTokenError("token-b", "boom"))

	// 渠道里已不存在的 token-b 会被清理，token-a 保留。
	pruned, err := PruneKeelcodeTokens(10824, []string{"token-a"})
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)

	records, err = GetKeelcodeTokensByChannel(10824)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Contains(t, records, "token-a")
}

// ===== CUSTOM END =====
