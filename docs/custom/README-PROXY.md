# 动态出口 IP 切换功能

针对 Bitdeer 渠道（10835/10836）的智能代理切换系统，自动应对 Cloudflare 429 限流。

---

## 🎯 核心功能

- ✅ **智能降级**：检测到连续 429 错误后自动切换到代理模式
- ✅ **自动恢复**：连续成功后自动恢复直连，优先使用本机 IP
- ✅ **排队控制**：代理模式下自动添加随机延迟，避免瞬间爆发
- ✅ **多 IP 池**：通过 Xray 代理池轮询 5 个 Cloudflare IP
- ✅ **实时监控**：提供 API 端点查看状态和统计数据
- ✅ **零人工干预**：全自动运行，无需手动切换

---

## 📊 工作原理

### 状态转换

```
正常模式 (DIRECT)
- 使用本机 IP: 154.202.119.148
- 零额外延迟
        ↓ 连续 5 次 429 错误
降级模式 (PROXY)
- 使用 Xray 代理池
- 随机出口 IP (5 个 CF IP)
- 添加 100-500ms 排队延迟
        ↓ 连续 10 次成功
恢复正常 (DIRECT)
```

### 触发条件

| 事件 | 条件 | 动作 |
|------|------|------|
| 降级 | 连续 5 次 429 错误 | 切换到代理模式 |
| 恢复 | 连续 10 次成功 | 恢复直连模式 |

---

## 🚀 快速开始

### 1 分钟部署

```bash
# 1. SSH 登录生产服务器
ssh yutou-prod

# 2. 运行部署脚本
cd /path/to/yutou-api
sudo ./scripts/setup-dynamic-proxy.sh

# 3. 配置 Docker 环境变量
# 编辑 docker-compose.yml，添加：
# BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
# network_mode: "host"

# 4. 重新部署
docker-compose restart new-api

# 5. 验证
./scripts/test-dynamic-proxy.sh YOUR_ADMIN_TOKEN
```

**详细步骤**: 参见 [快速部署指南](./QUICK-DEPLOY-PROXY.md)

---

## 📚 文档索引

### 入门文档

- **[快速部署指南](./QUICK-DEPLOY-PROXY.md)** ⭐ 推荐新手阅读
  - 3 步快速部署
  - 验证清单
  - 常见问题排查

### 详细文档

- **[完整实现总结](./dynamic-proxy-implementation-summary.md)**
  - 架构设计
  - 核心逻辑
  - 性能优化
  - 运维指南

- **[Docker 配置详解](./docker-compose-proxy-config.md)**
  - 网络模式选择
  - 参数调优
  - 配置示例

### 脚本文件

- **[部署脚本](../../scripts/setup-dynamic-proxy.sh)**
  - 自动配置 Xray 代理
  - 创建 systemd 服务
  - 验证连通性

- **[测试脚本](../../scripts/test-dynamic-proxy.sh)**
  - 自动化测试流程
  - API 端点验证
  - 连通性检查

- **[Xray 配置](../../scripts/xray-proxy-config.json)**
  - 5 个 Cloudflare IP 节点
  - 负载均衡配置
  - SOCKS5/HTTP 双协议

---

## 🔍 监控与管理

### API 端点

#### 查看代理状态

```bash
GET /api/option/proxy_stats/:channelID
Authorization: Bearer YOUR_ADMIN_TOKEN
```

**示例**：
```bash
curl 'http://localhost:3000/api/option/proxy_stats/10836' \
  -H 'Authorization: Bearer YOUR_TOKEN' | jq
```

**响应**：
```json
{
  "success": true,
  "data": {
    "enabled": true,
    "mode": "direct",
    "success_count": 1234,
    "failure_count": 56,
    "consecutive_ok": 8,
    "consecutive_429": 0,
    "total_proxy_reqs": 120,
    "total_direct_reqs": 1170,
    "triggered_at": "2026-09-03T20:00:00Z",
    "last_recovery_time": "2026-09-03T20:15:00Z",
    "proxy_url": "socks5://127.0.0.1:10808"
  }
}
```

