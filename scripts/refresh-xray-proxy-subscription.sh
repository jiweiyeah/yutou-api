#!/usr/bin/env bash
#
# 从订阅链接刷新 xray-proxy 的出站节点池。
#
# 用途：new-api 的动态代理降级（relay/proxy）在渠道连续 429 后会把请求切到
# socks5://127.0.0.1:10808，该端口由独立的 xray-proxy 服务提供。本脚本负责把
# 订阅里的 VLESS 节点转成 xray 配置并热重载，供 cron 每 2 小时执行一次。
#
# 设计要点：
#   - 只写 /usr/local/etc/xray-proxy/，绝不碰对外 VLESS 服务端的
#     /usr/local/etc/xray/config.json 与 xray.service。
#   - 新配置先用 `xray test` 校验、再原子替换并重启；失败则保留旧配置退出，
#     宁可继续用过期节点，也不要让 10808 变成黑洞。
#   - 重启后做一次真实出口探测，失败即回滚到上一份配置。
#
# 用法：
#   refresh-xray-proxy-subscription.sh              # 用默认订阅地址
#   SUB_URL=https://... refresh-xray-proxy-subscription.sh
#
set -euo pipefail

SUB_URL="${SUB_URL:-https://yes.mytunnel.kdns.fr/sub?token=85ca9a8fb4940f9e1c54b3a1b28f9989}"
CONF_DIR="${CONF_DIR:-/usr/local/etc/xray-proxy}"
CONF="$CONF_DIR/config.json"
SOCKS_PORT="${SOCKS_PORT:-10808}"
HTTP_PORT="${HTTP_PORT:-10809}"
# new-api 跑在容器里，必须能连到宿主的代理端口，所以监听容器网桥网关而非仅 127.0.0.1。
# 172.18.0.1 是 compose 里固定的 gateway。端口不对公网开放，仅靠 LISTEN_ADDR 限制作用域，
# 因此该地址必须是内网网关，绝不可改成 0.0.0.0。
LISTEN_ADDR="${LISTEN_ADDR:-172.18.0.1}"
SERVICE="${SERVICE:-xray-proxy}"
XRAY_BIN="${XRAY_BIN:-/usr/local/bin/xray}"
MAX_NODES="${MAX_NODES:-16}"
PROBE_URL="${PROBE_URL:-https://api.ipify.org}"

log() { echo "[$(date '+%F %T')] $*"; }
die() { log "ERROR: $*"; exit 1; }

command -v "$XRAY_BIN" >/dev/null || die "xray not found at $XRAY_BIN"
command -v python3 >/dev/null || die "python3 not found"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "fetching subscription..."
if ! curl -fsS --max-time 30 -A "v2rayNG/1.8.0" "$SUB_URL" -o "$TMP/sub.raw"; then
    die "subscription fetch failed, keeping existing config"
fi
[ -s "$TMP/sub.raw" ] || die "subscription is empty, keeping existing config"

# 订阅是 base64 编码的 vless:// 列表；解码失败时按明文再试一次。
if ! base64 -d < "$TMP/sub.raw" > "$TMP/sub.txt" 2>/dev/null; then
    cp "$TMP/sub.raw" "$TMP/sub.txt"
fi

log "building xray config..."
SOCKS_PORT="$SOCKS_PORT" HTTP_PORT="$HTTP_PORT" MAX_NODES="$MAX_NODES" LISTEN_ADDR="$LISTEN_ADDR" \
python3 - "$TMP/sub.txt" "$TMP/config.json" <<'PY'
import json, os, sys, urllib.parse

src, dst = sys.argv[1], sys.argv[2]
max_nodes = int(os.environ["MAX_NODES"])
listen_addr = os.environ["LISTEN_ADDR"]

