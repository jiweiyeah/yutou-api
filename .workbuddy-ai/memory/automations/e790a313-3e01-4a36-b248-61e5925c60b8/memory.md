# 自动化：atria (10864) 多 key 池回收

## 目的
把 atria-asi 渠道里「上游已恢复、但仍处于自动禁用 (status=3)」的 key 重新启用。
（`deepseek_free_tier_recovery` 任务只覆盖 deepseek / tokenharbor 分组，atria 无自动化。）

## 执行摘要

### 2026-09-27 09:31（首次执行）
- 渠道 10864 `atria-asi`，status=1，multi_key_size=101。
- 起始状态：`multi_key_status_list` = 10 个 status=3（idx 0,1,2,3,4,5,7,9,18,34），
  enabled_count=91 / manual_disabled=0 / auto_disabled=10。
- `multi_key_disabled_until` **为空** → 均为请求路径触发的永久禁用，不会自愈。
  禁用原因：9 个 `status_code=429, Token quota exhausted.`，1 个（idx 34）`408 响应时间超阈值`。
- 逐个直连 `POST https://api.atria-asi.ai/v1/chat/completions`（model `Atria-Dawn-Preview`，
  max_tokens=4，每次间隔 3s）：**10/10 返回 HTTP 200**。
- 逐个 `POST /api/channel/multi_key/manage` action=enable_key：10/10 `success:true`。
- 终态：`multi_key_status_list` = `{}`，enabled_count=101，可用池 **101/101**。
- 无残留临时文件（未落任何含 key 的文件）。

### 2026-09-27 10:37（第二次执行）—— 无待回收
- 渠道 10864 起始即 `multi_key_status_list = {}`（104 key 全 status=1）⇒ **没有 status=3**，
  未探活、未调用 enable_key，按「无待回收 key」结束。
- 管理 API `get_key_status` 交叉核对：`total=104 enabled=104 manual_disabled=0 auto_disabled=0`。
- key 池从 101 → **104**：审计 `09-27 09:53:30 yeheboo Updated channel atria-asi changed_fields=["key"]`
  ⇒ 人工加了 3 个 key（不是自动化行为）。
- 清理了服务端 09-23 遗留的 `/tmp/r.json`（含 key 的错误响应体）。
- **教训：本任务大概率空跑。** 先查状态分布，无 status=3 就立刻结束，不要先探活再判断
  （会白打 100+ 次上游请求并自伤 429 基线）。

### 2026-09-29 18:20（第三次执行）—— 有 7 个待回收，净恢复 6 个
- 渠道 10864 `atria-asi`，status=1，multi_key_size=**119**（较 09-27 的 104 又涨了 15）。
- 起始 `multi_key_status_list` = 7 个 status=3（idx 0,1,3,5,7,8,10），
  `multi_key_disabled_until` 为空 ⇒ 永久禁用；reason 全部 `status_code=429, Token quota exhausted.`。
- 首轮探活（间隔 3s）：**idx 0/1/7/8/10 → 200**；**idx 3/5 → 429**（`Cluster RPM rate limit exceeded.`）。
- 先启用 5 个 200 的 → 5/5 `success:true`。
- **补救 429 假阴性**：`sleep 70` 后单独补测 idx 3/5（间隔 8s）→ **双双 200** ⇒ 是自伤不是坏 key
  → 一并启用 → 2/2 `success:true`。**本次 7/7 全部探活通过并启用**。
- **终态 `{"0":3}`**：idx 0 在 **18:24:29**（启用后约 4 分钟）被**真实流量**以
  `status_code=429, Token quota exhausted.` **重新自动摘掉** ⇒ 它确实没恢复，自愈机制按预期工作。
- 最终：`get_key_status` → total=119 / enabled=118 / manual_disabled=0 / auto_disabled=**1**
  ⇒ **可用池 118/119**。
- 无残留临时文件（全程 `bash -s` + 变量，未落盘；`/tmp/r.json`、`/tmp/r2.json` 均确认不存在）。
- ⚠️ 18:25 那批 `Cluster RPM rate limit exceeded.` 分散在 idx 1/37/48/53/94（非本次新启用项），
  是**生产流量**撞上游集群限流，与本次操作无关。

## 硬事实 / 踩坑记录
- **探测脚本必须把 key 留在远端 shell 变量里**，不能 `psql -c ... > /tmp/k.txt` 再 cat；
  用 `$(docker exec ... psql -t -A -c "SELECT split_part(key, chr(10), N) ...")` 直接进变量。
  输出侧用 `sed "s/$k/REDACTED/g"` 兜底，防止上游错误体回显 key。
- `ssh host 'bash -s' <<'EOS'` 里的 `docker exec` **不能带 `-i`**（否则吃掉 heredoc 剩余脚本）。
  本任务全部 psql 都用 `-c`，无需 `-i`。
- 管理 API 鉴权：`Authorization` = users(role=100).access_token **裸值**（长度 32）+ `New-Api-User: 1`，
  Host 头 `yutou.bestvirtualgoods.com`，端口 8443，`curl -k`。
- 渠道 10864 上 `multi_key_status_list` 全清空后表现为 `{}`（不是 null）→ 判「全部启用」看这个。
- 上游 atria 对 `Atria-Dawn-Preview` 的 200 响应常带 `content:null` + `reasoning_content`，
  这是正常现象（reasoning 模型），不代表 key 有问题。

