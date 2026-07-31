#!/bin/bash

# =====================================================
# 为所有渠道新增模型并添加模型映射的执行脚本
# 服务器: 69.165.75.53
# 数据库: one_api
# =====================================================

set -e  # 遇到错误立即退出

DB_HOST=${DB_HOST:-127.0.0.1}
DB_USER=${DB_USER:-root}
DB_PASS=${DB_PASS:?请通过环境变量 DB_PASS 提供数据库密码}
DB_NAME=${DB_NAME:-one_api}
export MYSQL_PWD="$DB_PASS"

echo "=========================================="
echo "开始更新渠道模型和模型映射"
echo "=========================================="

# 执行 SQL 更新
mysql -h "$DB_HOST" -u "$DB_USER" "$DB_NAME" <<'EOF'

-- 步骤 1: 为所有渠道的 models 字段添加新模型
UPDATE channels
SET models = CASE
    WHEN models = '' OR models IS NULL THEN 'moonshotai/kimi-k2.7-code'
    WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN models
    ELSE CONCAT(models, ',moonshotai/kimi-k2.7-code')
END
WHERE models NOT LIKE '%moonshotai/kimi-k2.7-code%' OR models IS NULL OR models = '';

-- 步骤 2: 为所有渠道的 model_mapping 字段添加映射规则
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

-- 步骤 3: 验证更新结果
SELECT '========== 更新统计 ==========' as info;
SELECT
    COUNT(*) as total_channels,
    SUM(CASE WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as channels_with_new_model,
    SUM(CASE WHEN model_mapping LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as channels_with_new_mapping
FROM channels;

SELECT '========== 示例渠道数据 ==========' as info;
SELECT
    id,
    name,
    SUBSTRING(models, 1, 100) as models_preview,
    SUBSTRING(model_mapping, 1, 150) as mapping_preview
FROM channels
LIMIT 5;

EOF

echo ""
echo "=========================================="
echo "更新完成！"
echo "=========================================="
echo ""
echo "接下来需要手动重建 abilities 表:"
echo "1. 登录管理后台"
echo "2. 进入 '渠道管理'"
echo "3. 点击 '修复数据库一致性' 按钮"
echo ""
echo "或者通过 API 调用:"
echo "curl -X POST http://your-domain/api/channel/fix -H 'Authorization: Bearer YOUR_TOKEN'"
echo ""
