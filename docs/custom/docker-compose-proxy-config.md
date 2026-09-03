# 动态代理切换 - docker-compose.yml 配置示例

## 需要添加到 new-api 服务的环境变量：

```yaml
services:
  new-api:
    # ... 其他配置 ...
    environment:
      # ===== CUSTOM START: 动态代理切换配置 =====
      # 代理 URL（SOCKS5 或 HTTP）
      BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
      
      # 降级阈值：连续 N 次 429 错误后切换到代理模式
      BITDEER_DEGRADED_THRESHOLD: "5"
      
      # 恢复阈值：连续 N 次成功后恢复直连模式
      BITDEER_RECOVERY_THRESHOLD: "10"
      
      # 代理模式下的排队延迟范围（毫秒）
      BITDEER_QUEUE_DELAY_MS: "100-500"
      # ===== CUSTOM END =====
      
      # ... 其他环境变量 ...
    
    # ===== CUSTOM START: 网络模式 =====
    # 使用 host 网络模式，以便访问宿主机的 127.0.0.1:10808 代理
    network_mode: "host"
    # ===== CUSTOM END =====
```

## 注意事项

### 1. 网络模式

由于 Xray 代理监听在 `127.0.0.1:10808`，Docker 容器需要使用 `network_mode: "host"` 才能访问。

**方案 A：使用 host 网络模式（推荐）**
```yaml
services:
  new-api:
    network_mode: "host"
    environment:
      BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
```

优点：
- 简单直接
- 性能最好（无网络转发）

缺点：
- 容器共享宿主机网络栈
- 端口冲突需要手动管理

**方案 B：使用 host.docker.internal（Mac/Windows）**
```yaml
services:
  new-api:
    environment:
      BITDEER_PROXY_URL: "socks5://host.docker.internal:10808"
```

注意：Linux 需要额外配置 `extra_hosts`

**方案 C：修改 Xray 监听地址**

将 Xray 配置中的 `127.0.0.1` 改为 `0.0.0.0`：
```json
{
  "inbounds": [
    {
      "listen": "0.0.0.0",  // 改为监听所有接口
      "port": 10808,
      ...
    }
  ]
}
```

然后使用宿主机 IP：
```yaml
services:
  new-api:
    environment:
      BITDEER_PROXY_URL: "socks5://172.17.0.1:10808"  # Docker 网桥网关
```

⚠️ **安全警告**：监听 `0.0.0.0` 会暴露代理到网络，建议配合防火墙规则：
```bash
sudo ufw allow from 172.17.0.0/16 to any port 10808
```

### 2. 参数调优

根据实际情况调整参数：

| 参数 | 默认值 | 说明 | 建议 |
|------|--------|------|------|
| `BITDEER_DEGRADED_THRESHOLD` | 5 | 降级阈值 | 3-10，值越小越敏感 |
| `BITDEER_RECOVERY_THRESHOLD` | 10 | 恢复阈值 | 5-20，值越大越保守 |
| `BITDEER_QUEUE_DELAY_MS` | 100-500 | 排队延迟 | 根据上游速率限制调整 |

**保守策略**（更快切换到代理）：
```yaml
BITDEER_DEGRADED_THRESHOLD: "3"
BITDEER_RECOVERY_THRESHOLD: "15"
```

**激进策略**（尽量保持直连）：
```yaml
BITDEER_DEGRADED_THRESHOLD: "10"
BITDEER_RECOVERY_THRESHOLD: "5"
```

### 3. 监控和诊断

**查看当前状态**：
```bash
# 渠道 10836 (bitdeer)
curl 'http://localhost:3000/api/option/proxy_stats/10836' \
  -H 'Authorization: Bearer YOUR_ADMIN_TOKEN'

# 渠道 10835 (bitdeer-free)
curl 'http://localhost:3000/api/option/proxy_stats/10835' \
  -H 'Authorization: Bearer YOUR_ADMIN_TOKEN'
```

**重置状态**：
```bash
curl -X POST 'http://localhost:3000/api/option/proxy_stats/10836/reset' \
  -H 'Authorization: Bearer YOUR_ADMIN_TOKEN'
```

**查看日志**：
```bash
# 查看降级/恢复日志
docker logs new-api 2>&1 | grep -E "DegradedManager|degraded|PROXY|DIRECT"

# 查看 429 错误
docker logs new-api 2>&1 | grep "429"
```

### 4. 完整示例

```yaml
version: '3.8'

services:
  new-api:
    image: your-registry/new-api:latest
    container_name: new-api
    restart: unless-stopped
    network_mode: "host"  # 使用 host 网络访问宿主机代理
    
    environment:
      # 数据库配置
      SQL_DSN: "postgresql://newapi:xxx@127.0.0.1:5432/newapi?sslmode=disable"
      
      # Redis 配置
      REDIS_CONN_STRING: "redis://127.0.0.1:6379"
      
      # 动态代理切换
      BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
      BITDEER_DEGRADED_THRESHOLD: "5"
      BITDEER_RECOVERY_THRESHOLD: "10"
      BITDEER_QUEUE_DELAY_MS: "100-500"
      
      # 其他配置...
    
    volumes:
      - ./data:/data
    
    # 健康检查
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/api/status"]
      interval: 30s
      timeout: 10s
      retries: 3
```

### 5. 回滚方案

如果需要禁用动态代理：

**临时禁用**（不重启容器）：
```bash
# 将 BITDEER_PROXY_URL 设为空即可
docker exec new-api sh -c 'export BITDEER_PROXY_URL=""'
# 注意：需要重启 Go 进程才能生效，不如直接重启容器
```

**永久禁用**：
```yaml
environment:
  # 注释掉或删除
  # BITDEER_PROXY_URL: "socks5://127.0.0.1:10808"
```

然后重启：
```bash
docker-compose restart new-api
```

**停止 Xray 代理**：
```bash
sudo systemctl stop xray-proxy
sudo systemctl disable xray-proxy
```
