# 动态出口 IP 切换实现总结

## 概述

为 10835 (bitdeer-free) 和 10836 (bitdeer) 渠道实现动态出口 IP 切换功能，通过 Xray 代理池自动应对上游 Cloudflare 429 限流，同时在正常情况下保持本机 IP 直连以获得最佳性能。

---

## 实现架构

### 1. 核心组件

```
┌─────────────────────────────────────────────────────────────┐
│                     请求流程                                  │
└─────────────────────────────────────────────────────────────┘
                            ↓
         ┌─────────────────────────────────┐
         │   DegradedManager               │
         │   (降级管理器)                   │
         └─────────────────────────────────┘
                            ↓
         ┌─────────────────────────────────┐
         │  检查渠道模式                     │
         │  - 10835/10836 启用              │
         │  - 其他渠道直连                   │
         └─────────────────────────────────┘
                            ↓
    ┌────────────────┬───────────────────────┐
    │                │                       │
┌───▼────┐      ┌────▼─────┐         ┌─────▼────┐
│ DIRECT │      │  PROXY   │         │ 降级逻辑  │
│  模式  │      │   模式   │         │          │
└───┬────┘      └────┬─────┘         └─────┬────┘
    │                │                     │
    │ 本机 IP        │ Xray 代理池         │ 429 检测
    │ 154.x.x.x      │ (5 个 CF IP)        │
    │                │ - 104.17.219.106    │ 连续 5 次 429
    │                │ - 188.164.248.24    │ → 切换到 PROXY
    │                │ - 104.16.244.89     │
    │                │ - 104.17.107.127    │ 连续 10 次成功
    │                │ - 108.162.198.1     │ → 恢复到 DIRECT
    │                │                     │
    │                │ + 排队延迟          │
    │                │   (100-500ms)       │
    └────────────────┴─────────────────────┘
                     ↓
            上游 Bitdeer API
```

### 2. 文件结构

```
relay/proxy/
├── degraded_manager.go    # 核心降级管理器
└── helper.go              # 辅助函数

relay/channel/
└── api_request.go         # 集成点（已修改）

controller/
├── relay.go              # 响应记录（已修改）
└── proxy_stats.go        # 管理端点（新增）

router/
└── api-router.go         # 路由定义（已修改）

scripts/
├── setup-dynamic-proxy.sh       # 部署脚本
├── test-dynamic-proxy.sh        # 测试脚本
└── xray-proxy-config.json       # Xray 配置

docs/custom/
├── dynamic-proxy-setup.md       # 配置指南
└── docker-compose-proxy-config.md  # Docker 配置
```

---

## 核心逻辑

### 1. 降级管理器 (DegradedManager)

**状态机**：
```
初始状态: DIRECT (直连)
         │
         │ 429 错误 × 5
         ↓
       PROXY (代理)
         │
         │ 成功 × 10
         ↓
       DIRECT (直连)
```

**关键方法**：
- `GetHTTPClient(channelID)`: 根据当前模式返回 HTTP 客户端
- `ApplyQueueDelay(ctx, channelID)`: 代理模式下应用随机延迟
- `RecordResponse(channelID, statusCode, isSuccess)`: 记录响应并动态调整模式

### 2. 集成点

#### A. 请求发起 (relay/channel/api_request.go)
```go
// doRequest 函数中
baseClient := service.GetHttpClient()
client = relayproxy.WrapHTTPClientWithProxy(c, info, baseClient)
```

#### B. 响应处理 (controller/relay.go)
```go
// 成功
if newAPIError == nil {
    relayproxy.RecordResponseForProxy(relayInfo, 200, true)
    return
}

// 失败
statusCode := newAPIError.StatusCode
relayproxy.RecordResponseForProxy(relayInfo, statusCode, false)
```

### 3. 配置参数

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `BITDEER_PROXY_URL` | `""` | 代理地址（空则禁用） |
| `BITDEER_DEGRADED_THRESHOLD` | `5` | 连续 N 次 429 后降级 |
| `BITDEER_RECOVERY_THRESHOLD` | `10` | 连续 N 次成功后恢复 |
| `BITDEER_QUEUE_DELAY_MS` | `"100-500"` | 代理模式排队延迟范围 |

---

## 部署步骤

