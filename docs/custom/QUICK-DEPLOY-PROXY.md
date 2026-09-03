# 动态出口 IP 切换 - 快速部署指南

## 📋 前置条件

- ✅ 生产服务器访问权限（SSH）
- ✅ 管理员 API Token
- ✅ Xray 已安装（如果未安装，脚本会提示）

---

## 🚀 快速部署（3 步完成）

### 第 1 步：部署 Xray 代理服务

```bash
# SSH 登录生产服务器
ssh yutou-prod

# 下载并运行部署脚本
cd /path/to/yutou-api
sudo ./scripts/setup-dynamic-proxy.sh
```

**预期输出**：
```
✓ xray-proxy 服务启动成功
✓ 代理工作正常，出口 IP: 104.17.219.106
本机 IP: 154.202.119.148
代理 IP: 104.17.219.106
SOCKS5 代理: 127.0.0.1:10808
HTTP 代理:   127.0.0.1:10809
```

---

### 第 2 步：配置 Docker 环境变量

编辑 `docker-compose.yml`：

```yaml
services:
  new-api:
    # ===== CUSTOM START: 动态代理切换 =====
    network_mode: "host"  # 使用 host 网络访问宿主机代理
    # ===== CUSTOM END =====
    
    environment:
      # ... 其他环境变量 ...
      
      # ===== CUSTOM START: 动态代理配置 =====
      BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
      BITDEER_DEGRADED_THRESHOLD: "5"
      BITDEER_RECOVERY_THRESHOLD: "10"
      BITDEER_QUEUE_DELAY_MS: "100-500"
      # ===== CUSTOM END =====
```

---

### 第 3 步：部署新代码

```bash
# 本地编译（在开发机上）
go build -o new-api main.go

# 上传到生产服务器
scp new-api yutou-prod:/path/to/deploy/

# 生产服务器上重启（SSH 登录后）
cd /path/to/deploy
docker-compose down
docker-compose up -d

# 等待容器启动
sleep 5

# 验证容器健康
docker ps | grep new-api
```

---

## ✅ 验证部署

### 1. 自动化测试

```bash
# 在生产服务器上运行
cd /path/to/yutou-api
./scripts/test-dynamic-proxy.sh YOUR_ADMIN_TOKEN
```

### 2. 手动验证

#### A. 检查代理状态

```bash
# 查看渠道 10836
curl 'http://localhost:3000/api/option/proxy_stats/10836' \
  -H 'Authorization: Bearer YOUR_TOKEN' | jq

# 查看渠道 10835
curl 'http://localhost:3000/api/option/proxy_stats/10835' \
  -H 'Authorization: Bearer YOUR_TOKEN' | jq
```

**预期响应**：
```json
{
  "success": true,
  "data": {
    "enabled": true,
    "mode": "direct",
    "success_count": 0,
    "failure_count": 0,
    "consecutive_ok": 0,
    "consecutive_429": 0,
    "total_proxy_reqs": 0,
    "total_direct_reqs": 0,
    "proxy_url": "socks5://127.0.0.1:10808"
  }
}
```

#### B. 检查服务状态

```bash
# xray-proxy 服务
sudo systemctl status xray-proxy

# new-api 容器
docker logs new-api 2>&1 | grep "DegradedManager" | tail -5
```

#### C. 测试代理连通性

```bash
# 测试 SOCKS5 代理
curl -x socks5://127.0.0.1:10808 https://api.ipify.org

# 测试 HTTP 代理
curl -x http://127.0.0.1:10809 https://api.ipify.org
```

---

## 📊 监控指标

### 关键指标

| 指标 | 命令 | 正常值 |
|------|------|--------|
| 当前模式 | `curl .../proxy_stats/10836 \| jq '.data.mode'` | `"direct"` |
| 连续 429 | `curl .../proxy_stats/10836 \| jq '.data.consecutive_429'` | `0` |
| 代理请求数 | `curl .../proxy_stats/10836 \| jq '.data.total_proxy_reqs'` | 远小于 `total_direct_reqs` |

### 实时监控

```bash
# 监控降级/恢复事件
watch -n 5 'docker logs new-api 2>&1 | grep "DegradedManager" | tail -10'

# 监控 429 错误
watch -n 5 'docker logs new-api 2>&1 | grep -c "429"'

# 监控代理状态
watch -n 5 'curl -s http://localhost:3000/api/option/proxy_stats/10836 -H "Authorization: Bearer $TOKEN" | jq ".data | {mode, consecutive_429, total_proxy_reqs}"'
```

---

## 🔧 故障排查

### 问题 1：代理服务未启动

**症状**：
```
[ERROR] proxyconnect tcp: dial tcp 127.0.0.1:10808: connect: connection refused
```

**解决**：
```bash
# 检查服务
sudo systemctl status xray-proxy

# 启动服务
sudo systemctl start xray-proxy

# 查看日志
sudo journalctl -u xray-proxy -n 50
```