#### 重置代理状态

```bash
POST /api/option/proxy_stats/:channelID/reset
Authorization: Bearer YOUR_ADMIN_TOKEN
```

**示例**：
```bash
curl -X POST 'http://localhost:3000/api/option/proxy_stats/10836/reset' \
  -H 'Authorization: Bearer YOUR_TOKEN'
```

### 日志监控

```bash
# 查看降级/恢复事件
docker logs new-api 2>&1 | grep "DegradedManager"

# 示例输出：
# [DegradedManager] Channel 10836 degraded to PROXY mode after 5 consecutive 429 errors
# [DegradedManager] Channel 10836 recovered to DIRECT mode after 10 consecutive successes
```

---

## ⚙️ 配置参数

### 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `BITDEER_PROXY_URL` | `""` | 代理地址（空则禁用功能）|
| `BITDEER_DEGRADED_THRESHOLD` | `5` | 降级阈值（连续 N 次 429）|
| `BITDEER_RECOVERY_THRESHOLD` | `10` | 恢复阈值（连续 N 次成功）|
| `BITDEER_QUEUE_DELAY_MS` | `"100-500"` | 排队延迟范围（毫秒）|

### 推荐配置

**平衡策略**（默认）：
```yaml
BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
BITDEER_DEGRADED_THRESHOLD: "5"
BITDEER_RECOVERY_THRESHOLD: "10"
BITDEER_QUEUE_DELAY_MS: "100-500"
```

**保守策略**（更快切换到代理）：
```yaml
BITDEER_DEGRADED_THRESHOLD: "3"
BITDEER_RECOVERY_THRESHOLD: "20"
BITDEER_QUEUE_DELAY_MS: "200-800"
```

**激进策略**（优先直连性能）：
```yaml
BITDEER_DEGRADED_THRESHOLD: "10"
BITDEER_RECOVERY_THRESHOLD: "5"
BITDEER_QUEUE_DELAY_MS: "50-200"
```

---

## 🎓 技术细节

### 文件结构

```
relay/proxy/
├── degraded_manager.go    # 核心降级管理器（状态机）
└── helper.go              # 辅助函数（HTTP 客户端包装）

relay/channel/
└── api_request.go         # 请求发起集成点

controller/
├── relay.go               # 响应记录集成点
└── proxy_stats.go         # 管理 API 端点

router/
└── api-router.go          # API 路由定义

scripts/
├── setup-dynamic-proxy.sh       # 部署脚本
├── test-dynamic-proxy.sh        # 测试脚本
└── xray-proxy-config.json       # Xray 配置模板

docs/custom/
├── README-PROXY.md              # 本文件
├── QUICK-DEPLOY-PROXY.md        # 快速部署指南
├── dynamic-proxy-implementation-summary.md  # 完整实现文档
└── docker-compose-proxy-config.md  # Docker 配置文档
```

### 核心组件

#### DegradedManager（降级管理器）

- **职责**：管理渠道的代理状态和切换逻辑
- **状态**：`DIRECT`（直连）或 `PROXY`（代理）
- **线程安全**：使用 `sync.RWMutex` 保护状态

**关键方法**：
- `GetHTTPClient(channelID)`: 根据当前模式返回 HTTP 客户端
- `ApplyQueueDelay(ctx, channelID)`: 应用排队延迟
- `RecordResponse(channelID, statusCode, isSuccess)`: 记录响应并动态调整

#### Xray 代理池

- **协议**：VLESS + WebSocket + TLS
- **节点数**：5 个 Cloudflare CDN IP
- **负载均衡**：随机策略（random）
- **监听端口**：
  - SOCKS5: `127.0.0.1:10808`
  - HTTP: `127.0.0.1:10809`

---

## ❓ FAQ

### Q1: 为什么只针对 10835/10836 渠道？

