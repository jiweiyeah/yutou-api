#!/bin/bash

# =====================================================
# 智能更新脚本 - 支持 MySQL 和 PostgreSQL
# 为所有渠道添加 kimi-k2.7-code 模型和映射
# =====================================================

set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${GREEN}=========================================="
echo "智能更新脚本 - kimi-k2.7-code 模型"
echo -e "==========================================${NC}"
echo ""

# 检测数据库类型
echo -e "${YELLOW}[1/5] 检测数据库类型...${NC}"

if command -v psql &> /dev/null && psql -U postgres -d one_api -c '\q' 2>/dev/null; then
    echo -e "${BLUE}检测到 PostgreSQL 数据库${NC}"
    DB_TYPE="postgres"
elif command -v mysql &> /dev/null; then
    echo -e "${BLUE}检测到 MySQL 数据库${NC}"
    DB_TYPE="mysql"
    DB_USER=${DB_USER:-root}
    DB_PASS=${DB_PASS:?请通过环境变量 DB_PASS 提供数据库密码}
    DB_NAME=${DB_NAME:-one_api}
    export MYSQL_PWD="$DB_PASS"
else
    echo -e "${RED}错误: 未找到可用的数据库客户端${NC}"
    exit 1
fi

echo -e "${GREEN}✓ 数据库类型: $DB_TYPE${NC}"
echo ""

# 函数：执行查询
run_query() {
    if [ "$DB_TYPE" = "postgres" ]; then
        psql -U postgres -d one_api -t -A -c "$1"
    else
        mysql -u "$DB_USER" "$DB_NAME" -sN -e "$1"
    fi
}

# 备份
echo -e "${YELLOW}[2/5] 备份 channels 表...${NC}"
BACKUP_FILE="/tmp/channels_backup_$(date +%Y%m%d_%H%M%S).sql"
if [ "$DB_TYPE" = "postgres" ]; then
    pg_dump -U postgres -d one_api -t channels > "$BACKUP_FILE"
else
    mysqldump -u "$DB_USER" "$DB_NAME" channels > "$BACKUP_FILE"
fi
echo -e "${GREEN}✓ 备份完成: $BACKUP_FILE${NC}"
echo ""

# 检查当前状态
echo -e "${YELLOW}[3/5] 检查当前数据状态...${NC}"
TOTAL=$(run_query "SELECT COUNT(*) FROM channels;")
EXISTING=$(run_query "SELECT COUNT(*) FROM channels WHERE models LIKE '%moonshotai/kimi-k2.7-code%';")
TO_UPDATE=$((TOTAL - EXISTING))

echo -e "${BLUE}总渠道数: $TOTAL${NC}"
echo -e "${BLUE}已有该模型: $EXISTING${NC}"
echo -e "${BLUE}需要更新: $TO_UPDATE${NC}"
echo ""

if [ $TO_UPDATE -eq 0 ]; then
    echo -e "${GREEN}所有渠道已包含该模型，无需更新！${NC}"
    exit 0
fi

# 更新 models 字段
echo -e "${YELLOW}[4/5] 更新 models 字段...${NC}"

if [ "$DB_TYPE" = "postgres" ]; then
    # PostgreSQL 版本
    psql -U postgres -d one_api <<'PGSQL'
UPDATE channels
SET models = CASE
    WHEN models = '' OR models IS NULL THEN 'moonshotai/kimi-k2.7-code'
    WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN models
    ELSE models || ',moonshotai/kimi-k2.7-code'
END
WHERE models NOT LIKE '%moonshotai/kimi-k2.7-code%' OR models IS NULL OR models = '';
PGSQL
else
    # MySQL 版本
    mysql -u "$DB_USER" "$DB_NAME" <<'MYSQL'
UPDATE channels
SET models = CASE
    WHEN models = '' OR models IS NULL THEN 'moonshotai/kimi-k2.7-code'
    WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN models
    ELSE CONCAT(models, ',moonshotai/kimi-k2.7-code')
END
WHERE models NOT LIKE '%moonshotai/kimi-k2.7-code%' OR models IS NULL OR models = '';
MYSQL
fi

UPDATED_MODELS=$(run_query "SELECT COUNT(*) FROM channels WHERE models LIKE '%moonshotai/kimi-k2.7-code%';")
echo -e "${GREEN}✓ models 字段更新完成，当前包含该模型的渠道数: $UPDATED_MODELS${NC}"
echo ""

# 更新 model_mapping 字段
echo -e "${YELLOW}[5/5] 更新 model_mapping 字段...${NC}"