### 第一步：配置 Xray 代理

```bash
# 在生产服务器上运行
cd /path/to/yutou-api
chmod +x scripts/setup-dynamic-proxy.sh
sudo ./scripts/setup-dynamic-proxy.sh
```

**脚本会自动**：
1. 备份现有 Xray 配置
2. 创建独立的 xray-proxy 服务
3. 配置 5 个 Cloudflare IP 出口
4. 启动服务并验证连通性

### 第二步：更新 Docker 配置

**方案 A：使用 host 网络（推荐）**

编辑 `docker-compose.yml`：
```yaml
services:
  new-api:
    network_mode: "host"
    environment:
      BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
      BITDEER_DEGRADED_THRESHOLD: "5"
      BITDEER_RECOVERY_THRESHOLD: "10"
      BITDEER_QUEUE_DELAY_MS: "100-500"
```

**方案 B：修改 Xray 监听地址**

如果不想使用 host 网络：
```bash
# 修改 Xray 配置监听 0.0.0.0
sudo sed -i 's/"127.0.0.1"/"0.0.0.0"/g' /usr/local/etc/xray-proxy/config.json
sudo systemctl restart xray-proxy

# 配置防火墙
sudo ufw allow from 172.17.0.0/16 to any port 10808
```

然后使用 Docker 网桥 IP：
```yaml
environment:
  BITDEER_PROXY_URL: "socks5://172.17.0.1:10808"
```

### 第三步：重新编译并部署

```bash
# 本地编译
cd /path/to/yutou-api
go build -o new-api main.go

# 上传到生产服务器
scp new-api yutou-prod:/path/to/deploy/

# 生产服务器上重启
ssh yutou-prod
cd /path/to/deploy
docker-compose down
docker-compose up -d
```

### 第四步：验证部署

```bash
# 在生产服务器上运行测试脚本
cd /path/to/yutou-api
chmod +x scripts/test-dynamic-proxy.sh
./scripts/test-dynamic-proxy.sh YOUR_ADMIN_TOKEN
```

---

## 监控与运维

### 1. 查看当前状态

```bash
# 查看渠道 10836 状态
curl 'http://localhost:3000/api/option/proxy_stats/10836' \
  -H 'Authorization: Bearer YOUR_TOKEN' | jq
```

**响应示例**：
```json
{
  "success": true,
  "data": {
    "enabled": true,
    "mode": "direct",
    "success_count": 123,
    "failure_count": 5,
    "consecutive_ok": 8,
    "consecutive_429": 0,
    "total_proxy_reqs": 45,
    "total_direct_reqs": 856,
    "triggered_at": "2026-09-03T20:00:00Z",
    "last_recovery_time": "2026-09-03T20:15:00Z",
    "proxy_url": "socks5://127.0.0.1:10808"
  }
}
```

### 2. 关键指标

| 字段 | 说明 | 正常值 |
|------|------|--------|
| `mode` | 当前模式 | `"direct"` 为主，偶尔 `"proxy"` |
| `consecutive_429` | 连续 429 次数 | < 5 |
| `total_proxy_reqs` | 代理请求总数 | 应远小于 `total_direct_reqs` |
| `success_count` | 成功次数 | 持续增长 |

### 3. 日志监控

```bash
# 查看降级/恢复事件
docker logs new-api 2>&1 | grep "DegradedManager"

# 示例输出
# [DegradedManager] Channel 10836 degraded to PROXY mode after 5 consecutive 429 errors
# [DegradedManager] Channel 10836 recovered to DIRECT mode after 10 consecutive successes
```

### 4. 性能指标

**预期影响**：
- **直连模式**：延迟 +0ms（无影响）
- **代理模式**：延迟 +50-200ms（Cloudflare CDN 转发）
- **切换开销**：< 1ms（内存操作）

**观察点**：
```bash
# 监控响应时间
docker logs new-api 2>&1 | grep "response_time" | tail -100

# 监控 429 频率
docker logs new-api 2>&1 | grep "429" | wc -l
```

---

## 故障排查

### 问题 1: 代理无法连接

**症状**：
```
[ERROR] do request failed: proxyconnect tcp: dial tcp 127.0.0.1:10808: connect: connection refused
```

