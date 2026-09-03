#!/usr/bin/env bash
#
# 从订阅链接刷新 xray-proxy 的出站节点池。
#
# 用途：new-api 的动态代理降级（relay/proxy）在渠道连续 429 后会把请求切到
# socks5://172.18.0.1:10808，该端口由独立的 xray-proxy 服务提供。本脚本负责把
# 订阅里的 VLESS 节点转成 xray 配置并热重载，供 cron 每 2 小时执行一次。
#
# 设计要点：
#   - 只写 /usr/local/etc/xray-proxy/，绝不碰对外 VLESS 服务端的
#     /usr/local/etc/xray/config.json 与 xray.service。
#   - 订阅里约有两成节点是死的（2026-09-04 实测：16 个节点 3 个连不通，
#     random 负载均衡下实测请求失败率 37.5%）。所以入池前**逐个探测**，
#     只保留真能出网的节点；否则降级只是把 429 换成硬超时，比不降级更糟。
#   - 出口 IP 轮换来自节点自身的上游，不依赖负载均衡在节点间轮转
#     （实测单节点 12 次请求给出 10 个不同的 104.28.x.x 出口）。因此策略用
#     leastPing：挑存活且最快的节点，IP 多样性不受影响，同时绕开死节点。
#   - observatory 持续探活，覆盖两次刷新之间节点掉线的情况。
#   - 新配置先用 `xray run -test` 校验、再原子替换并重启；失败则保留旧配置退出，
#     宁可继续用过期节点，也不要让 10808 变成黑洞。
#   - 重启后做一次真实出口探测，失败即回滚到上一份配置。
#
# 用法：
#   refresh-xray-proxy-subscription.sh              # 用默认订阅地址
#   SUB_URL=https://... refresh-xray-proxy-subscription.sh
#   SKIP_NODE_PROBE=1 refresh-xray-proxy-subscription.sh   # 跳过逐节点探测（应急）
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
# 逐节点探测用的临时 socks 端口，只监听 127.0.0.1，不与正式的 10808/10809 冲突。
NODE_PROBE_PORT="${NODE_PROBE_PORT:-11900}"
NODE_PROBE_TIMEOUT="${NODE_PROBE_TIMEOUT:-8}"
# 存活节点少于该值就认为订阅整体不可用，保留旧配置而不是装一个近乎空的池子。
MIN_ALIVE_NODES="${MIN_ALIVE_NODES:-3}"
SKIP_NODE_PROBE="${SKIP_NODE_PROBE:-0}"

log() { echo "[$(date '+%F %T')] $*"; }
die() { log "ERROR: $*"; exit 1; }

command -v "$XRAY_BIN" >/dev/null || die "xray not found at $XRAY_BIN"
command -v python3 >/dev/null || die "python3 not found"
command -v curl >/dev/null || die "curl not found"

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

# 第一步：把订阅解析成候选出站列表（还没探活，先不生成正式配置）。
log "parsing subscription into candidate outbounds..."
MAX_NODES="$MAX_NODES" python3 - "$TMP/sub.txt" "$TMP/candidates.json" <<'PY'
import json, os, sys, urllib.parse

src, dst = sys.argv[1], sys.argv[2]
max_nodes = int(os.environ["MAX_NODES"])

outbounds, seen = [], set()
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

    outbounds.append({
        "tag": f"proxy-{len(outbounds) + 1}",
        "protocol": "vless",
        "settings": {"vnext": [{
            "address": u.hostname,
            "port": u.port,
            "users": [{"id": u.username, "encryption": q.get("encryption", ["none"])[0]}],
        }]},
        "streamSettings": stream,
    })
    if len(outbounds) >= max_nodes:
        break

if not outbounds:
    sys.exit("no usable vless node parsed from subscription")

json.dump(outbounds, open(dst, "w", encoding="utf-8"), indent=2, ensure_ascii=False)
print(f"parsed {len(outbounds)} candidate nodes")
PY

CANDIDATE_COUNT="$(python3 -c "import json;print(len(json.load(open('$TMP/candidates.json'))))")"
ALIVE_TAGS=""

if [ "$SKIP_NODE_PROBE" = "1" ]; then
    log "SKIP_NODE_PROBE=1, 跳过逐节点探测，$CANDIDATE_COUNT 个候选全部入池"
    for i in $(seq 1 "$CANDIDATE_COUNT"); do ALIVE_TAGS="$ALIVE_TAGS proxy-$i"; done
