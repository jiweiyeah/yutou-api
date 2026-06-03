# 渠道缓存性能监控

## 概述

为了监控几千个渠道场景下的负载均衡性能，已在关键路径添加了轻量级性能监控。

## 监控指标

### 1. 渠道选择统计
- `selection_count`: 总选择次数
- `selection_avg_us`: 平均选择耗时（微秒）
- `selection_avg_ms`: 平均选择耗时（毫秒）
- `slow_selection_count`: 慢查询次数（>10ms）
- `selection_error_count`: 选择失败次数

### 2. 缓存同步统计
- `sync_count`: 缓存同步次数
- `sync_avg_ms`: 平均同步耗时（毫秒）
- `last_sync_time`: 最后同步时间（Unix时间戳）
- `last_sync_channel_count`: 最后一次同步的渠道总数

### 3. 渠道状态变更统计
- `channel_disable_count`: 渠道禁用次数
- `channel_enable_count`: 渠道启用次数

### 4. 锁竞争统计
- `lock_wait_count`: 锁等待次数（每次获取锁都计数）

## API 端点

### 获取统计数据
```bash
GET /api/option/channel_cache_stats
Authorization: Bearer <ROOT_TOKEN>

# 响应示例
{
  "success": true,
  "data": {
    "selection_count": 123456,
    "selection_avg_us": 1234,
    "selection_avg_ms": 1.234,
    "slow_selection_count": 45,
    "selection_error_count": 12,
    "sync_count": 24,
    "sync_avg_ms": 234,
    "last_sync_time": 1717430400,
    "last_sync_channel_count": 3456,
    "channel_disable_count": 234,
    "channel_enable_count": 123,
    "lock_wait_count": 123456
  }
}
```

### 重置统计数据
```bash
POST /api/option/channel_cache_stats/reset
Authorization: Bearer <ROOT_TOKEN>
```

## 使用方式

### 1. 启动应用
确保环境变量已配置：
```bash
MEMORY_CACHE_ENABLED=true  # 必须启用内存缓存
SYNC_FREQUENCY=60          # 缓存同步频率（秒），默认 60
```

### 2. 收集基线数据
运行 24 小时后调用 API 查看统计数据：
```bash
curl -H "Authorization: Bearer YOUR_ROOT_TOKEN" \
     http://localhost:3000/api/option/channel_cache_stats
```

### 3. 分析性能瓶颈

#### 检查选择耗时
- 如果 `selection_avg_ms > 5`，说明选择渠道较慢
- 如果 `slow_selection_count / selection_count > 0.1`（10%），说明慢查询比例高

#### 检查锁竞争
- 如果 `lock_wait_count` 与 `selection_count` 接近，说明锁竞争严重
- 高并发场景下，`lock_wait_count` 应远小于 `selection_count`

#### 检查状态变更频率
- 查看 `channel_disable_count` 和 `channel_enable_count`
- 如果每分钟禁用/启用次数 > 10，说明渠道额度消耗很快
- 计算公式：每分钟禁用次数 = `channel_disable_count / (运行分钟数)`

#### 检查同步性能
- 如果 `sync_avg_ms > 1000`（>1秒），说明同步较慢
- 几千个渠道的同步通常在 100-500ms 之间

### 4. 优化决策

根据数据决定优化方向：

| 指标 | 阈值 | 优化方向 |
|------|------|---------|
| `selection_avg_ms` > 5 | 慢 | 实施预计算权重索引 |
| `slow_selection_count / selection_count` > 10% | 高 | 实施预计算权重索引 |
| `lock_wait_count / selection_count` > 0.5 | 高锁竞争 | 缩短 SYNC_FREQUENCY 或实施分片锁 |
| `channel_disable_count` 每分钟 > 10 | 频繁变更 | 缩短 SYNC_FREQUENCY（如改为 10 秒） |
| `sync_avg_ms` > 1000 | 同步慢 | 优化同步逻辑或增加并发 |

## 日志输出

慢查询会自动记录到日志：
```
[WARN] slow channel selection: group=default, model=gpt-4, retry=0, took=15234us
```

同步也会记录耗时：
```
[INFO] channels synced from database (took 234ms, 3456 channels)
```

## 注意事项

1. **统计数据是累计的**：从应用启动开始累计，不会自动重置
2. **重置功能仅用于测试**：生产环境不建议频繁重置
3. **性能开销极低**：使用原子操作，对性能影响可忽略不计（<0.1%）
4. **需要 ROOT 权限**：查看统计数据需要管理员 Token

## 下一步优化方案

根据监控数据，可以选择以下优化方案：

### 方案 1：缩短同步间隔（最低成本）
- 修改 `SYNC_FREQUENCY=10`（改为 10 秒）
- 适用于渠道状态频繁变更的场景

### 方案 2：预计算权重索引（中等改动）
- 在缓存初始化时预先计算权重
- 可减少 30-50% 的选择耗时

### 方案 3：分片锁（大改动）
- 将全局锁拆分为 32 个分片
- 适用于高并发场景（QPS > 10000）

---

**文件位置**：
- 统计实现：`model/channel_cache_stats.go`
- 监控埋点：`model/channel_cache.go`、`model/channel.go`
- API 端点：`controller/channel_cache_stats.go`
- 路由注册：`router/api-router.go`