**排查**：
```bash
# 检查 xray-proxy 服务
sudo systemctl status xray-proxy

# 检查端口监听
ss -tlnp | grep 10808

# 查看 Xray 日志
sudo journalctl -u xray-proxy -n 50
```

**解决**：
```bash
# 重启服务
sudo systemctl restart xray-proxy

# 如果仍失败，检查配置
sudo /usr/local/bin/xray run -test -config /usr/local/etc/xray-proxy/config.json
```

### 问题 2: 一直停留在代理模式

**症状**：
```json
{
  "mode": "proxy",
  "consecutive_ok": 5,  // 低于恢复阈值
  "total_proxy_reqs": 1000  // 持续增长
}
```

**原因**：代理模式下请求仍然失败，无法累积足够的成功次数

**排查**：
```bash
# 测试代理出口
curl -v -x socks5://127.0.0.1:10808 https://api-inference.bitdeer.ai/v1/models

# 查看实际错误
docker logs new-api 2>&1 | grep "bitdeer" | tail -50
```

**解决**：
```bash
# 手动重置状态
curl -X POST 'http://localhost:3000/api/option/proxy_stats/10836/reset' \
  -H 'Authorization: Bearer YOUR_TOKEN'
```

### 问题 3: 频繁切换模式

**症状**：
```
20:00:00 degraded to PROXY
20:02:00 recovered to DIRECT
20:03:00 degraded to PROXY
20:05:00 recovered to DIRECT
```

**原因**：阈值设置过于敏感

**解决**：调整参数
```yaml
environment:
  BITDEER_DEGRADED_THRESHOLD: "10"   # 提高降级阈值
  BITDEER_RECOVERY_THRESHOLD: "20"   # 提高恢复阈值
```

---

## 性能优化建议

### 1. 减少重试次数

当前 429 错误会重试 5 次，建议改为 1 次：

**文件**: `common/constants.go`
```go
// 当前
var RetryTimes = 5

// 建议：针对 429 单独处理
var RetryTimes = 5
var RetryTimes429 = 1  // 429 只重试 1 次
```

### 2. 增加代理出口

当前配置 5 个 IP，可以扩展到订阅的全部 16 个：

编辑 `/usr/local/etc/xray-proxy/config.json`，添加更多 `outbounds`。

### 3. 负载均衡策略

当前使用 `random` 策略，可以考虑：
- `leastPing`: 选择延迟最低的
- `roundRobin`: 轮询

```json
{
  "balancers": [{
    "strategy": {
      "type": "leastPing"
    }
  }]
}
```

---

## 回滚方案

### 临时禁用（无需重新编译）

```bash
# 停止 xray-proxy
sudo systemctl stop xray-proxy

# 或者清空环境变量
docker exec new-api sh -c 'export BITDEER_PROXY_URL=""'
docker-compose restart new-api
```

### 永久禁用（需要重新编译）

1. 删除或注释掉相关代码中的 `// CUSTOM START/END` 标记块
2. 删除 `relay/proxy/` 目录
3. 重新编译部署

---

## 总结

### 优势

✅ **智能降级**：自动检测 429 并切换代理，无需人工干预
✅ **性能优先**：正常情况下使用直连，延迟最低
✅ **分散风险**：5 个 Cloudflare IP 轮询，降低单 IP 被封概率
✅ **排队控制**：代理模式下自动添加延迟，避免瞬间爆发
✅ **自动恢复**：连续成功后自动恢复直连，节省代理流量
✅ **可监控**：提供 API 端点查看实时状态

### 局限性

⚠️ **代理延迟**：代理模式下增加 50-200ms 延迟
⚠️ **依赖订阅**：需要维护有效的 Xray 订阅
⚠️ **配置复杂**：需要额外部署 xray-proxy 服务
⚠️ **无法完全避免 429**：上游 Cloudflare 规则可能变化

### 适用场景

- ✅ 高并发场景（单用户短时大量请求）
- ✅ 多 key 渠道（频繁切换 API key）
- ✅ 对可用性要求高于延迟的场景
- ❌ 对延迟敏感的实时场景
- ❌ 预算有限无法承担代理成本的场景

---

## 联系方式

如有问题，请联系：
- 技术支持: [提交 issue]
- 文档维护: docs/custom/
