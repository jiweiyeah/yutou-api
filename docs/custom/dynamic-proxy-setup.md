# 动态出口 IP 切换配置指南

## 概述

为 10835 (bitdeer-free) 和 10836 (bitdeer) 渠道实现动态出口 IP 切换，避免触发上游 Cloudflare 限流。

## 架构

```
正常流量 → 直连（154.202.119.148）
   ↓
429 触发 → 降级模式
   ↓
   ├─ 排队延迟（100-500ms）
   ├─ 通过 Xray 代理（16 个 Cloudflare IP 轮询）
   └─ 连续成功 N 次 → 恢复直连
```

## 步骤 1：配置 Xray 本地代理

### 1.1 备份现有配置

```bash
ssh yutou-prod
sudo cp /usr/local/etc/xray/config.json /usr/local/etc/xray/config.json.bak
```

### 1.2 创建独立的代理配置

由于现有 Xray 已经用于 Reality 入站服务（443 端口），我们需要**新增一个独立的 Xray 实例**专门用于出站代理。

```bash
# 创建新的配置目录
sudo mkdir -p /usr/local/etc/xray-proxy

# 上传配置文件（从 scripts/xray-proxy-config.json）
sudo tee /usr/local/etc/xray-proxy/config.json > /dev/null <<'EOF'
{
  "log": {
    "loglevel": "warning"
  },
  "inbounds": [
    {
      "tag": "socks-in",
      "listen": "127.0.0.1",
      "port": 10808,
      "protocol": "socks",
      "settings": {
        "auth": "noauth",
        "udp": false
      }
    },
    {
      "tag": "http-in",
      "listen": "127.0.0.1",
      "port": 10809,
      "protocol": "http"
    }
  ],
  "outbounds": [
    {
      "tag": "proxy-1",
      "protocol": "vless",
      "settings": {
        "vnext": [{
          "address": "104.17.219.106",
          "port": 2053,
          "users": [{
            "id": "51a0af77-a60c-4d9b-81a4-f50e1b38d90b",
            "encryption": "none"
          }]
        }]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "yes.mytunnel.kdns.fr",
          "fingerprint": "chrome"
        },
        "wsSettings": {
          "path": "/"
        }
      }
    },
    {
      "tag": "proxy-2",
      "protocol": "vless",
      "settings": {
        "vnext": [{
          "address": "188.164.248.24",
          "port": 2083,
          "users": [{
            "id": "51a0af77-a60c-4d9b-81a4-f50e1b38d90b",
            "encryption": "none"
          }]
        }]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "yes.mytunnel.kdns.fr",
          "fingerprint": "chrome"
        },
        "wsSettings": {
          "path": "/"
        }
      }
    },
    {
      "tag": "proxy-3",
      "protocol": "vless",
      "settings": {
        "vnext": [{
          "address": "104.16.244.89",
          "port": 443,
          "users": [{
            "id": "51a0af77-a60c-4d9b-81a4-f50e1b38d90b",
            "encryption": "none"
          }]
        }]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "yes.mytunnel.kdns.fr",
          "fingerprint": "chrome"
        },
        "wsSettings": {
          "path": "/"
        }
      }
    },
    {
      "tag": "proxy-4",
      "protocol": "vless",
      "settings": {
        "vnext": [{
          "address": "104.17.107.127",
          "port": 8443,
          "users": [{
            "id": "51a0af77-a60c-4d9b-81a4-f50e1b38d90b",
            "encryption": "none"
          }]
        }]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "yes.mytunnel.kdns.fr",
          "fingerprint": "chrome"
        },
        "wsSettings": {
          "path": "/"
        }
      }
    },
    {
      "tag": "proxy-5",
      "protocol": "vless",
      "settings": {
        "vnext": [{
          "address": "108.162.198.1",
          "port": 8443,
          "users": [{
            "id": "51a0af77-a60c-4d9b-81a4-f50e1b38d90b",
            "encryption": "none"
          }]
        }]
      },
      "streamSettings": {
        "network": "ws",
        "security": "tls",
        "tlsSettings": {
          "serverName": "yes.mytunnel.kdns.fr",
          "fingerprint": "chrome"
        },
        "wsSettings": {
          "path": "/"
        }
      }
    },
    {
      "tag": "direct",
      "protocol": "freedom"
    }
  ],
  "routing": {
    "domainStrategy": "AsIs",
    "rules": [
      {
        "type": "field",
        "inboundTag": ["socks-in", "http-in"],
        "balancerTag": "proxy-balancer"
      }
    ],
    "balancers": [
      {
        "tag": "proxy-balancer",
        "selector": ["proxy-1", "proxy-2", "proxy-3", "proxy-4", "proxy-5"],
        "strategy": {
          "type": "random"
        }
      }
    ]
  }
}
EOF
```

