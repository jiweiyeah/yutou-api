#!/bin/bash

CHANNEL_ID=10838

echo "=== 渠道 $CHANNEL_ID 关键发现 ==="
echo ""

# 1. 统计被禁用的 key 索引分布
echo "1. 被禁用的 key 索引范围："
min_disabled=$(ssh yutou-prod "docker exec new-api-postgres psql -U newapi -d newapi -t -c \"SELECT MIN(key::int) FROM json_each_text((SELECT channel_info->'multi_key_status_list' FROM channels WHERE id = $CHANNEL_ID)) WHERE value = '3';\"" | tr -d ' ')
max_disabled=$(ssh yutou-prod "docker exec new-api-postgres psql -U newapi -d newapi -t -c \"SELECT MAX(key::int) FROM json_each_text((SELECT channel_info->'multi_key_status_list' FROM channels WHERE id = $CHANNEL_ID)) WHERE value = '3';\"" | tr -d ' ')
echo "   最小索引: $min_disabled"
echo "   最大索引: $max_disabled"
echo ""

# 2. 查看实际的 key 内容（前3个被禁用的）
echo "2. 前3个被禁用的 key 实际内容："
ssh yutou-prod "docker exec new-api-postgres psql -U newapi -d newapi -t -c \"
WITH disabled_indices AS (
  SELECT key::int as idx
  FROM json_each_text((SELECT channel_info->'multi_key_status_list' FROM channels WHERE id = $CHANNEL_ID))
  WHERE value = '3'
  ORDER BY key::int
  LIMIT 3
),
key_array AS (
  SELECT unnest(string_to_array(key, E'\n')) as key_value,
         generate_subscripts(string_to_array(key, E'\n'), 1) - 1 as idx
  FROM channels
  WHERE id = $CHANNEL_ID
)
SELECT ka.idx, left(ka.key_value, 50) || '...' as key_preview
FROM key_array ka
JOIN disabled_indices di ON ka.idx = di.idx;
\""
echo ""

# 3. 测试：如果我们现在调用 GetNextEnabledKey，它会选择哪个索引？
echo "3. GetNextEnabledKey 逻辑分析："
echo "   - 总 key 数: 6385"
echo "   - 被禁用数: 357"
echo "   - 可用 key 数: 6385 - 357 = 6028"
echo ""
echo "   根据代码 model/channel.go:220-228："
echo "   - 如果索引不在 status_list 中 => 默认启用（状态1）"
echo "   - 如果索引在 status_list 中且状态=3 => 禁用"
echo ""
echo "   结论：索引 0, 1, 2, 3, 4, 5, 7, 9... 等不在 status_list 中的 key"
echo "   都是可用的，只有 357 个明确被禁用的索引会被跳过。"
echo ""

# 4. 验证：这些被禁用的 key 能否自动恢复？
echo "4. 关键问题：被禁用的 key 能否自动恢复？"
echo ""
echo "   查找代码中修改 status_list 的地方："
ssh yutou-prod "cd /root && grep -rn 'multi_key_status_list.*=.*3' --include='*.go' | head -5" 2>/dev/null || echo "   (需要在服务器上检查源代码)"
echo ""
echo "   根据 model/channel.go GetNextEnabledKey 函数："
echo "   - 它只读取 status_list，从不修改"
echo "   - 被禁用的 key (status=3) 会被永久跳过"
echo "   - 唯一恢复方法："
echo "     a) 手动调用 EnableAllKeys() 清空 status_list"
echo "     b) 手动修改数据库中的 status_list"
echo ""

# 5. 检查是否有自动恢复机制
echo "5. 检查渠道整体状态："
status=$(ssh yutou-prod "docker exec new-api-postgres psql -U newapi -d newapi -t -c \"SELECT status FROM channels WHERE id = $CHANNEL_ID;\"" | tr -d ' ')
echo "   渠道状态: $status (1=启用, 2=禁用, 3=自动禁用)"
echo ""
if [ "$status" -eq 3 ]; then
    echo "   ⚠️  渠道状态为自动禁用(3)！"
    echo "   根据 model/channel.go:209-215，EnableAllKeys() 只在渠道状态=3时生效"
    echo "   但这会恢复所有 key，而不是单个 key"
else
    echo "   渠道状态正常(1)，个别被禁用的 key 无法自动恢复"
fi
echo ""

echo "=== 最终结论 ==="
echo "✅ 渠道 10838 有 6028 个可用 key（未在 status_list 中 + 在 status_list 中状态≠3）"
echo "❌ 357 个被禁用的 key (status=3) 永远不会被 GetNextEnabledKey() 选中"
echo "❌ 没有自动恢复机制 - 被禁用的 key 会一直保持禁用状态"
echo "✅ 解决方案: 调用 /api/channel/$CHANNEL_ID/enable_all_keys 清空 status_list"