if [ "$DB_TYPE" = "postgres" ]; then
    # PostgreSQL 版本
    psql -U postgres -d one_api <<'PGSQL'
UPDATE channels
SET model_mapping = CASE
    WHEN model_mapping IS NULL OR model_mapping = '' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    WHEN model_mapping = '{}' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    WHEN model_mapping LIKE '%"moonshotai/kimi-k2.7-code"%' THEN model_mapping
    ELSE SUBSTRING(model_mapping, 1, LENGTH(model_mapping) - 1) || ',"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
END
WHERE model_mapping NOT LIKE '%"moonshotai/kimi-k2.7-code"%' OR model_mapping IS NULL OR model_mapping = '' OR model_mapping = '';
PGSQL
else
    # MySQL 版本
    mysql -u "$DB_USER" "$DB_NAME" <<'MYSQL'
UPDATE channels
SET model_mapping = CASE
    WHEN model_mapping IS NULL OR model_mapping = '' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    WHEN model_mapping = '{}' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    WHEN model_mapping LIKE '%"moonshotai/kimi-k2.7-code"%' THEN model_mapping
    ELSE CONCAT(
        SUBSTRING(model_mapping, 1, LENGTH(model_mapping) - 1),
        ',"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    )
END
WHERE model_mapping NOT LIKE '%"moonshotai/kimi-k2.7-code"%' OR model_mapping IS NULL OR model_mapping = '' OR model_mapping = '{}';
MYSQL
fi

UPDATED_MAPPING=$(run_query "SELECT COUNT(*) FROM channels WHERE model_mapping LIKE '%moonshotai/kimi-k2.7-code%';")
echo -e "${GREEN}✓ model_mapping 字段更新完成，当前包含该映射的渠道数: $UPDATED_MAPPING${NC}"
echo ""

# 验证结果
echo -e "${YELLOW}📊 更新结果统计:${NC}"
if [ "$DB_TYPE" = "postgres" ]; then
    psql -U postgres -d one_api -c "
        SELECT
            COUNT(*) as total_channels,
            SUM(CASE WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_model,
            SUM(CASE WHEN model_mapping LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_mapping
        FROM channels;
    "
else
    mysql -u "$DB_USER" "$DB_NAME" -t -e "
        SELECT
            COUNT(*) as total_channels,
            SUM(CASE WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_model,
            SUM(CASE WHEN model_mapping LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_mapping
        FROM channels;
    "
fi
echo ""

echo -e "${GREEN}=========================================="
echo "✓ 更新完成！"
echo -e "==========================================${NC}"
echo ""

# 重要提醒
echo -e "${RED}⚠️  重要：必须重建 abilities 表！${NC}"
echo ""
echo -e "${YELLOW}什么是 abilities 表？${NC}"
echo "abilities 是模型调度的核心索引表，功能："
echo "  • 将 channels.models（逗号分隔字符串）展开为独立记录"
echo "  • 每个 (group, model, channel_id) 组合一条记录"
echo "  • 提供快速的模型匹配查询（通过索引）"
echo "  • 支持优先级和权重调度"
echo ""
echo -e "${YELLOW}为什么要重建？${NC}"
echo "  • channels.models 已更新，但 abilities 不会自动同步"
echo "  • 新增的 'moonshotai/kimi-k2.7-code' 不在 abilities 中"
echo "  • 用户请求该模型时会找不到可用渠道"
echo "  • 必须调用 FixAbility() 重新生成索引"
echo ""
echo -e "${YELLOW}如何重建？选择以下方式之一：${NC}"
echo ""
echo "方式 1（推荐）: 管理后台"
echo "  1. 登录管理后台"
echo "  2. 进入「渠道管理」页面"
echo "  3. 点击「修复数据库一致性」按钮"
echo ""
echo "方式 2: API 调用"
echo "  curl -X POST http://your-domain/api/channel/fix \\"
echo "       -H 'Authorization: Bearer YOUR_ADMIN_TOKEN'"
echo ""
echo "方式 3: 数据库直接操作（仅了解原理）"
echo "  # 注意：这只是清空，还需要应用程序调用 AddAbilities()"
if [ "$DB_TYPE" = "postgres" ]; then
    echo "  psql -U postgres -d one_api -c 'TRUNCATE TABLE abilities;'"
else
    echo "  MYSQL_PWD=\"\$DB_PASS\" mysql -u \"\$DB_USER\" \"\$DB_NAME\" -e 'TRUNCATE TABLE abilities;'"
fi
echo ""
echo -e "${GREEN}备份文件位置: $BACKUP_FILE${NC}"
echo ""
