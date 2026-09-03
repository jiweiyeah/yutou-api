#!/bin/bash
# 动态代理切换部署脚本

set -e

echo "=========================================="
echo "动态代理切换部署脚本"
echo "=========================================="

# 检查是否在生产环境
if [ "$(hostname)" != "vm228-99-ubuntu" ]; then
    echo "错误: 请在生产服务器上运行此脚本"
    exit 1
fi

# 1. 备份现有配置
echo ""
echo "[1/6] 备份现有 Xray 配置..."
sudo cp /usr/local/etc/xray/config.json /usr/local/etc/xray/config.json.bak.$(date +%Y%m%d_%H%M%S)

# 2. 创建代理配置目录
echo ""
echo "[2/6] 创建 Xray 代理配置目录..."
sudo mkdir -p /usr/local/etc/xray-proxy

# 3. 写入代理配置
echo ""
echo "[3/6] 写入 Xray 代理配置..."
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

# 4. 创建 systemd 服务
echo ""
echo "[4/6] 创建 xray-proxy systemd 服务..."
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

# 5. 启动服务
echo ""
echo "[5/6] 启动 xray-proxy 服务..."
sudo systemctl daemon-reload
sudo systemctl enable xray-proxy
sudo systemctl start xray-proxy

# 等待服务启动
sleep 2

# 检查服务状态
if sudo systemctl is-active --quiet xray-proxy; then
    echo "✓ xray-proxy 服务启动成功"
else
    echo "✗ xray-proxy 服务启动失败"
    sudo systemctl status xray-proxy
    exit 1
fi

# 6. 验证代理可用性
echo ""
echo "[6/6] 验证代理可用性..."
PROXY_IP=$(curl -s -x socks5://127.0.0.1:10808 https://api.ipify.org || echo "FAILED")
if [ "$PROXY_IP" != "FAILED" ] && [ "$PROXY_IP" != "" ]; then
    echo "✓ 代理工作正常，出口 IP: $PROXY_IP"
else
    echo "✗ 代理验证失败"
    exit 1
fi

# 显示本机 IP 对比
LOCAL_IP=$(curl -s https://api.ipify.org)
echo ""
echo "=========================================="
echo "部署完成"
echo "=========================================="
echo "本机 IP: $LOCAL_IP"
echo "代理 IP: $PROXY_IP"
echo ""
echo "SOCKS5 代理: 127.0.0.1:10808"
echo "HTTP 代理:   127.0.0.1:10809"
echo ""
echo "下一步:"
echo "1. 添加环境变量到 docker-compose.yml:"
echo "   BITDEER_PROXY_URL=socks5://127.0.0.1:10808"
echo "   BITDEER_DEGRADED_THRESHOLD=5"
echo "   BITDEER_RECOVERY_THRESHOLD=10"
echo "   BITDEER_QUEUE_DELAY_MS=100-500"
echo ""
echo "2. 重启 new-api 容器:"
echo "   docker-compose restart new-api"
echo ""
echo "3. 查看代理统计:"
echo "   curl 'http://localhost:3000/api/option/proxy_stats/10836' -H 'Authorization: Bearer YOUR_TOKEN'"
echo "=========================================="