### 1.3 创建独立的 systemd 服务

```bash
sudo tee /etc/systemd/system/xray-proxy.service > /dev/null <<'EOF'
[Unit]
Description=Xray Proxy Service (for API Gateway)
Documentation=https://xtls.github.io
After=network.target nss-lookup.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/xray run -config /usr/local/etc/xray-proxy/config.json
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

# 启动服务
sudo systemctl daemon-reload
sudo systemctl enable xray-proxy
sudo systemctl start xray-proxy

# 检查状态
sudo systemctl status xray-proxy
```

### 1.4 验证代理可用性

```bash
# 测试 SOCKS5 代理
curl -x socks5://127.0.0.1:10808 https://api.ipify.org
# 应该返回 Cloudflare IP（104.x.x.x 或 188.x.x.x）

# 测试 HTTP 代理
curl -x http://127.0.0.1:10809 https://api.ipify.org
```

## 步骤 2：实现 Go 代码支持

### 2.1 添加代理配置到 Channel 模型

需要修改以下文件：
- `model/channel.go` - 添加 `ProxyUrl` 字段
- `relay/channel/api_request.go` - HTTP 客户端支持代理
- `controller/relay.go` - 429 降级逻辑

### 2.2 添加降级状态管理

使用 Redis 存储渠道降级状态：
```
Key: channel:degraded:{channel_id}
Value: {
  "mode": "proxy",  // "direct" | "proxy"
  "success_count": 0,
  "triggered_at": "2026-09-03T19:00:00Z"
}
TTL: 3600s
```

## 步骤 3：配置环境变量

在生产环境添加：

```bash
# Docker compose 添加
BITDEER_PROXY_ENABLED=true
BITDEER_PROXY_URL=socks5://127.0.0.1:10808
BITDEER_DEGRADED_THRESHOLD=5  # 连续 5 次 429 后降级
BITDEER_RECOVERY_THRESHOLD=10  # 连续 10 次成功后恢复直连
BITDEER_QUEUE_DELAY_MS=100-500  # 降级模式下的排队延迟范围
```

## 步骤 4：数据库配置（可选）

如果希望通过后台管理界面控制，可以在 `channels` 表添加：

```sql
ALTER TABLE channels ADD COLUMN proxy_url TEXT DEFAULT NULL;
ALTER TABLE channels ADD COLUMN proxy_enabled BOOLEAN DEFAULT false;
ALTER TABLE channels ADD COLUMN degraded_mode BOOLEAN DEFAULT false;
```

## 监控指标

建议记录以下指标：
- 直连模式请求数
- 代理模式请求数
- 降级触发次数
- 恢复直连次数
- 各代理 IP 的成功率

## 注意事项

1. **代理延迟**：通过 Cloudflare CDN 会增加 50-200ms 延迟
2. **代理稳定性**：订阅链接需要保持有效，建议定期检查
3. **成本**：Xray 代理本身免费，但订阅服务可能有费用
4. **IP 池管理**：当前配置了 5 个代理，可以根据订阅内容扩展到 16 个

## 回滚方案

如果代理出现问题：

```bash
# 停止代理服务
sudo systemctl stop xray-proxy

# 或者通过环境变量禁用
BITDEER_PROXY_ENABLED=false
```

系统会自动回退到直连模式。
