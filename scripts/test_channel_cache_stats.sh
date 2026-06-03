#!/bin/bash

# 渠道缓存性能监控测试脚本
# 用法: ./test_channel_cache_stats.sh [BASE_URL] [ROOT_TOKEN]

BASE_URL=${1:-"http://localhost:3000"}
ROOT_TOKEN=${2:-""}

if [ -z "$ROOT_TOKEN" ]; then
    echo "错误: 请提供 ROOT_TOKEN"
    echo "用法: $0 [BASE_URL] ROOT_TOKEN"
    exit 1
fi

echo "=========================================="
echo "渠道缓存性能监控测试"
echo "=========================================="
echo "Base URL: $BASE_URL"
echo ""

# 1. 获取当前统计数据
echo "1. 获取统计数据..."
echo ""
STATS=$(curl -s -H "Authorization: Bearer $ROOT_TOKEN" \
     "$BASE_URL/api/option/channel_cache_stats")

echo "$STATS" | jq '.'

# 2. 提取关键指标
if command -v jq &> /dev/null; then
    echo ""
    echo "=========================================="
    echo "关键指标分析"
    echo "=========================================="

    SELECTION_COUNT=$(echo "$STATS" | jq -r '.data.selection_count // 0')
    SELECTION_AVG_MS=$(echo "$STATS" | jq -r '.data.selection_avg_ms // 0')
    SLOW_COUNT=$(echo "$STATS" | jq -r '.data.slow_selection_count // 0')
    ERROR_COUNT=$(echo "$STATS" | jq -r '.data.selection_error_count // 0')
    DISABLE_COUNT=$(echo "$STATS" | jq -r '.data.channel_disable_count // 0')
    ENABLE_COUNT=$(echo "$STATS" | jq -r '.data.channel_enable_count // 0')
    LOCK_WAIT=$(echo "$STATS" | jq -r '.data.lock_wait_count // 0')
    SYNC_AVG_MS=$(echo "$STATS" | jq -r '.data.sync_avg_ms // 0')
    LAST_SYNC_CHANNELS=$(echo "$STATS" | jq -r '.data.last_sync_channel_count // 0')

    echo "选择统计:"
    echo "  - 总选择次数: $SELECTION_COUNT"
    echo "  - 平均耗时: ${SELECTION_AVG_MS}ms"
    echo "  - 慢查询次数: $SLOW_COUNT"
    echo "  - 错误次数: $ERROR_COUNT"

    if [ "$SELECTION_COUNT" -gt 0 ]; then
        SLOW_RATIO=$(echo "scale=2; $SLOW_COUNT * 100 / $SELECTION_COUNT" | bc)
        ERROR_RATIO=$(echo "scale=2; $ERROR_COUNT * 100 / $SELECTION_COUNT" | bc)
        echo "  - 慢查询比例: ${SLOW_RATIO}%"
        echo "  - 错误率: ${ERROR_RATIO}%"
    fi

    echo ""
    echo "渠道状态变更:"
    echo "  - 禁用次数: $DISABLE_COUNT"
    echo "  - 启用次数: $ENABLE_COUNT"
    echo "  - 总变更次数: $((DISABLE_COUNT + ENABLE_COUNT))"

    echo ""
    echo "锁竞争:"
    echo "  - 锁等待次数: $LOCK_WAIT"
    if [ "$SELECTION_COUNT" -gt 0 ]; then
        LOCK_RATIO=$(echo "scale=2; $LOCK_WAIT * 100 / $SELECTION_COUNT" | bc)
        echo "  - 锁等待比例: ${LOCK_RATIO}%"
    fi

    echo ""
    echo "缓存同步:"
    echo "  - 平均同步耗时: ${SYNC_AVG_MS}ms"
    echo "  - 当前渠道数: $LAST_SYNC_CHANNELS"

    echo ""
    echo "=========================================="
    echo "性能评估"
    echo "=========================================="

    NEED_OPTIMIZATION=0

    # 检查选择耗时
    if (( $(echo "$SELECTION_AVG_MS > 5" | bc -l) )); then
        echo "⚠️  选择耗时较高 (${SELECTION_AVG_MS}ms > 5ms)"
        echo "   建议: 实施预计算权重索引优化"
        NEED_OPTIMIZATION=1
    else
        echo "✅ 选择耗时正常 (${SELECTION_AVG_MS}ms)"
    fi

    # 检查慢查询比例
    if [ "$SELECTION_COUNT" -gt 0 ]; then
        if (( $(echo "$SLOW_RATIO > 10" | bc -l) )); then
            echo "⚠️  慢查询比例较高 (${SLOW_RATIO}% > 10%)"
            echo "   建议: 实施预计算权重索引优化"
            NEED_OPTIMIZATION=1
        else
            echo "✅ 慢查询比例正常 (${SLOW_RATIO}%)"
        fi
    fi

    # 检查锁竞争
    if [ "$SELECTION_COUNT" -gt 0 ]; then
        if (( $(echo "$LOCK_RATIO > 50" | bc -l) )); then
            echo "⚠️  锁竞争较高 (锁等待比例 ${LOCK_RATIO}% > 50%)"
            echo "   建议: 缩短 SYNC_FREQUENCY 或实施分片锁"
            NEED_OPTIMIZATION=1
        else
            echo "✅ 锁竞争正常 (锁等待比例 ${LOCK_RATIO}%)"
        fi
    fi

    # 检查同步耗时
    if (( $(echo "$SYNC_AVG_MS > 1000" | bc -l) )); then
        echo "⚠️  缓存同步较慢 (${SYNC_AVG_MS}ms > 1000ms)"
        echo "   建议: 优化同步逻辑"
        NEED_OPTIMIZATION=1
    else
        echo "✅ 缓存同步正常 (${SYNC_AVG_MS}ms)"
    fi

    # 检查渠道变更频率（假设运行了至少 1 小时）
    if [ "$SELECTION_COUNT" -gt 3600 ]; then
        # 粗略估计运行时间（假设每秒 1 次选择）
        RUNTIME_MINUTES=$((SELECTION_COUNT / 60))
        if [ "$RUNTIME_MINUTES" -gt 0 ]; then
            DISABLE_PER_MIN=$((DISABLE_COUNT / RUNTIME_MINUTES))
            if [ "$DISABLE_PER_MIN" -gt 10 ]; then
                echo "⚠️  渠道禁用频率较高 (约 ${DISABLE_PER_MIN} 次/分钟 > 10)"
                echo "   建议: 缩短 SYNC_FREQUENCY 到 10 秒"
                NEED_OPTIMIZATION=1
            else
                echo "✅ 渠道状态变更频率正常"
            fi
        fi
    fi

    echo ""
    if [ "$NEED_OPTIMIZATION" -eq 0 ]; then
        echo "🎉 性能表现良好，暂无需优化"
    else
        echo "💡 建议查看 docs/channel_cache_monitoring.md 了解优化方案"
    fi
else
    echo ""
    echo "提示: 安装 jq 可获得更详细的分析"
    echo "      brew install jq  (macOS)"
    echo "      apt install jq   (Ubuntu)"
fi

echo ""
echo "=========================================="
echo "测试完成"
echo "=========================================="
