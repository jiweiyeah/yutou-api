#!/bin/bash
# 动态代理切换功能测试脚本

set -e

API_BASE="http://localhost:3000"
ADMIN_TOKEN=""  # 需要管理员 token

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=========================================="
echo "动态代理切换功能测试"
echo "=========================================="

# 检查参数
if [ -z "$1" ]; then
    echo "用法: $0 <ADMIN_TOKEN>"
    exit 1
fi

ADMIN_TOKEN=$1

# 测试函数
test_proxy_stats() {
    local channel_id=$1
    echo ""
    echo -e "${YELLOW}[测试] 查询渠道 $channel_id 代理统计${NC}"

    response=$(curl -s -w "\nHTTP_CODE:%{http_code}" \
        "$API_BASE/api/option/proxy_stats/$channel_id" \
        -H "Authorization: Bearer $ADMIN_TOKEN")

    http_code=$(echo "$response" | grep "HTTP_CODE" | cut -d: -f2)
    body=$(echo "$response" | sed '/HTTP_CODE/d')

    if [ "$http_code" = "200" ]; then
        echo -e "${GREEN}✓ 成功${NC}"
        echo "$body" | jq '.'
    else
        echo -e "${RED}✗ 失败 (HTTP $http_code)${NC}"
        echo "$body"
        return 1
    fi
}

test_proxy_reset() {
    local channel_id=$1
    echo ""
    echo -e "${YELLOW}[测试] 重置渠道 $channel_id 代理状态${NC}"

    response=$(curl -s -w "\nHTTP_CODE:%{http_code}" -X POST \
        "$API_BASE/api/option/proxy_stats/$channel_id/reset" \
        -H "Authorization: Bearer $ADMIN_TOKEN")

    http_code=$(echo "$response" | grep "HTTP_CODE" | cut -d: -f2)
    body=$(echo "$response" | sed '/HTTP_CODE/d')

    if [ "$http_code" = "200" ]; then
        echo -e "${GREEN}✓ 成功${NC}"
        echo "$body" | jq '.'
    else
        echo -e "${RED}✗ 失败 (HTTP $http_code)${NC}"
        echo "$body"
        return 1
    fi
}

# 1. 检查 xray-proxy 服务状态
echo ""
echo -e "${YELLOW}[检查] xray-proxy 服务状态${NC}"
if systemctl is-active --quiet xray-proxy 2>/dev/null; then
    echo -e "${GREEN}✓ xray-proxy 服务运行中${NC}"
else
    echo -e "${RED}✗ xray-proxy 服务未运行${NC}"
    echo "请先运行: sudo systemctl start xray-proxy"
    exit 1
fi

# 2. 检查代理端口
echo ""
echo -e "${YELLOW}[检查] 代理端口监听状态${NC}"
if ss -tlnp 2>/dev/null | grep -q ":10808"; then
    echo -e "${GREEN}✓ SOCKS5 代理端口 10808 监听中${NC}"
else
    echo -e "${RED}✗ SOCKS5 代理端口 10808 未监听${NC}"
    exit 1
fi

if ss -tlnp 2>/dev/null | grep -q ":10809"; then
    echo -e "${GREEN}✓ HTTP 代理端口 10809 监听中${NC}"
else
    echo -e "${RED}✗ HTTP 代理端口 10809 未监听${NC}"
    exit 1
fi

# 3. 测试代理连通性
echo ""
echo -e "${YELLOW}[检查] 代理连通性测试${NC}"
PROXY_IP=$(curl -s -x socks5://127.0.0.1:10808 --max-time 10 https://api.ipify.org || echo "FAILED")
LOCAL_IP=$(curl -s --max-time 10 https://api.ipify.org || echo "FAILED")

if [ "$PROXY_IP" != "FAILED" ] && [ "$PROXY_IP" != "" ]; then
    echo -e "${GREEN}✓ 代理连接成功${NC}"
    echo "  本机 IP: $LOCAL_IP"
    echo "  代理 IP: $PROXY_IP"
else
    echo -e "${RED}✗ 代理连接失败${NC}"
    exit 1
fi

# 4. 测试 API 端点
echo ""
echo "=========================================="
echo "API 端点测试"
echo "=========================================="

# 测试渠道 10835 (bitdeer-free)
test_proxy_stats 10835

# 测试渠道 10836 (bitdeer)
test_proxy_stats 10836

# 测试重置功能
test_proxy_reset 10836

# 再次查询确认重置
test_proxy_stats 10836

# 5. 检查环境变量
echo ""
echo "=========================================="
echo "环境变量检查"
echo "=========================================="

echo ""
echo -e "${YELLOW}[检查] Docker 容器环境变量${NC}"
docker exec new-api env | grep -E "BITDEER_|PROXY" || echo "未配置相关环境变量"

# 6. 模拟 429 场景测试（可选）
echo ""
echo "=========================================="
echo "功能说明"
echo "=========================================="
echo ""
echo "动态代理切换工作流程:"
echo "1. 正常情况下，10835/10836 渠道使用本机 IP 直连"
echo "2. 当检测到连续 N 次 429 错误时，自动切换到代理模式"
echo "3. 代理模式下会应用 100-500ms 随机排队延迟"
echo "4. 请求会通过 Xray 代理，使用不同的出口 IP"
echo "5. 当连续 M 次请求成功后，自动恢复直连模式"
echo ""
echo "配置参数:"
echo "- BITDEER_PROXY_URL: 代理地址"
echo "- BITDEER_DEGRADED_THRESHOLD: 降级阈值（默认 5）"
echo "- BITDEER_RECOVERY_THRESHOLD: 恢复阈值（默认 10）"
echo "- BITDEER_QUEUE_DELAY_MS: 排队延迟范围（默认 100-500）"
echo ""
echo "监控命令:"
echo "- 查看统计: curl '$API_BASE/api/option/proxy_stats/10836' -H 'Authorization: Bearer \$TOKEN'"
echo "- 重置状态: curl -X POST '$API_BASE/api/option/proxy_stats/10836/reset' -H 'Authorization: Bearer \$TOKEN'"
echo ""
echo "=========================================="
echo -e "${GREEN}测试完成${NC}"
echo "=========================================="