**A**: 这两个是 Bitdeer 渠道，多 key（1000+ keys）+ 高并发容易触发 Cloudflare 限流。其他渠道通常不会遇到此问题。

### Q2: 代理模式会增加多少延迟？

**A**: 
- **直连模式**: 0ms 额外延迟
- **代理模式**: +50-200ms（Cloudflare CDN 转发）+ 100-500ms（排队延迟）
- **总增加**: 约 150-700ms

### Q3: 代理流量费用如何？

**A**: Xray 订阅通常包含流量套餐，实际使用量取决于降级频率。正常情况下 > 90% 请求走直连，代理流量很少。

### Q4: 如果代理也被限流怎么办？

**A**: 
1. 代理池有 5 个不同 IP，分散风险
2. 降级模式会添加排队延迟，降低请求速率
3. 如果仍被限流，系统会记录失败但不会崩溃
4. 可以手动重置状态或调整参数

### Q5: 能否禁用自动切换？

**A**: 可以。将 `BITDEER_PROXY_URL` 设为空字符串或不设置即可禁用整个功能。

### Q6: 为什么需要 `network_mode: "host"`？

**A**: Xray 代理监听在 `127.0.0.1:10808`，Docker 容器默认网络无法访问宿主机的 localhost。使用 host 网络模式可以直接访问。

**替代方案**: 修改 Xray 监听地址为 `0.0.0.0`（但需配合防火墙规则）。

### Q7: 如何调优参数？

**A**: 观察以下指标：
- 如果频繁降级：提高 `DEGRADED_THRESHOLD`
- 如果恢复太慢：降低 `RECOVERY_THRESHOLD`
- 如果代理模式仍 429：增加 `QUEUE_DELAY_MS`

---

## 🔒 安全注意事项

1. ⚠️ **代理凭证**：Xray 配置包含 UUID，不要泄露或提交到公开仓库
2. ⚠️ **防火墙**：如果修改 Xray 监听为 `0.0.0.0`，务必配置防火墙
3. ⚠️ **日志**：代理 URL 会出现在日志中，注意日志权限
4. ✅ **订阅更新**：定期检查 Xray 订阅是否过期

---

## 🛠️ 故障排查

### 常见问题

| 症状 | 原因 | 解决方案 |
|------|------|----------|
| `connection refused` | xray-proxy 未启动 | `sudo systemctl start xray-proxy` |
| `enabled: false` | 环境变量未配置 | 检查 `docker-compose.yml` |
| 一直停留在 PROXY 模式 | 代理出口仍被限流 | 手动重置或调整参数 |
| 频繁切换模式 | 阈值过于敏感 | 提高阈值 |

**详细排查**: 参见 [快速部署指南 - 故障排查](./QUICK-DEPLOY-PROXY.md#-故障排查)

---

## 📈 性能影响

### 预期指标

| 指标 | 直连模式 | 代理模式 |
|------|----------|----------|
| 延迟 | 基线 | +150-700ms |
| 吞吐量 | 100% | ~80%（因排队延迟）|
| 429 错误率 | 可能较高 | 显著降低 |

### 资源消耗

- **CPU**: < 1%（状态管理开销很小）
- **内存**: < 10MB（每个渠道约 1KB 状态）
- **网络**: 取决于实际降级频率（通常 < 10%）

---

## 🎉 总结

动态出口 IP 切换功能通过智能降级机制，自动应对 Cloudflare 429 限流，在保证可用性的同时最大化性能：

- ✅ **自动化**：零人工干预
- ✅ **高可用**：多 IP 池降低封禁风险
- ✅ **高性能**：正常情况下零额外延迟
- ✅ **可观测**：实时监控和统计
- ✅ **可配置**：灵活调整策略

---

## 📞 支持

- 📖 完整文档：`docs/custom/`
- 🔧 部署脚本：`scripts/`
- 🐛 问题反馈：提交 issue 或联系管理员
