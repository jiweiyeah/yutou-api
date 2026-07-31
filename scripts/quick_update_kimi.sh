#!/bin/bash

# =====================================================
# 一键更新脚本 - 为所有渠道添加 kimi-k2.7-code 模型
# 使用方法: 在服务器上执行此脚本
# =====================================================

set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 数据库配置
DB_USER=${DB_USER:-root}
DB_PASS=${DB_PASS:?请通过环境变量 DB_PASS 提供数据库密码}
DB_NAME=${DB_NAME:-one_api}
export MYSQL_PWD="$DB_PASS"

echo -e "${GREEN}=========================================="
echo "开始为所有渠道添加 kimi-k2.7-code 模型"
echo -e "==========================================${NC}"
echo ""

# 步骤 1: 备份
echo -e "${YELLOW}[1/4] 备份 channels 表...${NC}"
mysqldump -u "$DB_USER" "$DB_NAME" channels > /tmp/channels_backup_$(date +%Y%m%d_%H%M%S).sql
echo -e "${GREEN}✓ 备份完成: /tmp/channels_backup_*.sql${NC}"
echo ""

# 步骤 2: 更新 models 字段
echo -e "${YELLOW}[2/4] 更新 models 字段...${NC}"
ROWS_AFFECTED=$(mysql -u "$DB_USER" "$DB_NAME" -sNe "
UPDATE channels
SET models = CASE
    WHEN models = '' OR models IS NULL THEN 'moonshotai/kimi-k2.7-code'
    WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN models
    ELSE CONCAT(models, ',moonshotai/kimi-k2.7-code')
END
WHERE models NOT LIKE '%moonshotai/kimi-k2.7-code%' OR models IS NULL OR models = '';
SELECT ROW_COUNT();
")
echo -e "${GREEN}✓ 已更新 $ROWS_AFFECTED 个渠道的 models 字段${NC}"
echo ""

# 步骤 3: 更新 model_mapping 字段
echo -e "${YELLOW}[3/4] 更新 model_mapping 字段...${NC}"
ROWS_AFFECTED=$(mysql -u "$DB_USER" "$DB_NAME" -sNe "
UPDATE channels
SET model_mapping = CASE
    WHEN model_mapping IS NULL OR model_mapping = '' THEN '{\"moonshotai/kimi-k2.7-code\":\"@cf/moonshotai/kimi-k2.7-code\"}'
    WHEN model_mapping = '{}' THEN '{\"moonshotai/kimi-k2.7-code\":\"@cf/moonshotai/kimi-k2.7-code\"}'
    WHEN model_mapping LIKE '%\"moonshotai/kimi-k2.7-code\"%' THEN model_mapping
    ELSE CONCAT(
        SUBSTRING(model_mapping, 1, LENGTH(model_mapping) - 1),
        ',\"moonshotai/kimi-k2.7-code\":\"@cf/moonshotai/kimi-k2.7-code\"}'
    )
END
WHERE model_mapping NOT LIKE '%\"moonshotai/kimi-k2.7-code\"%' OR model_mapping IS NULL OR model_mapping = '' OR model_mapping = '{}';
SELECT ROW_COUNT();
")
echo -e "${GREEN}✓ 已更新 $ROWS_AFFECTED 个渠道的 model_mapping 字段${NC}"
echo ""

# 步骤 4: 验证结果
echo -e "${YELLOW}[4/4] 验证更新结果...${NC}"
mysql -u "$DB_USER" "$DB_NAME" -t <<'EOF'
SELECT
    COUNT(*) as total_channels,
    SUM(CASE WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_new_model,
    SUM(CASE WHEN model_mapping LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as with_new_mapping
FROM channels;
EOF
echo ""

echo -e "${GREEN}=========================================="
echo "✓ 更新完成！"
echo -e "==========================================${NC}"
echo ""
echo -e "${RED}⚠️  重要提醒：${NC}"
echo "更新完成后必须重建 abilities 表以保持数据一致性"
echo ""
echo "请执行以下操作之一："
echo "  1. 登录管理后台 -> 渠道管理 -> 点击'修复数据库一致性'"
echo "  2. 或调用 API: curl -X POST http://your-domain/api/channel/fix -H 'Authorization: Bearer TOKEN'"
echo ""
