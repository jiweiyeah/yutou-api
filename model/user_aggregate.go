package model

// user_aggregate.go — CUSTOM: 管理员用户列表的「累计充值 / 订阅状态」汇总、筛选与排序扩展。
// 独立文件存放，尽量降低与 upstream 的合并冲突（见 CLAUDE.md Rule 8）。

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 订阅状态筛选取值。
const (
	SubStatusAny    = ""       // 不筛
	SubStatusActive = "active" // 存在进行中订阅
	SubStatusNone   = "none"   // 不存在进行中订阅
)

// 排序字段（白名单）。
const (
	OrderByDefault    = ""            // 默认 id desc
	OrderByTotalTopup = "total_topup" // 按累计充值额度排序
)

// attachUserAggregates 为一页用户批量填充「累计充值」「实付金额」与「订阅状态」汇总字段。
//
// 累计充值口径（已与产品确认）：在线支付 + 兑换码，统一折算为 quota 单位。
//   - 在线支付到账额度 = SUM(top_ups.amount) × QuotaPerUnit（amount 为美元数量，见 controller/topup.go）
//   - 兑换码额度        = SUM(redemptions.quota)（本身即 quota 单位）
//
// 实付金额口径：仅在线支付成功订单的 SUM(top_ups.money)（兑换码无支付动作，不计入）。
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

	// 1) 在线支付：成功订单的 amount（折额度）与 money（实付）之和（按 user_id）
	type topupAgg struct {
		UserId    int
		SumAmount int64
		SumMoney  float64
	}
	var topupRows []topupAgg
	if err := DB.Model(&TopUp{}).
		Select("user_id, COALESCE(SUM(amount), 0) AS sum_amount, COALESCE(SUM(money), 0) AS sum_money").
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
	topupMoneyMap := make(map[int]float64, len(topupRows))
	for _, r := range topupRows {
		topupQuotaMap[r.UserId] = decimal.NewFromInt(r.SumAmount).Mul(qpu).IntPart()
		topupMoneyMap[r.UserId] = r.SumMoney
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
		u.TotalTopupMoney = topupMoneyMap[u.Id]
		if end, ok := subEndMap[u.Id]; ok {
			u.SubActive = true
			u.SubEndTime = end
		}
	}
	return nil
}

// applyUserFilters 在给定查询上叠加用户列表的筛选条件（含订阅状态 EXISTS 过滤）。
// Count 与取数查询共用本函数，确保筛选后的 total 与列表一致。
func applyUserFilters(query *gorm.DB, keyword, group string, role, status *int, subStatus string) *gorm.DB {
	if keyword != "" {
		like := "username LIKE ? OR email LIKE ? OR display_name LIKE ?"
		args := []interface{}{"%" + keyword + "%", "%" + keyword + "%", "%" + keyword + "%"}
		if kwInt, err := strconv.Atoi(keyword); err == nil {
			like = "id = ? OR " + like
			args = append([]interface{}{kwInt}, args...)
		}
		query = query.Where("("+like+")", args...)
	}
	if group != "" {
		query = query.Where(commonGroupCol+" = ?", group)
	}
	if role != nil {
		query = query.Where("role = ?", *role)
	}
	if status != nil {
		// ===== CUSTOM: 吸收上游软删除筛选语义 —— status==-1 查已软删用户，否则仅查未删用户 =====
		// queryUsers 用 Unscoped()（默认含软删行），故此处显式控制 deleted_at。
		if *status == -1 {
			query = query.Where("deleted_at IS NOT NULL")
		} else {
			query = query.Where("deleted_at IS NULL").Where("status = ?", *status)
		}
	}

	if subStatus == SubStatusActive || subStatus == SubStatusNone {
		now := common.GetTimestamp()
		// 半连接：是否存在该用户的进行中订阅。命中复合索引 idx_user_sub_active。
		existsSub := DB.Table("user_subscriptions AS s").
			Select("1").
			Where("s.user_id = users.id AND s.status = ? AND s.end_time > ?", "active", now)
		if subStatus == SubStatusActive {
			query = query.Where("EXISTS (?)", existsSub)
		} else {
			query = query.Where("NOT EXISTS (?)", existsSub)
		}
	}
	return query
}

// queryUsers 是用户列表的统一查询入口，支持现有筛选 + 订阅状态过滤 + 累计充值排序。
// 展示用的汇总值（累计充值/实付/订阅到期）仍由分页后的 attachUserAggregates 回填，
// 排序键与展示值同源同口径。
func queryUsers(keyword, group string, role, status *int, subStatus, orderBy, orderDir string, startIdx, num int) (users []*User, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 保留 Unscoped 以维持现有「可见软删用户」的行为。
	base := tx.Unscoped().Model(&User{})
	base = applyUserFilters(base, keyword, group, role, status, subStatus)

	// Count：独立 session，绝不挂 Order / 排序 Joins（别名列会触发 unknown column）。
	// 但保留 applyUserFilters 注入的 EXISTS，使筛选后的 total 正确。
	if err = base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// 取数：按需挂排序 JOIN + ORDER BY 表达式。
	dataTx := base.Session(&gorm.Session{})
	if orderBy == OrderByTotalTopup {
		topupSub := tx.Table("top_ups").
			Select("user_id, COALESCE(SUM(amount), 0) AS sum_amount").
			Where("status = ?", common.TopUpStatusSuccess).
			Group("user_id")
		// redemptions 有软删除，裸 JOIN 不会自动排除已删行 —— 必须显式 deleted_at IS NULL，
		// 否则排序键会把已软删的兑换码也计入，与 attachUserAggregates 的展示值口径分叉。
		redeemSub := tx.Table("redemptions").
			Select("used_user_id, COALESCE(SUM(quota), 0) AS sum_quota").
			Where("status = ? AND deleted_at IS NULL", common.RedemptionCodeStatusUsed).
			Group("used_user_id")

		dataTx = dataTx.
			Joins("LEFT JOIN (?) AS t ON t.user_id = users.id", topupSub).
			Joins("LEFT JOIN (?) AS r ON r.used_user_id = users.id", redeemSub)

		// QuotaPerUnit 为受信后端常量，格式化成整数字面量拼接（非用户输入，无注入）。
		// 双层 COALESCE(...,0) 使排序键恒非 NULL，规避三库 NULL 排序方向差异（无需 NULLS LAST）。
		qpu := strconv.FormatInt(int64(common.QuotaPerUnit), 10)
		dir := "DESC"
		if strings.ToLower(orderDir) == "asc" {
			dir = "ASC"
		}
		dataTx = dataTx.Order("(COALESCE(t.sum_amount, 0) * " + qpu + " + COALESCE(r.sum_quota, 0)) " + dir)
		// 追加稳定次级键，避免相同累计充值时翻页重复/漏。
		dataTx = dataTx.Order("users.id DESC")
	} else {
		dataTx = dataTx.Order("users.id DESC")
	}

	if err = dataTx.Omit("password").Limit(num).Offset(startIdx).Find(&users).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	// 分页后回填展示值（复用现有逻辑，口径同源）。失败仅降级、不阻断列表。
	if aggErr := attachUserAggregates(users); aggErr != nil {
		common.SysError("attachUserAggregates failed (queryUsers): " + aggErr.Error())
	}
	return users, total, nil
}
