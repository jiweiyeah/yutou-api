-- =====================================================
-- 为所有渠道新增模型 zai-org/glm-5.2
-- 并添加模型映射: zai-org/glm-5.2 -> @cf/zai-org/glm-5.2
--
-- 目标库: PostgreSQL (容器 new-api-postgres, 库 newapi)
-- 执行: docker exec -i -e PGPASSWORD=*** new-api-postgres \
--          psql -U newapi -d newapi < add_glm52_model.sql
--
-- 特性: 幂等(可重复执行) + 单事务(全成功或全回滚)
-- 生效: MemoryCacheEnabled=false, 调度直查 DB abilities 表,
--       COMMIT 后即时生效, 无需重启容器 / 无需调用 /api/channel/fix
-- =====================================================
\set ON_ERROR_STOP on
BEGIN;

-- 步骤 1: models 字段追加新模型 (精确按逗号分隔的 token 匹配, 幂等)
UPDATE channels
SET models = models || ',zai-org/glm-5.2'
WHERE ',' || models || ',' NOT LIKE '%,zai-org/glm-5.2,%';

-- 步骤 2: model_mapping 追加映射键值对
--   去尾部空白 -> 去掉末尾的 '}' -> 拼接 ,"key":"value"}
--   (已验证全部 9526 行均为以 '}' 结尾的合法 JSON 对象)
UPDATE channels
SET model_mapping = left(rtrim(model_mapping), length(rtrim(model_mapping)) - 1)
                    || ',"zai-org/glm-5.2":"@cf/zai-org/glm-5.2"}'
WHERE model_mapping NOT LIKE '%"zai-org/glm-5.2"%';

-- 步骤 3: abilities 调度索引表增量插入新模型记录
--   按 channels.group 拆分(当前仅 default), enabled = (status=1),
--   priority/weight/tag 与渠道一致, 与 AddAbilities() 行为对齐
INSERT INTO abilities ("group", model, channel_id, enabled, priority, weight, tag)
SELECT DISTINCT trim(g.grp), 'zai-org/glm-5.2', c.id, (c.status = 1), c.priority, c.weight, c.tag
FROM channels c
CROSS JOIN LATERAL unnest(string_to_array(c."group", ',')) AS g(grp)
WHERE ',' || c.models || ',' LIKE '%,zai-org/glm-5.2,%'
ON CONFLICT ("group", model, channel_id) DO NOTHING;

COMMIT;