---

## ⚠️ 2026-10-02 审计：本任务已连续 3 轮执行失败（零输出）

**下一轮执行前必读** —— 池子里积了 10 个待回收 key，且已实测确认可用。

### 执行历史（`automation_runs` 表，不是 `system_tasks`）
| 时间 | 结果 |
|---|---|
| 09-27 09:31 | ✅ 恢复 10/10 |
| 09-27 10:36 | ✅ 空跑 |
| 09-28 18:46 | ✅ 成功（未留痕） |
| 09-29 18:20 | ✅ 恢复 7/7，终态 118/119 |
| **09-30 13:20** | ❌ `automation-rate-limited`（8s，0 tokens） |
| **10-01 16:40** | ❌ `automation-run-interrupted` |
| **10-02 10:33** | ❌ `automation-run-interrupted`（88s 后会话结束） |

`automation_runtime_state.last_error` = `Conversation ended before automation request completed: error`
⇒ **平台侧问题**（会话在任务完成前结束），不是任务逻辑错。
⚠️ 实际执行时间与 `FREQ=DAILY;BYHOUR=10;BYMINUTE=30` 严重漂移（09-28 18:46、09-29 18:20、
10-01 16:40）⇒ 是 catch-up 补跑，调度不可靠。

### 当前待回收清单（2026-10-02 10:45 实测，全部可直连可用）
渠道 10864，池 **131 key**，`multi_key_status_list` 有 **10 个 status=3**：
`idx 0,1,2,3,4,5,6,7,9,18`
- `multi_key_disabled_until` **为空** ⇒ 永久禁用，不会自愈（符合本任务的处理条件）。
- 禁用时间全部落在 **09-29 18:24 ~ 09-30 01:23**，reason 全为 `status_code=429, Token quota exhausted.`。
- **逐个直连探活（间隔 3s，model `Atria-Dawn-Preview`，max_tokens=4）→ 10/10 全部 HTTP 200**
  ⇒ 直接走 `enable_key` 即可，无需再探活（但探活一遍也无害，只多 30s）。

### 质效复盘（用生产 logs 反推，别信任务自报）
```sql
SELECT (NULLIF(other,'')::jsonb->'admin_info'->>'multi_key_index')::int AS idx, count(*)
FROM logs WHERE created_at >= <epoch> AND type=2 AND other LIKE '%"10864"%'
GROUP BY 1 ORDER BY 2 DESC;
```
- 09-27 那批 10 个：**8 个真实恢复**（09-27~09-30 各承接 232~433 次成功）⇒ 有效。
- 09-29 那批 7 个：**只有 idx 8 / 10 存活**（09-30 后 278 / 295 次），其余 5 个 4 分钟~7 小时内被重摘 ⇒ 2/7。

### 副作用教训（本次新增，务必纳入判断）
09-29 18:20 回收后 **7 小时内池子又新增 10 个禁用**，其中 idx 2/4/6/9/18 **原本是启用状态**。
起点（18:24）紧贴回收操作 ⇒ 时间高度可疑，但 18:00-19:00 也是当天流量峰值，
且 skill 记录过「批量探活把 429 率从 0% 推到 ~26%」⇒ **不能排除探活自伤 + 上游波动叠加**。
**结论：探活批次要更保守**（间隔拉到 5-8s、单轮不超过 10 个），
且**回收后 10 分钟内不要对渠道指标下结论**。

### 建议
平台侧 automation 不可靠（7 轮 3 失败、调度漂移）⇒ 若再失败一次，
考虑改成**脚本 + cron**（走管理 API，任何能上网的机器都能跑，见 skill 的
「登录领额度 → 次日回收」范例），把可靠性从平台拿回来。

---

## ⛔ 2026-10-02：本自动化已被服务器端 system_task 取代，不要再执行回收

**用户决定**：不再依赖 WorkBuddy automation，改成像 `kite_credits` 那样的 new-api 内置任务。

**替代实现（已上线）**：
- `type='atria_key_recovery'`，代码 `service/atria_key_recovery_task.go` +
  `controller/system_task_handlers.go` 的 `atriaKeyRecoveryHandler`，commit `ba8dd3f32`，
  部署 run `36972166636`。
- 默认**每 360 分钟**跑一轮（`ATRIA_KEY_RECOVERY_INTERVAL_MINUTES`）。
- 2026-10-02 14:16:51 首轮实测：`auto_disabled_keys:1, keys_probed:1, keys_enabled:1`，
  6 秒完成，终态池 `131/131` 全可用。

**⚠️ 本轮若被触发，请直接结束，不要重复回收**：
- 回收逻辑已由 system_task 承担，重复执行只会白打上游请求、自伤 429 基线。
- 若确实被触发，只需回一句「已由 system_task `atria_key_recovery` 接管，本次跳过」即可。
- 需要看回收情况，去查生产库：
  `SELECT to_timestamp(updated_at) at time zone 'Asia/Shanghai', NULLIF(result,'')::jsonb
   FROM system_tasks WHERE type='atria_key_recovery' ORDER BY id DESC LIMIT 5;`

**✅ 2026-10-02 14:33 已暂停（`status=PAUSED`）**，不会再被调度。本文件到此为止，无需再维护。
