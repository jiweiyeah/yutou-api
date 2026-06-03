# 快速开始 - 渠道缓存性能监控

## ⚡ 3 分钟快速部署

### 1️⃣ 编译（已验证通过）
```bash
go build -o yutou-api
```

### 2️⃣ 启动应用
```bash
# 确保已启用内存缓存
export MEMORY_CACHE_ENABLED=true
export SYNC_FREQUENCY=60  # 缓存同步频率（秒）

./yutou-api
```

### 3️⃣ 查看统计数据（24 小时后）
```bash
# 方法 1: 使用测试脚本（推荐）
./scripts/test_channel_cache_stats.sh http://your-domain:3000 YOUR_ROOT_TOKEN

# 方法 2: 手动调用 API
curl -H "Authorization: Bearer YOUR_ROOT_TOKEN" \
     http://your-domain:3000/api/option/channel_cache_stats | jq '.'
```

## 📊 输出示例

```json
{
  "success": true,
  "data": {
    "selection_count": 123456,           // 总选择次数
    "selection_avg_ms": 2.5,             // 平均耗时 2.5ms ✅ 良好
    "slow_selection_count": 1234,        // 慢查询次数
    "selection_error_count": 12,         // 错误次数
    "channel_disable_count": 234,        // 渠道禁用次数
    "channel_enable_count": 123,         // 渠道启用次数
    "lock_wait_count": 123456,           // 锁等待次数
    "sync_avg_ms": 234,                  // 平均同步耗时
    "last_sync_channel_count": 3456      // 当前渠道总数
  }
}
```

## 🔍 如何判断是否需要优化？

### ✅ 健康状态
- `selection_avg_ms` < 5
- 慢查询比例 < 10%
- 同步耗时 < 1000ms

### ⚠️ 需要优化
- `selection_avg_ms` > 5 → **实施方案 2**（预计算权重）
- 慢查询比例 > 10% → **实施方案 2**
- 渠道禁用频率 > 10次/分钟 → **实施方案 1**（缩短同步间隔）
- 锁等待比例 > 50% → **实施方案 3**（分片锁）

## 💡 最常见的优化：缩短同步间隔

如果你发现渠道经常被禁用（额度耗尽），最简单的优化：

```bash
# 改为 10 秒同步一次（原来 60 秒）
export SYNC_FREQUENCY=10
```

**效果**：
- 已禁用渠道能在 10 秒内从缓存中移除
- 减少 83% 的无效请求（60秒 → 10秒）

## 📖 详细文档

- **完整文档**: `docs/channel_cache_monitoring.md`
- **实施总结**: `docs/channel_cache_monitoring_summary.md`

## 🆘 常见问题

### Q: API 返回 401 Unauthorized
A: 确保使用的是 ROOT 用户的 Token

### Q: 统计数据都是 0
A: 应用刚启动，还没有流量。等待一段时间后再查看

### Q: 想重新收集数据
A: 调用重置接口
```bash
curl -X POST -H "Authorization: Bearer YOUR_ROOT_TOKEN" \
     http://your-domain:3000/api/option/channel_cache_stats/reset
```

### Q: 如何持续监控？
A: 可以配合 Prometheus/Grafana，或定时调用 API 保存到数据库

---

**有任何问题？** 查看 `docs/channel_cache_monitoring.md` 获取详细说明
