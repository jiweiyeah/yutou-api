# 渠道模型更新方案 - 完整指南

## 📚 abilities 表详解

### 什么是 abilities 表？

`abilities` 表是 yutou-api 中**模型调度的核心索引表**，作用类似于数据库的索引，用于快速匹配用户请求的模型到可用渠道。

### 表结构

```go
type Ability struct {
    Group     string  // 分组名（如 'default', 'vip'）
    Model     string  // 模型名（如 'gpt-4', 'claude-3'）
    ChannelId int     // 渠道ID
    Enabled   bool    // 是否启用
    Priority  *int64  // 优先级（用于多优先级调度）
    Weight    uint    // 权重（用于负载均衡）
    Tag       *string // 标签
}
```

**复合主键**: `(Group, Model, ChannelId)` - 确保每个组合唯一

### 数据关系示例

假设有一个渠道：

```
Channel ID: 123
Group: "default,vip"
Models: "gpt-4,claude-3,moonshotai/kimi-k2.7-code"
```

`AddAbilities()` 函数会生成 **6 条** abilities 记录：

```
| Group   | Model                        | ChannelId | Enabled |
|---------|------------------------------|-----------|---------|
| default | gpt-4                        | 123       | true    |
| default | claude-3                     | 123       | true    |
| default | moonshotai/kimi-k2.7-code    | 123       | true    |
| vip     | gpt-4                        | 123       | true    |
| vip     | claude-3                     | 123       | true    |
| vip     | moonshotai/kimi-k2.7-code    | 123       | true    |
```

### 为什么需要这个表？

#### ❌ 不使用 abilities 表（低效）

每次用户请求模型时：

```sql
-- 需要扫描所有渠道，检查逗号分隔的字符串
SELECT * FROM channels
WHERE group LIKE '%default%'
  AND models LIKE '%gpt-4%'
  AND status = 1;
```

- **性能差**：LIKE 查询无法使用索引
- **逻辑复杂**：需要处理逗号分隔、模糊匹配
- **难以优化**：无法按优先级、权重快速筛选

#### ✅ 使用 abilities 表（高效）

```sql
-- 精确匹配，使用复合索引
SELECT channel_id FROM abilities
WHERE group = 'default'
  AND model = 'gpt-4'
  AND enabled = true
ORDER BY priority DESC, weight DESC;
```

- **性能好**：使用复合索引，查询毫秒级
- **逻辑清晰**：直接 WHERE 条件，无需字符串处理
- **易于优化**：支持多优先级、权重、随机负载均衡

### 更新 channels 后为什么要重建？

当你执行：

```sql
UPDATE channels
SET models = CONCAT(models, ',moonshotai/kimi-k2.7-code');
```

**只更新了 `channels` 表**，`abilities` 表**不会自动同步**！

此时：
- ✅ `channels.models` 包含 `moonshotai/kimi-k2.7-code`
- ❌ `abilities` 表中**没有**该模型的记录
- ❌ 用户请求 `moonshotai/kimi-k2.7-code` 时，查询 `abilities` 表返回空
- ❌ 系统报错：**"该模型不可用"**

### FixAbility() 函数的工作流程

```go
// model/ability.go:287
func FixAbility() (int, int, error) {
    // 1. 清空 abilities 表
    TRUNCATE TABLE abilities

    // 2. 获取所有渠道
    var channels []*Channel
    DB.Find(&channels)

    // 3. 遍历每个渠道，重新生成 abilities 记录
    for _, channel := range channels {
        models := strings.Split(channel.Models, ",")   // 分割模型字符串
        groups := strings.Split(channel.Group, ",")    // 分割分组字符串

        // 4. 为每个 (group, model) 组合创建记录
        for _, model := range models {
            for _, group := range groups {
                ability := Ability{
                    Group:     group,
                    Model:     model,
                    ChannelId: channel.Id,
                    Enabled:   channel.Status == ChannelStatusEnabled,
                    Priority:  channel.Priority,
                    Weight:    channel.Weight,
                }
                DB.Create(&ability)
            }
        }
    }
}
```

### 数据一致性保证

系统通过以下机制维护一致性：

1. **创建渠道时**：`channel.Insert()` → 自动调用 `AddAbilities()`
2. **更新渠道时**：`channel.Update()` → 自动调用 `UpdateAbilities()`
3. **批量导入时**：`BatchInsertChannels()` → 自动调用 `AddAbilities()`
4. **手动修复**：管理后台「修复数据库一致性」→ 调用 `FixAbility()`

**本次操作例外**：我们直接执行 SQL 更新 `channels` 表，**绕过了 Go 代码**，所以必须手动重建。

---

## 🚀 执行步骤

### 步骤 1: 检查数据库状态（必须先做）

在服务器上执行：

```bash
bash /path/to/check_database_before_update.sh
```

这个脚本会：
- 自动检测数据库类型（MySQL 或 PostgreSQL）
- 统计渠道总数
- 检查已有 kimi-k2.7-code 模型的渠道数
- 分析 models 和 model_mapping 字段的数据分布
- 预估更新影响范围

### 步骤 2: 执行更新

确认检查结果无误后，执行：

```bash
bash /path/to/smart_update_kimi.sh
```

这个脚本会：
- 自动检测数据库类型
- 自动备份 channels 表
- 更新 models 字段（添加新模型）
- 更新 model_mapping 字段（添加映射规则）
- 验证更新结果
- 显示详细的统计信息

