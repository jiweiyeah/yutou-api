-- =====================================================
-- 为所有渠道新增模型 moonshotai/kimi-k2.7-code
-- 并添加模型映射: moonshotai/kimi-k2.7-code -> @cf/moonshotai/kimi-k2.7-code
-- =====================================================

-- 步骤 1: 为所有渠道的 models 字段添加新模型（如果尚未存在）
UPDATE channels
SET models = CASE
    -- 如果 models 为空，直接设置为新模型
    WHEN models = '' OR models IS NULL THEN 'moonshotai/kimi-k2.7-code'
    -- 如果 models 已包含该模型，保持不变
    WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN models
    -- 否则，在末尾追加新模型（确保逗号分隔）
    ELSE CONCAT(models, ',moonshotai/kimi-k2.7-code')
END
WHERE models NOT LIKE '%moonshotai/kimi-k2.7-code%' OR models IS NULL OR models = '';

-- 步骤 2: 为所有渠道的 model_mapping 字段添加映射规则
UPDATE channels
SET model_mapping = CASE
    -- 如果 model_mapping 为空或 null，创建新的 JSON 对象
    WHEN model_mapping IS NULL OR model_mapping = '' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    -- 如果是空的 JSON 对象 {}
    WHEN model_mapping = '{}' THEN '{"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    -- 如果已经包含该映射，保持不变
    WHEN model_mapping LIKE '%"moonshotai/kimi-k2.7-code"%' THEN model_mapping
    -- 否则，在现有 JSON 对象中添加新的键值对
    ELSE CONCAT(
        SUBSTRING(model_mapping, 1, LENGTH(model_mapping) - 1),
        ',"moonshotai/kimi-k2.7-code":"@cf/moonshotai/kimi-k2.7-code"}'
    )
END
WHERE model_mapping NOT LIKE '%"moonshotai/kimi-k2.7-code"%' OR model_mapping IS NULL OR model_mapping = '' OR model_mapping = '{}';

-- 步骤 3: 验证更新结果
-- 查看更新后的渠道数量
SELECT
    COUNT(*) as total_channels,
    SUM(CASE WHEN models LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as channels_with_new_model,
    SUM(CASE WHEN model_mapping LIKE '%moonshotai/kimi-k2.7-code%' THEN 1 ELSE 0 END) as channels_with_new_mapping
FROM channels;

-- 查看前 5 个渠道的 models 和 model_mapping（用于验证）
SELECT
    id,
    name,
    models,
    model_mapping
FROM channels
LIMIT 5;
