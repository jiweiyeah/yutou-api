# 渠道模型和映射更新说明

## 概述

已生成用于为所有渠道添加 `moonshotai/kimi-k2.7-code` 模型及其映射的 SQL 脚本和 Shell 脚本。

## 文件说明

### 1. SQL 脚本
**文件路径**: `scripts/add_kimi_k27_code_model.sql`

- 直接 SQL 脚本，可在 MySQL 客户端中执行
- 包含详细注释和验证查询

### 2. Shell 脚本（推荐）
**文件路径**: `scripts/update_channels_kimi_k27.sh`

- 自动化执行脚本
- 包含错误处理和结果统计
- 执行后会显示更新统计和示例数据

## 执行步骤

### 在服务器 69.165.75.53 上执行：

```bash
# 方式 1: 上传并执行 Shell 脚本
scp scripts/update_channels_kimi_k27.sh root@69.165.75.53:/tmp/
ssh root@69.165.75.53 "bash /tmp/update_channels_kimi_k27.sh"

# 方式 2: 上传 SQL 并手动执行
scp scripts/add_kimi_k27_code_model.sql root@69.165.75.53:/tmp/
ssh root@69.165.75.53 'MYSQL_PWD="$DB_PASS" mysql -u "${DB_USER:-root}" "${DB_NAME:-one_api}" < /tmp/add_kimi_k27_code_model.sql'

# 方式 3: 直接通过 SSH 执行（推荐）
cat scripts/update_channels_kimi_k27.sh | ssh root@69.165.75.53 "bash -s"
```

## 更新内容

### 1. 模型字段 (models)
在每个渠道的 `models` 字段末尾追加 `,moonshotai/kimi-k2.7-code`

**示例**:
```
更新前: gpt-3.5-turbo,gpt-4
更新后: gpt-3.5-turbo,gpt-4,moonshotai/kimi-k2.7-code
```

### 2. 模型映射字段 (model_mapping)
添加映射规则: `moonshotai/kimi-k2.7-code` → `@cf/moonshotai/kimi-k2.7-code`

**示例**:
```json
更新前: {"gpt-4":"gpt-4-turbo"}
更新后: {"gpt-4":"gpt-4-turbo","moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}
```

## 重要提醒

### ⚠️ 执行后必须重建 abilities 表

更新 `channels` 表后，必须重建 `abilities` 表以保持数据一致性：

#### 方式 1: 管理后台（推荐）
1. 登录管理后台
2. 导航至 **渠道管理** 页面
3. 点击 **修复数据库一致性** 按钮

#### 方式 2: API 调用
```bash
curl -X POST http://your-domain/api/channel/fix \
  -H "Authorization: Bearer YOUR_ADMIN_TOKEN"
```

#### 方式 3: 直接执行 SQL
```sql
-- 清空 abilities 表并根据 channels 重建
TRUNCATE TABLE abilities;
-- 然后通过应用程序调用 FixAbility() 函数
```

## 验证结果

执行脚本后会显示统计信息：
- `total_channels`: 总渠道数
- `channels_with_new_model`: 包含新模型的渠道数
- `channels_with_new_mapping`: 包含新映射的渠道数

前 5 个渠道的数据示例也会显示，用于人工验证。

## 回滚（如需要）

如果需要回滚，可使用以下 SQL：

```sql
-- 移除模型
UPDATE channels
SET models = REPLACE(models, ',moonshotai/kimi-k2.7-code', '')
WHERE models LIKE '%,moonshotai/kimi-k2.7-code%';

-- 移除映射（需要手动处理 JSON）
-- 建议在回滚前备份数据
```

## 注意事项

1. **备份优先**: 建议执行前先备份 `channels` 表
   ```bash
   MYSQL_PWD="$DB_PASS" mysqldump -u "${DB_USER:-root}" "${DB_NAME:-one_api}" channels > channels_backup.sql
   ```

2. **幂等性**: 脚本具有幂等性，重复执行不会产生重复数据

3. **性能**: 如果渠道数量巨大（>10000），建议分批执行

4. **缓存刷新**: 如果启用了内存缓存，更新后需重启应用以刷新缓存

## 数据库连接信息

- **主机**: 69.165.75.53
- **用户**: root
- **密码**: 通过环境变量 `DB_PASS` 提供，不写入仓库
- **数据库**: one_api
