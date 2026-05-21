#!/usr/bin/env python3
"""批量改 token 的有效期和额度 + 自动清 Redis 缓存（一条命令搞定）。

宿主机上运行，直接读写挂载出来的 SQLite 文件，并通过 docker exec 清 Redis。
零外部依赖（只用 Python 标准库）。

用法
----
    # dry-run（默认，不写库不清缓存）
    python3 update_tokens.py tokens.txt

    # 真跑（自动备份 DB + 改 DB + 清 Redis）
    python3 update_tokens.py tokens.txt --apply

    # 改路径/容器名/密码
    python3 update_tokens.py tokens.txt --apply \\
        --db /opt/new-api/data/one-api.db \\
        --redis-container new-api-redis \\
        --app-container new-api

txt 格式（每行一条，空白分隔，# 之后视为注释）
---------------------------------------------
    <KEY>  <EXPIRE>  <QUOTA>

    KEY    完整密钥，可带 sk- 前缀
    EXPIRE - "YYYY-MM-DD HH:MM:SS" 绝对时间（北京时间 UTC+8，可含空格）
           - "YYYY-MM-DD"          只到日，自动补 23:59:59（北京时间）
           - never / forever / 永久 / -1 / -    永不过期
    QUOTA  - 美元金额（整数或小数），自动换算并把 unlimited_quota 置为 false
           - unlimited / 无限 / -1 / -          无限额度（unlimited_quota = true）
"""
import argparse
import os
import re
import shutil
import sqlite3
import subprocess
import sys
from datetime import datetime, timezone, timedelta

CST = timezone(timedelta(hours=8))

FOREVER_TOKENS = {"never", "forever", "永久", "无期限", "无限期", "-1", "-"}
UNLIMITED_TOKENS = {"unlimited", "无限", "无限额", "-1", "-"}

DATETIME_FORMATS = (
    "%Y-%m-%d %H:%M:%S",
    "%Y-%m-%d %H:%M",
    "%Y-%m-%d",
    "%Y/%m/%d %H:%M:%S",
    "%Y/%m/%d",
)


def parse_expire(text):
    s = text.strip()
    if s.lower() in FOREVER_TOKENS or s in FOREVER_TOKENS:
        return -1
    for fmt in DATETIME_FORMATS:
        try:
            dt = datetime.strptime(s, fmt)
            if fmt in ("%Y-%m-%d", "%Y/%m/%d"):
                dt = dt.replace(hour=23, minute=59, second=59)
            dt = dt.replace(tzinfo=CST)
            return int(dt.timestamp())
        except ValueError:
            continue
    raise ValueError(f"无法解析时间: {text!r}")


def parse_quota(text, qpu):
    s = text.strip()
    if s.lower() in UNLIMITED_TOKENS or s in UNLIMITED_TOKENS:
        return 0, 1
    try:
        d = float(s)
    except ValueError:
        raise ValueError(f"无法解析额度: {text!r}")
    if d < 0:
        raise ValueError(f"额度不能为负: {text!r}")
    return int(round(d * qpu)), 0


def parse_line(line, qpu):
    parts = re.split(r"\s+", line.strip())
    if len(parts) < 3:
        raise ValueError("字段数不足，至少 3 段: <key> <expire> <quota>")
    key = parts[0]
    quota_raw = parts[-1]
    expire_raw = " ".join(parts[1:-1])
    if key.startswith("sk-"):
        key = key[3:]
    expired_time = parse_expire(expire_raw)
    remain_quota, unlimited = parse_quota(quota_raw, qpu)
    return key, expired_time, remain_quota, unlimited


def mask(key):
    return (key[:4] + "*" * 10 + key[-4:]) if len(key) > 8 else "*" * len(key)


def autodetect_redis_password(app_container):
    """从 app 容器的 REDIS_CONN_STRING 环境变量提取密码。"""
    try:
        out = subprocess.check_output(
            ["docker", "inspect", app_container,
             "--format", "{{range .Config.Env}}{{println .}}{{end}}"],
            text=True, stderr=subprocess.DEVNULL, timeout=10,
        )
    except Exception:
        return None
    for line in out.splitlines():
        if line.startswith("REDIS_CONN_STRING="):
            url = line[len("REDIS_CONN_STRING="):]
            m = re.search(r"redis://[^:]*:([^@]+)@", url)
            if m:
                return m.group(1)
    return None


def clear_redis_token_cache(redis_container, password):
    """通过 docker exec 清掉 Redis 里所有 token:* 键。返回 (ok, message)。"""
    cmd = (
        f"docker exec {redis_container} sh -c "
        f"'redis-cli -a {sh_quote(password)} --no-auth-warning --scan --pattern \"token:*\" "
        f"| xargs -r redis-cli -a {sh_quote(password)} --no-auth-warning del'"
    )
    try:
        out = subprocess.check_output(
            cmd, shell=True, text=True, stderr=subprocess.STDOUT, timeout=30,
        )
        return True, (out.strip() or "0")
    except subprocess.CalledProcessError as e:
        return False, e.output


