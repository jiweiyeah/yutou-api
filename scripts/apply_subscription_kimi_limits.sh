#!/bin/bash
# =====================================================
# 给 name 以「订阅卡」开头的令牌设置模型限制:
#   model_limits        = moonshotai/kimi-k2.5,moonshotai/kimi-k2.6,moonshotai/kimi-k2.7-code
#   model_limits_enabled = true
# 含：整表备份 + 更新前快照 + 数量校验(应=200) + 更新 + 结果验证 + 回滚提示
# 连接信息全部从容器实际环境读取，不硬编码库名/用户。
# 用法： bash scripts/apply_subscription_kimi_limits.sh
# =====================================================
set -uo pipefail
SERVER="root@69.165.75.53"

ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 "$SERVER" 'bash -s' <<'REMOTE'
set -euo pipefail

PGC=$(docker ps --format '{{.Names}}|{{.Image}}' | grep -i postgres | head -1 | cut -d'|' -f1)
[ -z "${PGC:-}" ] && { echo "❌ 未找到 postgres 容器，中止。"; exit 1; }
PGU=$(docker exec "$PGC" printenv POSTGRES_USER 2>/dev/null || echo postgres)
PGD=$(docker exec "$PGC" printenv POSTGRES_DB   2>/dev/null || echo postgres)
LIMITS='moonshotai/kimi-k2.5,moonshotai/kimi-k2.6,moonshotai/kimi-k2.7-code'
TS=$(date +%Y%m%d_%H%M%S)
echo "PG容器=$PGC  user=$PGU  db=$PGD"
PSQL() { docker exec "$PGC" psql -U "$PGU" -d "$PGD" -v ON_ERROR_STOP=1 -P pager=off "$@"; }

echo "===[1/6] 备份整个 tokens 表 → 宿主机 /tmp/tokens_backup_$TS.sql ==="
docker exec "$PGC" pg_dump -U "$PGU" -d "$PGD" -t tokens > "/tmp/tokens_backup_$TS.sql"
echo "  备份字节数: $(wc -c < "/tmp/tokens_backup_$TS.sql")"

echo "===[2/6] 更新前快照(受影响行) → /tmp/subcards_before_$TS.txt ==="
docker exec "$PGC" psql -U "$PGU" -d "$PGD" -P pager=off -c \
  "SELECT id,name,model_limits_enabled,model_limits FROM tokens WHERE name LIKE '订阅卡%' ORDER BY id" \
  > "/tmp/subcards_before_$TS.txt"
echo "  已保存。"

echo "===[3/6] 更新前数量确认(预期 200) ==="
PSQL -c "SELECT count(*) AS before_cnt FROM tokens WHERE name LIKE '订阅卡%';"

echo "===[4/6] 执行更新(psql 将打印 UPDATE <影响行数>) ==="
PSQL -c "UPDATE tokens SET model_limits='$LIMITS', model_limits_enabled=true WHERE name LIKE '订阅卡%';"

echo "===[5/6] 结果验证 ==="
echo "  -- 已正确设置的订阅卡数量(预期 200):"
PSQL -c "SELECT count(*) AS ok_cnt FROM tokens WHERE name LIKE '订阅卡%' AND model_limits_enabled=true AND model_limits='$LIMITS';"
echo "  -- 订阅卡中仍未正确设置的数量(预期 0):"
PSQL -c "SELECT count(*) AS missed FROM tokens WHERE name LIKE '订阅卡%' AND NOT (model_limits_enabled=true AND model_limits='$LIMITS');"
echo "  -- 抽样 5 行:"
PSQL -c "SELECT id,name,model_limits_enabled,model_limits FROM tokens WHERE name LIKE '订阅卡%' ORDER BY id LIMIT 5;"

echo "===[6/6] 完成 ==="
echo "Redis token 缓存 TTL=60s（SYNC_FREQUENCY 未设，默认 60），最多 60 秒后全部生效，无需删缓存。"
echo
echo "如需回滚:"
echo "  还原为空且关闭限制:"
echo "    docker exec $PGC psql -U $PGU -d $PGD -c \"UPDATE tokens SET model_limits='', model_limits_enabled=false WHERE name LIKE '订阅卡%';\""
echo "  或整表恢复:"
echo "    cat /tmp/tokens_backup_$TS.sql | docker exec -i $PGC psql -U $PGU -d $PGD"
REMOTE
