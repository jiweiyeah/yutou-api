package model

// ===== CUSTOM START: 清理已退役的 keelcode token 台账表 =====

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupKeelcodeCleanupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// 旧库升级上来时 keelcode_tokens 还在，启动迁移要把它删掉。
func TestDropLegacyKeelcodeTokenTableRemovesRetiredTable(t *testing.T) {
	db := setupKeelcodeCleanupTestDB(t)
	require.NoError(t, db.Exec(
		"CREATE TABLE keelcode_tokens (id INTEGER PRIMARY KEY, access_token TEXT NOT NULL)",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO keelcode_tokens (access_token) VALUES ('token-a'), ('token-b')",
	).Error)
	require.True(t, db.Migrator().HasTable(legacyKeelcodeTokenTable))

	dropLegacyKeelcodeTokenTable(db)

	assert.False(t, db.Migrator().HasTable(legacyKeelcodeTokenTable))
}

// 全新库和已经删过的库都不该报错，重复执行必须幂等。
func TestDropLegacyKeelcodeTokenTableIsIdempotent(t *testing.T) {
	db := setupKeelcodeCleanupTestDB(t)

	require.NotPanics(t, func() { dropLegacyKeelcodeTokenTable(db) })

	require.NoError(t, db.Exec(
		"CREATE TABLE keelcode_tokens (id INTEGER PRIMARY KEY, access_token TEXT NOT NULL)",
	).Error)
	dropLegacyKeelcodeTokenTable(db)
	dropLegacyKeelcodeTokenTable(db)

	assert.False(t, db.Migrator().HasTable(legacyKeelcodeTokenTable))
}

// 传 nil 是「数据库没初始化」的调用形态，不能 panic。
func TestDropLegacyKeelcodeTokenTableToleratesNilDB(t *testing.T) {
	require.NotPanics(t, func() { dropLegacyKeelcodeTokenTable(nil) })
}

// ===== CUSTOM END =====