else
    # 第二步：逐个探活。死节点在 random 均衡下会随机吞掉请求，
    # 结果是「降级」把上游 429 换成本地超时，比不降级更糟，所以必须先筛掉。
    log "probing $CANDIDATE_COUNT candidate nodes (timeout ${NODE_PROBE_TIMEOUT}s each)..."
    dead=0
    for idx in $(seq 0 $((CANDIDATE_COUNT - 1))); do
        tag="proxy-$((idx + 1))"
        addr="$(NODE_PROBE_PORT="$NODE_PROBE_PORT" python3 - "$TMP/candidates.json" "$idx" "$TMP/probe.json" <<'PY'
import json, os, sys

cands = json.load(open(sys.argv[1]))
ob = cands[int(sys.argv[2])]
json.dump({
    "log": {"loglevel": "error"},
    "inbounds": [{"listen": "127.0.0.1", "port": int(os.environ["NODE_PROBE_PORT"]),
                  "protocol": "socks", "settings": {"udp": False}}],
    "outbounds": [ob],
}, open(sys.argv[3], "w", encoding="utf-8"))
v = ob["settings"]["vnext"][0]
print(f"{v['address']}:{v['port']}")
PY
)"
        "$XRAY_BIN" run -config "$TMP/probe.json" >/dev/null 2>&1 &
        probe_pid=$!
        sleep 1
        if curl -fsS --max-time "$NODE_PROBE_TIMEOUT" \
                -x "socks5h://127.0.0.1:$NODE_PROBE_PORT" "$PROBE_URL" >/dev/null 2>&1; then
            ALIVE_TAGS="$ALIVE_TAGS $tag"
            log "  alive $tag $addr"
        else
            dead=$((dead + 1))
            log "  dead  $tag $addr"
        fi
        kill "$probe_pid" 2>/dev/null || true
        wait "$probe_pid" 2>/dev/null || true
    done

    alive_count="$(echo "$ALIVE_TAGS" | wc -w)"
    log "node probe result: alive=$alive_count dead=$dead total=$CANDIDATE_COUNT"
    [ "$alive_count" -ge "$MIN_ALIVE_NODES" ] \
        || die "only $alive_count alive node(s) < MIN_ALIVE_NODES=$MIN_ALIVE_NODES, keeping existing config"
fi

# 第三步：只用存活节点生成正式配置。
log "building xray config from alive nodes..."
SOCKS_PORT="$SOCKS_PORT" HTTP_PORT="$HTTP_PORT" LISTEN_ADDR="$LISTEN_ADDR" ALIVE_TAGS="$ALIVE_TAGS" \
python3 - "$TMP/candidates.json" "$TMP/config.json" <<'PY'
import json, os, sys

cands = json.load(open(sys.argv[1]))
alive = set(os.environ["ALIVE_TAGS"].split())
outbounds = [o for o in cands if o["tag"] in alive]
if not outbounds:
    sys.exit("no alive node to build config from")

listen_addr = os.environ["LISTEN_ADDR"]
conf = {
    "log": {"loglevel": "warning"},
    "inbounds": [
        {"tag": "socks-in", "listen": listen_addr, "port": int(os.environ["SOCKS_PORT"]),
         "protocol": "socks", "settings": {"udp": False}},
        {"tag": "http-in", "listen": listen_addr, "port": int(os.environ["HTTP_PORT"]),
         "protocol": "http"},
    ],
    "outbounds": outbounds + [{"tag": "direct", "protocol": "freedom"}],
    # 持续探活。刷新脚本每 2 小时才跑一次，节点在这中间掉线只能靠 observatory 发现。
    "observatory": {
        "subjectSelector": ["proxy-"],
        "probeURL": "https://www.gstatic.com/generate_204",
        "probeInterval": "3m",
    },
    "routing": {
        # 出口 IP 由节点自身上游逐请求轮换（实测单节点 12 次请求给出 10 个不同出口），
        # 不依赖在节点之间轮转，所以这里用 leastPing：只挑存活且最快的节点，
        # IP 多样性不受影响，又不会像 random 那样随机撞上死节点。
        "balancers": [{"tag": "proxy-balancer", "selector": ["proxy-"],
                       "strategy": {"type": "leastPing"}}],
        "rules": [{"type": "field", "inboundTag": ["socks-in", "http-in"],
                   "balancerTag": "proxy-balancer"}],
    },
}
json.dump(conf, open(sys.argv[2], "w", encoding="utf-8"), indent=2, ensure_ascii=False)
print(f"built config with {len(outbounds)} alive node(s)")
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