### 问题 2：容器无法访问宿主机代理

**症状**：
```json
{
  "enabled": false,
  "proxy_url": ""
}
```

**检查**：
```bash
# 确认使用了 host 网络模式
docker inspect new-api | jq '.[0].HostConfig.NetworkMode'
# 应该输出: "host"

# 确认环境变量
docker exec new-api env | grep BITDEER
```

**解决**：
```yaml
# docker-compose.yml 中添加
services:
  new-api:
    network_mode: "host"
```

### 问题 3：一直停留在代理模式

**症状**：
```json
{
  "mode": "proxy",
  "consecutive_ok": 5
}
```

**原因**：代理模式下请求仍然失败

**排查**：
```bash
# 测试代理出口能否访问 Bitdeer
curl -v -x socks5://127.0.0.1:10808 https://api-inference.bitdeer.ai/v1/models

# 查看实际错误
docker logs new-api 2>&1 | grep "bitdeer" | tail -20
```

**解决**：
```bash
# 手动重置状态
curl -X POST 'http://localhost:3000/api/option/proxy_stats/10836/reset' \
  -H 'Authorization: Bearer YOUR_TOKEN'
```

---

## 🎯 工作原理

### 状态机

```
┌─────────────┐
│   DIRECT    │ ←────────────┐
│  (本机 IP)   │              │
└──────┬──────┘              │
       │                     │
       │ 连续 5 次 429        │ 连续 10 次成功
       │                     │
       ↓                     │
┌─────────────┐              │
│    PROXY    │ ─────────────┘
│ (Xray 代理)  │
│ + 排队延迟   │
└─────────────┘
```

### 请求流程

```
用户请求
  ↓
检查渠道 ID (10835/10836)
  ↓
┌────────────┬────────────┐
│   DIRECT   │   PROXY    │
│            │            │
│ 本机 IP     │ Xray 代理   │
│ 154.x.x.x  │ CF IP 池    │
│ 无延迟     │ +100-500ms │
└────────────┴────────────┘
  ↓
上游 Bitdeer API
  ↓
┌────────────┬────────────┐
│   成功     │   429      │
└────────────┴────────────┘
  ↓            ↓
记录统计 → 动态调整模式
```

---

## 📝 配置调优

### 保守策略（优先稳定性）

```yaml
BITDEER_DEGRADED_THRESHOLD: "3"   # 更快切换到代理
BITDEER_RECOVERY_THRESHOLD: "20"  # 更慢恢复直连
BITDEER_QUEUE_DELAY_MS: "200-800" # 更长延迟
```

### 激进策略（优先性能）

```yaml
BITDEER_DEGRADED_THRESHOLD: "10"  # 更慢切换到代理
BITDEER_RECOVERY_THRESHOLD: "5"   # 更快恢复直连
BITDEER_QUEUE_DELAY_MS: "50-200"  # 更短延迟
```

### 平衡策略（推荐，默认）

```yaml
BITDEER_DEGRADED_THRESHOLD: "5"
BITDEER_RECOVERY_THRESHOLD: "10"
BITDEER_QUEUE_DELAY_MS: "100-500"
```

---

## 🔙 回滚方案

### 临时禁用（无需重新编译）

```bash
# 停止 xray-proxy
sudo systemctl stop xray-proxy

# 或者清空环境变量并重启容器
# 编辑 docker-compose.yml，注释掉 BITDEER_PROXY_URL
docker-compose restart new-api
```

### 永久禁用

```bash
# 1. 停止并禁用 xray-proxy
sudo systemctl stop xray-proxy
sudo systemctl disable xray-proxy

# 2. 删除配置
sudo rm -rf /usr/local/etc/xray-proxy
sudo rm -f /etc/systemd/system/xray-proxy.service

# 3. docker-compose.yml 中删除相关配置
# 注释掉或删除 BITDEER_* 环境变量和 network_mode: "host"

# 4. 重启容器
docker-compose restart new-api
```

---

## 📖 相关文档

- [完整实现总结](./dynamic-proxy-implementation-summary.md)
- [Docker 配置详解](./docker-compose-proxy-config.md)
- [Xray 代理配置](../scripts/xray-proxy-config.json)

---

## 💡 最佳实践

1. ✅ **定期监控**：每天检查一次代理状态和统计
2. ✅ **保留日志**：定期备份包含 "DegradedManager" 的日志
3. ✅ **及时更新订阅**：Xray 订阅可能过期，需定期检查
4. ✅ **调优参数**：根据实际 429 频率调整阈值
5. ⚠️ **避免频繁重置**：手动重置会丢失统计数据

---

## 🆘 支持

遇到问题？
1. 查看 [故障排查](#-故障排查) 章节
2. 运行自动测试脚本：`./scripts/test-dynamic-proxy.sh`
3. 查看完整文档：`docs/custom/dynamic-proxy-implementation-summary.md`