def sh_quote(s):
    """单引号包裹 shell 字符串（密码里可能有特殊字符）。"""
    return "'" + s.replace("'", "'\"'\"'") + "'"


def main():
    ap = argparse.ArgumentParser(description="批量改 token 有效期/额度 + 自动清 Redis 缓存")
    ap.add_argument("txt", help="输入文本文件")
    ap.add_argument("--db", default="/opt/new-api/data/one-api.db", help="宿主机 SQLite 路径")
    ap.add_argument("--apply", action="store_true", help="实际执行（默认 dry-run）")
    ap.add_argument("--quota-per-unit", type=float, default=500000.0,
                    help="$1 等价 quota 数，默认 500000（common.QuotaPerUnit）")
    ap.add_argument("--no-backup", action="store_true", help="不自动备份 DB")
    ap.add_argument("--no-redis", action="store_true", help="跳过 Redis 缓存清理")
    ap.add_argument("--redis-container", default="new-api-redis")
    ap.add_argument("--app-container", default="new-api",
                    help="读取 REDIS_CONN_STRING 的容器名")
    ap.add_argument("--redis-password", help="Redis 密码（不传则自动从 app 容器 env 解析）")
    args = ap.parse_args()

    if not os.path.isfile(args.txt):
        sys.exit(f"❌ 文件不存在: {args.txt}")
    if not os.path.isfile(args.db):
        sys.exit(f"❌ 数据库不存在: {args.db}")

    plans, errors = [], []
    with open(args.txt, encoding="utf-8") as f:
        for line_no, raw in enumerate(f, 1):
            line = raw.split("#", 1)[0].strip()
            if not line:
                continue
            try:
                k, e, q, u = parse_line(line, args.quota_per_unit)
                plans.append((line_no, k, e, q, u))
            except ValueError as ex:
                errors.append((line_no, raw.rstrip("\n"), str(ex)))

    if errors:
        print(f"❌ 解析错误 {len(errors)} 处，中止：")
        for ln, raw, msg in errors:
            print(f"  第 {ln} 行: {msg}")
            print(f"    {raw}")
        sys.exit(1)

    if not plans:
        sys.exit("⚠️  无可处理行")

    print(f"📋 解析出 {len(plans)} 条更新计划：")
    print()
    print(f"  {'#':>4}  {'KEY':<22}  {'expired_time':<26}  {'remain_quota':>14}  {'unlimited':>9}")
    print(f"  {'-'*4}  {'-'*22}  {'-'*26}  {'-'*14}  {'-'*9}")
    for ln, k, e, q, u in plans:
        es = "permanent (-1)" if e == -1 else datetime.fromtimestamp(e, CST).strftime("%Y-%m-%d %H:%M:%S CST")
        print(f"  {ln:>4}  {mask(k):<22}  {es:<26}  {q:>14,}  {'yes' if u else 'no':>9}")
    print()

    if not args.apply:
        print("🔍 dry-run 模式，未改动。加 --apply 实际执行。")
        return

    if not args.no_backup:
        ts = datetime.now().strftime("%Y%m%d-%H%M%S")
        bak = f"{args.db}.bak.{ts}"
        print(f"📦 备份 {args.db} -> {bak}")
        shutil.copy2(args.db, bak)

    conn = sqlite3.connect(args.db)
    conn.execute("PRAGMA busy_timeout = 30000")
    cur = conn.cursor()
    updated, missing = [], []
    try:
        cur.execute("BEGIN")
        for ln, k, e, q, u in plans:
            cur.execute(
                "UPDATE tokens SET expired_time = ?, remain_quota = ?, unlimited_quota = ? "
                "WHERE `key` = ? AND deleted_at IS NULL",
                (e, q, u, k),
            )
            (updated if cur.rowcount > 0 else missing).append((ln, k))
        conn.commit()
    except Exception as e:
        conn.rollback()
        sys.exit(f"❌ 事务回滚，未写入: {e}")
    finally:
        conn.close()

    print(f"✅ 数据库更新 {len(updated)} 条")
    if missing:
        print(f"⚠️  未匹配 {len(missing)} 条（key 不存在或已被软删除）：")
        for ln, k in missing:
            print(f"    第 {ln} 行: {mask(k)}")

    if args.no_redis:
        print("ℹ️  --no-redis 已跳过 Redis 缓存清理")
        return

    pwd = (args.redis_password
           or os.environ.get("REDIS_PASSWORD")
           or autodetect_redis_password(args.app_container))
    if not pwd:
        print(f"⚠️  无法自动获取 Redis 密码（容器 {args.app_container} 未找到 REDIS_CONN_STRING）")
        print(f"    可手动: docker exec {args.redis_container} redis-cli -a PASSWORD --scan --pattern 'token:*' \\\n            | xargs -r redis-cli -a PASSWORD del")
        return

    print("🧹 清理 Redis token:* 缓存...")
    ok, msg = clear_redis_token_cache(args.redis_container, pwd)
    if ok:
        print(f"   删除条数: {msg}")
    else:
        print(f"❌ Redis 清理失败:\n{msg}")
        print("   DB 已改但缓存未清；最长 60s 后 TTL 自动过期。")


if __name__ == "__main__":
    main()