### 步骤 3: 重建 abilities 表（必须）

**方式 1（推荐）**: 管理后台操作
1. 登录管理后台（通常是 `http://your-domain/admin`）
2. 点击左侧菜单「渠道管理」
3. 点击页面上方的「修复数据库一致性」按钮
4. 等待处理完成（会显示成功/失败数量）

**方式 2**: API 调用
```bash
# 需要管理员 Token
curl -X POST http://your-domain/api/channel/fix \
     -H "Authorization: Bearer YOUR_ADMIN_TOKEN"
```

**方式 3**: 重启应用（如果配置了启动时自动修复）
```bash
# 某些部署可能配置了启动时检查并修复
systemctl restart yutou-api
# 或
docker restart yutou-api
```

---

## 📋 更新内容

### 1. models 字段更新

在每个渠道的 `models` 字段末尾追加新模型：

```
更新前: gpt-4,claude-3
更新后: gpt-4,claude-3,moonshotai/kimi-k2.7-code
```

### 2. model_mapping 字段更新

在 `model_mapping` JSON 对象中添加映射规则：

```json
更新前: {"gpt-4":"gpt-4-turbo"}
更新后: {"gpt-4":"gpt-4-turbo","moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}
```

**映射作用**：
- 用户请求 `moonshotai/kimi-k2.7-code`
- 系统查询 model_mapping，发现映射规则
- 实际向上游发送 `@cf/moonshotai/kimi-k2.7-code`

这样做的好处：
- 用户使用统一的模型名
- 后端灵活切换不同的上游实现
- 支持 Cloudflare Workers AI 的 `@cf/` 前缀模型

---

## 🔍 验证方法

### 验证 1: 数据库检查

```sql
-- 检查 channels 表
SELECT COUNT(*) FROM channels WHERE models LIKE '%moonshotai/kimi-k2.7-code%';
SELECT COUNT(*) FROM channels WHERE model_mapping LIKE '%moonshotai/kimi-k2.7-code%';

-- 检查 abilities 表
SELECT COUNT(*) FROM abilities WHERE model = 'moonshotai/kimi-k2.7-code';

-- 查看具体记录（前 5 个）
SELECT id, name, models, model_mapping FROM channels LIMIT 5;
SELECT * FROM abilities WHERE model = 'moonshotai/kimi-k2.7-code' LIMIT 5;
```

### 验证 2: API 测试

```bash
# 测试模型是否可用
curl -X POST http://your-domain/v1/chat/completions \
     -H "Authorization: Bearer YOUR_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{
       "model": "moonshotai/kimi-k2.7-code",
       "messages": [{"role": "user", "content": "Hello"}]
     }'
```

如果返回正常响应（不是 "model not found" 错误），说明更新成功！

---

## 📦 提供的脚本文件

1. **check_database_before_update.sh** - 执行前检查脚本
2. **smart_update_kimi.sh** - 智能更新脚本（支持 MySQL 和 PostgreSQL）
3. **add_kimi_k27_code_model.sql** - 纯 SQL 脚本（可手动执行）
4. **README_KIMI_UPDATE.md** - 完整文档（本文件）

所有脚本都已添加执行权限，可直接运行。

---

## ⚠️ 注意事项

1. **必须先检查**：运行 `check_database_before_update.sh` 确认数据状况
2. **自动备份**：`smart_update_kimi.sh` 会自动备份到 `/tmp/channels_backup_*.sql`
3. **幂等性**：脚本可重复执行，不会产生重复数据
4. **缓存刷新**：如果启用了内存缓存，重建 abilities 后可能需要重启应用
5. **批量操作**：如果渠道数量超过 10000，建议分批执行或在低峰时段操作

---

## 🆘 常见问题

### Q1: 更新后用户请求新模型报错 "model not found"
**A**: 说明 abilities 表未重建，执行步骤 3 重建 abilities 表。

### Q2: 备份文件太大怎么办？
**A**: 备份文件位于 `/tmp/`，可以压缩后移动到其他位置：
```bash
gzip /tmp/channels_backup_*.sql
mv /tmp/channels_backup_*.sql.gz /path/to/backup/
```

### Q3: 如何回滚？
**A**: 使用备份文件恢复：
```bash
# PostgreSQL
psql -U postgres -d one_api < /tmp/channels_backup_*.sql

# MySQL
MYSQL_PWD="$DB_PASS" mysql -u "${DB_USER:-root}" "${DB_NAME:-one_api}" < /tmp/channels_backup_*.sql
```

### Q4: 能否只更新部分渠道？
**A**: 可以修改 SQL 添加 WHERE 条件，例如：
```sql
WHERE id IN (1,2,3) AND models NOT LIKE '%moonshotai/kimi-k2.7-code%'
```

---

## 📞 执行建议

推荐在服务器上按顺序执行：

```bash
# 1. 上传脚本到服务器
scp scripts/*.sh root@69.165.75.53:/tmp/

# 2. SSH 连接到服务器
ssh root@69.165.75.53

# 3. 执行检查脚本
bash /tmp/check_database_before_update.sh

# 4. 确认无误后执行更新
bash /tmp/smart_update_kimi.sh

# 5. 登录管理后台重建 abilities 表
# 或使用 API 调用
```

准备就绪！需要我帮你执行吗？
