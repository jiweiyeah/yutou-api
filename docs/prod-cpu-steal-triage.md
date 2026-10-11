# 生产 CPU 高排查报告

**时间**：2026-09-27 01:42–02:41 UTC（= 09:42–10:41 北京时间）
**主机**：`yutou-prod` — 4 核 KVM 虚拟机（Intel Xeon Gold 6138 @ 2.00GHz），3.8 GiB 内存

---

## TL;DR

CPU 高由**两件事叠加**造成，都不是网关流量：

1. **一个手动全量探测脚本** `/tmp/kb2.sh` 用 `xargs -P 8` 并发 8 个 `curl`，
   逐个查询渠道 10821 全部 **11478 个 Kite key** 的余额，持续压满 CPU **59 分钟**。
2. **云主机 CPU 被限流（steal）**：这台 4 核实例是共享/超卖型，基线 steal 就有约 10%；
   被自己的负载压满后，steal 飙升到 **65%**，实际可用算力只剩约 1.4 核。

脚本结束后 steal 立刻回落到 9–17%，load 从 12.03 降到 2.52 —— **因果明确**。

---

## 时间线

| 时刻 (UTC) | 事件 |
|---|---|
| 01:40:12 | 用户从 `113.81.199.202` SSH 登录（连续多个会话） |
| 01:42:15 | `/tmp/kb2.sh` 启动（`nohup` 式后台，ppid=1），8 并发 curl 开始 |
| 02:34 | 排查开始：`load 12.03`、`st=64.6%` |
| 02:41 | 脚本自然结束，共跑 59 分钟，产出 11458 条采样 |
| 02:43 | 复测：`load 2.52`、steal 9–17%、new-api CPU 25.5% |

---

## 证据链

### 1. 负载构成：容器只占小头

```
docker stats:
  new-api          66.54%   ← 单核百分比，≈0.67/4 核
  new-api-postgres  9.15%
  new-api-redis     1.73%
```
容器合计仅约 76% 单核（≈19% 总算力），但 `load average` 是 12.03。**说明大头在宿主侧。**

### 2. 宿主 top：steal 是主因，curl 是推手

```
%Cpu(s): 25.1 us, 8.6 sy, 1.1 id, 0.0 wa, 0.6 si, 64.6 st
   PID     USER  %CPU  COMMAND
2301424  root    32.5  curl -s --max-time 20 https://delayed-inference.prod.gokite.ai/v1/credits ...
2301450  root    22.5  curl ... (同上)
2301459  root    22.5  curl ...
... 共 8 个并发 curl
```
`st`(steal) = 64.6% 意味着**近 2/3 的 CPU 时间被 hypervisor 抢走**。
`vmstat 1 3` 连续三次 st 均为 67–68%，`/proc/pressure/cpu` → `some avg300=81.29`（持续高压）。

### 3. 元凶定位

```
ps -eo pid,ppid,etime,args --sort=start_time | grep -E "bash|\.sh"
2213091  1  52:58  bash /tmp/kb2.sh
```

脚本文件已被删除，通过进程 fd 读回原文：

```bash
#!/bin/bash
docker exec new-api-postgres psql -U newapi -d newapi -t -A \
  -c "SELECT key FROM channels WHERE id=10821" > /tmp/k2.txt
awk '{print NR-1, $0}' /tmp/k2.txt > /tmp/k2i.txt
cat /tmp/k2i.txt | xargs -P 8 -n 2 bash -c '
i="$0"; k="$1"
b=$(curl -s --max-time 20 https://delayed-inference.prod.gokite.ai/v1/credits \
     -H "Authorization: Bearer $k" | grep -o "\"balance\":\"[^\"]*\"" | head -1 | sed "s/.*:\"//;s/\"//")
echo "$i ${b:-ERR}"
' > /tmp/k2r.txt
```

- 输入规模：渠道 10821 `key` 字段 708785 字节，**11478 个 key**
- 并发度：`-P 8`
- 单请求上限：`--max-time 20`
- 每个 curl 占 15–40% CPU（TLS 握手 + 8 路并发叠加）

### 4. 因果验证（关键）

| 指标 | 脚本运行中 | 脚本结束后 |
|---|---|---|
| `load average` (1m) | 12.03 | **2.52** |
| steal (`st`) | 64.6–68% | **9–17%** |
| runnable (`r`) | 9–11 | **0** |
| idle | 1.1% | **84–93%** |
| new-api CPU | 66.54% | **25.53%** |
| postgres CPU | 9.15% | **0.35%** |

⇒ steal 高**不是**纯粹的邻居噪声，而是自身负载压满共享型实例后被限流。
（15 天累计 steal = 8.65%，与回落后的 9–17% 吻合，说明 ~10% 是这台机器的**基线**。）

---

## 排除项

- **不是网关流量**：近 1 小时仅 157 个请求，new-api 本身很闲。
- **不是内存/IO**：内存 3.8 GiB 仅用 460 MiB，swap 几乎未用，`wa=0`。
- **不是入侵**：`/var/log/auth.log` 中该时段登录全部来自 `113.81.199.202`（用户本人 IP），公钥 `ED25519 SHA256:52q27Kp...`。

---

## 附带产出：渠道 10821 (marathon) Kite key 余额

脚本的 `/tmp/k2r.txt` 在中途被删除（脚本结尾的 `wc -l` 统计必然失败），
已通过 `cp /proc/<xargs_pid>/fd/1` 抢救为 `/tmp/k2r_recovered.txt`（160730 字节）。
**注意：硬链接抢救会失败**（`Invalid cross-device link`，`/proc` 属另一设备），必须用 `cp`。

采样 **11458 / 11478**（覆盖 99.8%）：

| 指标 | 值 |
|---|---|
| 探测失败 (ERR) | **6** |
| 余额 ≤ 0.10 | **1** |
| 余额 ≤ 0.50 | **15** |
| 均值 | 4.87 |

**余额最低的 15 个**（index / 余额 USD）：

```
   0  0.031219   ← 已低于 0.10 阈值
  76  0.258836
  57  0.340516
  71  0.348032
  90  0.348116
  59  0.361391
  35  0.395082
  46  0.415560
  44  0.437248
  54  0.438031
   2  0.439280
   8  0.451237
  16  0.455950
  25  0.473727
  40  0.480394
```

**探测失败的 6 个**：index `6304, 6307, 6309, 6312, 6313, 6314` —— 全部落在连续区间内，
疑似同一批导入后失效的 key。

### 交叉验证

本次手动全量探测的 `余额 ≤ 0.10 = 1 个`，与今早 `kite_credits` 定时任务
（task 9625，10:34 完成）报告的 `low_balance_keys: 1` **完全吻合**。
⇒ 修复后的定时任务（提交 `0df1f71e9`）判定正确，两条独立路径互证。

---

## 建议

1. **降低并发**：这类全量 key 探测把 `-P 8` 降到 `-P 2~3`，并加 `--connect-timeout 5`。
   11478 个 key 在 8 并发下耗时 59 分钟，CPU 收益远低于代价。
2. **优先看定时任务**：渠道 10821/10867 的余额巡检已由 `kite_credits` 任务覆盖
   （单轮约 3m40s、22939 个 key），手动全量跑通常不必再做。
3. **临时文件别放 `/tmp`**：`/tmp` 会被清理，且本次结果文件在运行中被删导致统计失败。
   改用 `/root/` 或加唯一后缀。
4. **steal 基线 ~10%** 是这台共享型实例的常态；若要彻底消除，需换独享型/计算型实例。
5. 余额耗尽的 index `0` 已由任务自动摘除（`multi_key_status_list {"0":3}`），无需手工处理。
