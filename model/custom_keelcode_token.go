package model

// ===== CUSTOM START: keelcode token 续期台账 =====
// keelcode 的 access_token 是不透明随机串（不是 JWT），过期时间无法从 key 本身
// 推断，因此需要一张独立台账记录「渠道里的这把 key 什么时候到期」。
// 台账按 token 值主键化，不按 key 索引 —— 多密钥渠道的索引会随增删前移，
// 而 token 值在其生命周期内唯一且稳定。

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// KeelcodeToken 记录一把 keelcode access_token 的生命周期。
// AccessToken 与渠道 key 列表里的某一项一一对应；台账里存在但渠道里已消失的
// 记录会在下一轮扫描时被清理（见 PruneKeelcodeTokens）。
type KeelcodeToken struct {
	Id           int    `json:"id" gorm:"primary_key"`
	ChannelId    int    `json:"channel_id" gorm:"index"`
	AccessToken  string `json:"access_token" gorm:"type:varchar(255);uniqueIndex"`
	ObtainedAt   int64  `json:"obtained_at" gorm:"bigint"`
	ExpiresAt    int64  `json:"expires_at" gorm:"bigint;index"`
	RefreshCount int    `json:"refresh_count" gorm:"default:0"`
	LastError    string `json:"last_error" gorm:"type:text"`
	CreatedAt    int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt    int64  `json:"updated_at" gorm:"bigint"`
}

func (token *KeelcodeToken) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if token.CreatedAt == 0 {
		token.CreatedAt = now
	}
	if token.UpdatedAt == 0 {
		token.UpdatedAt = now
	}
	return nil
}

// GetKeelcodeTokensByChannel 返回某渠道的全部台账记录，按 token 值索引，
// 方便与渠道 key 列表做匹配。
func GetKeelcodeTokensByChannel(channelId int) (map[string]*KeelcodeToken, error) {
	var records []*KeelcodeToken
	if err := DB.Where("channel_id = ?", channelId).Find(&records).Error; err != nil {
		return nil, err
	}
	byToken := make(map[string]*KeelcodeToken, len(records))
	for _, record := range records {
		byToken[record.AccessToken] = record
	}
	return byToken, nil
}

// UpsertKeelcodeToken 写入或更新一条台账记录。token 值唯一，重复写入是幂等的。
func UpsertKeelcodeToken(record KeelcodeToken) error {
	record.AccessToken = strings.TrimSpace(record.AccessToken)
	if record.AccessToken == "" {
		return errors.New("keelcode token is empty")
	}
	now := common.GetTimestamp()
	record.UpdatedAt = now

	var existing KeelcodeToken
	err := DB.Where("access_token = ?", record.AccessToken).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		record.CreatedAt = now
		return DB.Create(&record).Error
	}
	if err != nil {
		return err
	}
	return DB.Model(&KeelcodeToken{}).Where("id = ?", existing.Id).Updates(map[string]any{
		"channel_id":    record.ChannelId,
		"obtained_at":   record.ObtainedAt,
		"expires_at":    record.ExpiresAt,
		"refresh_count": record.RefreshCount,
		"last_error":    record.LastError,
		"updated_at":    now,
	}).Error
}

// MarkKeelcodeTokenError 记录一次续期失败，便于在后台排查是哪把 key 续不动了。
// 记录不存在时静默跳过：续期成功路径才负责建档。
func MarkKeelcodeTokenError(accessToken string, message string) error {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil
	}
	return DB.Model(&KeelcodeToken{}).Where("access_token = ?", accessToken).Updates(map[string]any{
		"last_error": message,
		"updated_at": common.GetTimestamp(),
	}).Error
}

// PruneKeelcodeTokens 删除渠道里已不存在的台账记录（key 被手工摘除、被别的
// 流程覆盖等）。keepTokens 为渠道当前的 key 集合。
func PruneKeelcodeTokens(channelId int, keepTokens []string) (int, error) {
	query := DB.Where("channel_id = ?", channelId)
	if len(keepTokens) > 0 {
		query = query.Where("access_token NOT IN ?", keepTokens)
	}
	result := query.Delete(&KeelcodeToken{})
	return int(result.RowsAffected), result.Error
}

// ReplaceChannelKeysInPlace 把渠道 key 列表里的旧 key 原地替换成新 key。
//
// 原地替换（而不是「追加新 key + 摘除旧 key」）是刻意的：多密钥渠道的所有状态
// 映射（MultiKeyStatusList / MultiKeyDisabledReason / ...）都以 key 索引为键，
// 增删会让索引整体前移并使这些映射错位。同索引替换则让全部映射保持有效，
// MultiKeySize 与轮询指针也无需调整。
//
// replacements 为「旧 key → 新 key」。渠道里已不存在的旧 key 会被忽略；
// 新 key 已存在于渠道里时同样跳过，避免出现重复 key。
// 返回实际替换的条数。
func ReplaceChannelKeysInPlace(channelId int, replacements map[string]string) (int, error) {
	if len(replacements) == 0 {
		return 0, nil
	}

	channelStatusLock.Lock()
	defer channelStatusLock.Unlock()
	pollingLock := GetChannelPollingLock(channelId)
	pollingLock.Lock()
	defer pollingLock.Unlock()

	var (
		channel  Channel
		replaced int
	)
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).First(&channel, "id = ?", channelId).Error; err != nil {
			return err
		}

		keys := channel.GetKeys()
		existing := make(map[string]bool, len(keys))
		for _, key := range keys {
			existing[strings.TrimSpace(key)] = true
		}

		nextKeys := make([]string, len(keys))
		copy(nextKeys, keys)
		for index, key := range nextKeys {
			newKey, ok := replacements[strings.TrimSpace(key)]
			if !ok || newKey == "" || existing[newKey] {
				continue
			}
			nextKeys[index] = newKey
			existing[newKey] = true
			replaced++
		}
		if replaced == 0 {
			return nil
		}

		channel.Key = strings.Join(nextKeys, "\n")
		channel.Keys = nextKeys
		return tx.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
			"key": channel.Key,
		}).Error
	})
	if err != nil || replaced == 0 {
		return 0, err
	}

	channel.Keys = channel.GetKeys()
	CacheUpdateChannel(&channel)
	return replaced, nil
}

// ===== CUSTOM END =====
