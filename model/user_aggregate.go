package model

// user_aggregate.go — CUSTOM: 管理员用户列表的「累计充值 / 订阅状态」汇总扩展。
// 独立文件存放，尽量降低与 upstream 的合并冲突（见 CLAUDE.md Rule 8）。

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
)

// attachUserAggregates 为一页用户批量填充「累计充值」与「订阅状态」汇总字段。
//
// 累计充值口径（已与产品确认）：在线支付 + 兑换码，统一折算为 quota 单位。
//   - 在线支付到账额度 = SUM(top_ups.amount) × QuotaPerUnit（amount 为美元数量，见 controller/topup.go）
//   - 兑换码额度        = SUM(redemptions.quota)（本身即 quota 单位）
//
// 为避免 N+1，仅针对当页用户 id 各发 1 条 GROUP BY 聚合查询；
// 所用 COALESCE/SUM/MAX/GROUP BY 与 IN 子句对 SQLite/MySQL/PostgreSQL 三库通用（见 Rule 2）。
func attachUserAggregates(users []*User) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]int, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.Id)
	}

	// 1) 在线支付：成功订单的 amount 之和（按 user_id）
	type topupAgg struct {
		UserId    int
		SumAmount int64
	}
	var topupRows []topupAgg
	if err := DB.Model(&TopUp{}).
		Select("user_id, COALESCE(SUM(amount), 0) AS sum_amount").
		Where("user_id IN ? AND status = ?", ids, common.TopUpStatusSuccess).
		Group("user_id").
		Scan(&topupRows).Error; err != nil {
		return err
	}

	// 2) 兑换码：已用兑换码的 quota 之和（按 used_user_id）
	type redeemAgg struct {
		UsedUserId int
		SumQuota   int64
	}
	var redeemRows []redeemAgg
	if err := DB.Model(&Redemption{}).
		Select("used_user_id, COALESCE(SUM(quota), 0) AS sum_quota").
		Where("used_user_id IN ? AND status = ?", ids, common.RedemptionCodeStatusUsed).
		Group("used_user_id").
		Scan(&redeemRows).Error; err != nil {
		return err
	}

	// 3) 进行中订阅：最晚到期时间（按 user_id）
	type subAgg struct {
		UserId int
		MaxEnd int64
	}
	var subRows []subAgg
	now := common.GetTimestamp()
	if err := DB.Model(&UserSubscription{}).
		Select("user_id, MAX(end_time) AS max_end").
		Where("user_id IN ? AND status = ? AND end_time > ?", ids, "active", now).
		Group("user_id").
		Scan(&subRows).Error; err != nil {
		return err
	}

	qpu := decimal.NewFromFloat(common.QuotaPerUnit)
	topupQuotaMap := make(map[int]int64, len(topupRows))
	for _, r := range topupRows {
		topupQuotaMap[r.UserId] = decimal.NewFromInt(r.SumAmount).Mul(qpu).IntPart()
	}
	redeemQuotaMap := make(map[int]int64, len(redeemRows))
	for _, r := range redeemRows {
		redeemQuotaMap[r.UsedUserId] = r.SumQuota
	}
	subEndMap := make(map[int]int64, len(subRows))
	for _, r := range subRows {
		subEndMap[r.UserId] = r.MaxEnd
	}

	for _, u := range users {
		u.TotalTopupQuota = topupQuotaMap[u.Id] + redeemQuotaMap[u.Id]
		if end, ok := subEndMap[u.Id]; ok {
			u.SubActive = true
			u.SubEndTime = end
		}
	}
	return nil
}