outbounds, tags, seen = [], [], set()
for line in open(src, encoding="utf-8", errors="ignore"):
    line = line.strip()
    if not line.startswith("vless://"):
        continue
    u = urllib.parse.urlparse(line)
    if not (u.hostname and u.port and u.username):
        continue
    q = urllib.parse.parse_qs(u.query)
    # 同一 IP:PORT 只保留一个，避免订阅内重复节点让负载均衡分布失衡
    key = (u.hostname, u.port)
    if key in seen:
        continue
    seen.add(key)

    net = q.get("type", ["tcp"])[0]
    sec = q.get("security", ["none"])[0]
    sni = (q.get("sni") or q.get("host") or [""])[0]

    stream = {"network": net, "security": sec}
    if sec == "tls":
        stream["tlsSettings"] = {
            "serverName": sni,
            "fingerprint": q.get("fp", ["chrome"])[0],
        }
    if net == "ws":
        stream["wsSettings"] = {"path": q.get("path", ["/"])[0]}
        if sni:
            stream["wsSettings"]["headers"] = {"Host": sni}

    tag = f"proxy-{len(outbounds) + 1}"
    outbounds.append({
        "tag": tag,
        "protocol": "vless",
        "settings": {"vnext": [{
            "address": u.hostname,
            "port": u.port,
            "users": [{"id": u.username, "encryption": q.get("encryption", ["none"])[0]}],
        }]},
        "streamSettings": stream,
    })
    tags.append(tag)
    if len(outbounds) >= max_nodes:
        break

if not outbounds:
    sys.exit("no usable vless node parsed from subscription")

conf = {
    "log": {"loglevel": "warning"},
    "inbounds": [
        {"tag": "socks-in", "listen": listen_addr, "port": int(os.environ["SOCKS_PORT"]),
         "protocol": "socks", "settings": {"udp": False}},
        {"tag": "http-in", "listen": listen_addr, "port": int(os.environ["HTTP_PORT"]),
         "protocol": "http"},
    ],
    "outbounds": outbounds + [{"tag": "direct", "protocol": "freedom"}],
    "routing": {
        # leastPing 需要 observatory；这里用 random 轮询，目的就是分散出口 IP
        "balancers": [{"tag": "proxy-balancer", "selector": ["proxy-"], "strategy": {"type": "random"}}],
        "rules": [{"type": "field", "inboundTag": ["socks-in", "http-in"], "balancerTag": "proxy-balancer"}],
    },
}
json.dump(conf, open(dst, "w", encoding="utf-8"), indent=2, ensure_ascii=False)
print(f"parsed {len(outbounds)} nodes")
PY

log "validating new config..."
# 注意：本版本 Xray（26.x）没有独立的 `xray test` 子命令，校验要用 `run -test`。
"$XRAY_BIN" run -test -config "$TMP/config.json" >"$TMP/test.log" 2>&1 \
    || { cat "$TMP/test.log"; die "config validation failed, keeping existing config"; }

mkdir -p "$CONF_DIR"
if [ -f "$CONF" ]; then
    cp "$CONF" "$TMP/config.json.prev"
fi

install -m 600 "$TMP/config.json" "$CONF"
log "restarting $SERVICE..."
systemctl restart "$SERVICE" || die "systemctl restart $SERVICE failed"

# 起服务后探一次真实出口，确认 10808 不是黑洞
sleep 3
ok=0
for i in 1 2 3; do
    if ip="$(curl -fsS --max-time 20 -x "socks5h://$LISTEN_ADDR:$SOCKS_PORT" "$PROBE_URL" 2>/dev/null)"; then
        log "proxy egress OK via $ip (attempt $i)"
        ok=1
        break
    fi
    log "egress probe attempt $i failed, retrying..."
    sleep 3
done

if [ "$ok" -ne 1 ]; then
    if [ -f "$TMP/config.json.prev" ]; then
        log "egress probe failed, rolling back to previous config"
        install -m 600 "$TMP/config.json.prev" "$CONF"
        systemctl restart "$SERVICE" || true
    fi
    die "new node pool cannot reach $PROBE_URL"
fi

log "done."
