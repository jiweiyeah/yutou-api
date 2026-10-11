package model

// ===== CUSTOM START: 清理已退役的 keelcode token 台账表 =====
// keelcode token 自动续期功能已整体下线，KeelcodeToken 模型不再由 AutoMigrate
// 维护。旧库里的 keelcode_tokens 表如果留着，就是一张没人读写的死表，这里在
// 启动时删掉它。删除失败只记日志不阻断启动 —— 应用已经不依赖这张表了。

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const legacyKeelcodeTokenTable = "keelcode_tokens"

func dropLegacyKeelcodeTokenTable(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(legacyKeelcodeTokenTable) {
		return
	}
	var rows int64
	if err := db.Table(legacyKeelcodeTokenTable).Count(&rows).Error; err != nil {
		common.SysError(common.LogText("failed to count retired %s rows: %s", legacyKeelcodeTokenTable, err.Error()))
	}
	if err := db.Migrator().DropTable(legacyKeelcodeTokenTable); err != nil {
		common.SysError(common.LogText("failed to drop retired %s table: %s", legacyKeelcodeTokenTable, err.Error()))
		return
	}
	common.SysLog(common.LogText("dropped retired %s table (%d rows)", legacyKeelcodeTokenTable, rows))
}

// ===== CUSTOM END =====
