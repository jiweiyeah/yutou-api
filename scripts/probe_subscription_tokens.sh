#!/bin/bash
# =====================================================
# 只读探查脚本（不做任何写操作）
# 目的：
#   1. 确认生产库到底是 PostgreSQL 还是 MySQL、真实库名/用户
#   2. 查看 name 以「订阅卡」开头的令牌(token)的模型限制现状
# 用法： bash scripts/probe_subscription_tokens.sh
# =====================================================
set -uo pipefail
SERVER="root@69.165.75.53"

ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 "$SERVER" 'bash -s' <<'REMOTE'
set -uo pipefail
echo "================ 运行中的容器 ================"
docker ps --format '{{.Names}} | {{.Image}}'
echo

# 自动发现数据库容器（不依赖任何硬编码库名/用户）
PGC=$(docker ps --format '{{.Names}}|{{.Image}}' | grep -i postgres        | head -1 | cut -d'|' -f1)
MYC=$(docker ps --format '{{.Names}}|{{.Image}}' | grep -iE 'mysql|mariadb' | head -1 | cut -d'|' -f1)

if [ -n "${PGC:-}" ]; then
  PGU=$(docker exec "$PGC" printenv POSTGRES_USER 2>/dev/null || echo postgres)
  PGD=$(docker exec "$PGC" printenv POSTGRES_DB   2>/dev/null || echo postgres)
  echo "================ PostgreSQL  容器=$PGC  user=$PGU  db=$PGD ================"
  PSQL() { docker exec "$PGC" psql -U "$PGU" -d "$PGD" -P pager=off -c "$1"; }

  echo "--- [结构] tokens 表相关列是否存在 ---"
  PSQL "SELECT column_name, data_type FROM information_schema.columns WHERE table_name='tokens' AND column_name IN ('name','model_limits','model_limits_enabled') ORDER BY column_name;"

  echo "--- [统计] name 以 '订阅卡' 开头的令牌数量 ---"
  PSQL "SELECT count(*) AS subscription_tokens FROM tokens WHERE name LIKE '订阅卡%';"

  echo "--- [明细] 这些令牌当前的模型限制现状 ---"
  PSQL "SELECT id, name, model_limits_enabled, model_limits FROM tokens WHERE name LIKE '订阅卡%' ORDER BY id;"

  echo "--- [参考] name 含 '订阅' 的样例(确认前缀写法是否就是'订阅卡', 前20条) ---"
  PSQL "SELECT id, name FROM tokens WHERE name LIKE '%订阅%' ORDER BY id LIMIT 20;"
fi

if [ -n "${MYC:-}" ]; then
  MYP=$(docker exec "$MYC" printenv MYSQL_ROOT_PASSWORD 2>/dev/null || echo "")
  MYD=$(docker exec "$MYC" printenv MYSQL_DATABASE       2>/dev/null || echo "")
  echo "================ MySQL  容器=$MYC  db=${MYD:-?} ================"
  docker exec "$MYC" mysql -uroot ${MYP:+-p"$MYP"} ${MYD:+"$MYD"} -t -e \
    "SELECT count(*) AS subscription_tokens FROM tokens WHERE name LIKE '订阅卡%';
     SELECT id, name, model_limits_enabled, model_limits FROM tokens WHERE name LIKE '订阅卡%' ORDER BY id;
     SELECT id, name FROM tokens WHERE name LIKE '%订阅%' ORDER BY id LIMIT 20;"
fi

if [ -z "${PGC:-}" ] && [ -z "${MYC:-}" ]; then
  echo "⚠️ 未发现 postgres/mysql 容器，数据库可能是外部托管。完整容器列表："
  docker ps -a --format '{{.Names}} | {{.Image}} | {{.Status}}'
fi
REMOTE
