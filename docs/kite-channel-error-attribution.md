# 渠道 10821 / 10867 错误归因（2026-09-27）

两个渠道都是 `type=59`（Kite Delayed 适配器），共用**同一个 key 池**（`md5(key)` 相同，
各 11511 个 key），只按 `base_url` 后缀分流：

| 渠道 | 名称 | 上游线路 | 上游端点 | 对外模型 |
|---|---|---|---|---|
| **10821** | `marathon` | Marathon（异步 job + 轮询） | `POST /v1/delayed/chat/completions` | `deepseek/deepseek-v4-pro`、`moonshotai/kimi-k3`、`z-ai/glm-5.2` |
| **10867** | `marathon-router-gray` | Kite Router（同步直连） | `POST /v1/chat/completions` | `anthropic/claude-opus-5`、`anthropic/claude-sonnet-5`、`openai/gpt-5.6-luna`、`openai/gpt-5.6-sol`、`moonshotai/kimi-k3`、`deepseek/deepseek-v4-pro` |

两者 `status=1`、`priority=0`、`weight=0`（同档均分），`group=default,y-api,y-api-paid`。
线上镜像 `b6a3124d1`，容器启动 `2026-09-27T17:57:48+08`。

## 一、先纠正口径：必须按 `request_id` 聚合

`logs` 里一条 `type=5` 是「**每次上游尝试**」，而 `RetryTimes=5` 意味着一次客户端请求
最多产生 6 条错误行。直接统计 `type=5` 会把失败率放大数倍。

```sql
WITH reqs AS (
  SELECT channel_id, request_id, bool_or(type=2) AS any_ok, count(*) AS attempts
  FROM logs WHERE channel_id IN (10821,10867) AND type IN (2,5)
    AND coalesce(request_id,'') <> ''
    AND created_at > extract(epoch from now())::bigint - 86400
  GROUP BY 1,2
)
SELECT channel_id, count(*) AS client_reqs,
       count(*) FILTER (WHERE NOT any_ok) AS fail,
       round(100.0*count(*) FILTER (WHERE NOT any_ok)/count(*),1) AS fail_pct,
       round(avg(attempts),2) AS avg_attempts FROM reqs GROUP BY 1;
```

| 窗口 | 渠道 | 客户端请求 | 最终失败 | **失败率** | avg_attempts |
|---|---|---|---|---|---|
| 近 24h | 10821 | 211 | 7 | **3.3%** | 1.00 |
| 近 24h | 10867 | 249 | 34 | **13.7%** | 1.13 |
| 近 6h | 10821 | 51 | 3 | 5.9% | 1.00 |
| 近 6h | 10867 | 131 | 24 | **18.3%** | 1.24 |

尝试级（未按请求聚合）看起来更吓人：10867 近 24h `type=5` 66 条 / `type=2` 215 条 = 23.5%。
**以客户端级 13.7% 为准。**

## 二、10867（Kite Router 线）：13.7% 的三个根因

### 1. ⭐ 上游边缘 60s 硬超时（最主要，且网关侧无解）

**直连复现**（绕过网关，同一 key，长输出请求）：

| 模型 | 结果 |
|---|---|
| `deepseek-v4-pro` | **504** @ 61.06s |
| `claude-opus-5` | **504** @ 61.06s |
| `gpt-5.6-sol` | **504** @ 61.75s |
| `claude-sonnet-5` | 200 @ 44.96s（未超 60s 即成功） |

返回体是 **Kite 自己的 nginx HTML**（`<html><head><title>504 Gateway Time-out</title>…`），
不是网关的错误体。生产侧 504 的耗时分布 **124/132 落在 60–67s**。

⇒ Router 是**同步接口**，请求发出后到拿到完整结果之间没有任何数据流动，
**任何生成耗时 > 60s 的请求必然 504**。这是上游结构性上限。

**对照：10821（Marathon）的 504 是另一回事** —— 25 次全部正好 `use_time = 300s`，
文案 `Kite Delayed poll failed: context deadline exceeded`，是**我们自己的 5 分钟 `pollTimeout`**。
两条线的 504 不要混为一谈。

### 2. 上游下游 provider 抖动 → `502 {"detail":"model service returned an error"}`

- **不是 key 问题**：502 散在几百个不同 key 上（top15 每个 1 次），无 401/403；
  且**同 key 池的 10821 只有 3.3% 失败**。
- 直连可复现的一个确定触发条件：请求体带 `parallel_tool_calls` 必 502
  （**已由 10867 的 `param_override` 以 `delete` 模式剥掉，故线上不是这个原因**）。
- 其余 502 与负载/供应商侧相关，网关无法干预。

### 3. 09-26 的 502 风暴已消失

| 日期 | 502 | 504 | context canceled |
|---|---|---|---|
| 09-26 | **419** | 107 | 13 |
| 09-27 | 24 | 25 | 15 |

09-26 的 419 次集中在已被摘掉的模型上：`deepseek-v4.1-flash`(203)、
`deepseek-v4-flash`(142)、`tencent/hy3`、`xiaomi/mimo-v2.5` ——
它们在 09-26 10:24~13:13 被逐批从 10867 的 `models` 里移除。
**残留 502 现在集中在 `claude-sonnet-5`(18) / `claude-opus-5`(6)**（Claude Platform on AWS 侧抖动）。

## 三、10821（Marathon 线）：3.3%，但有一个 100% 失败的路径

### ☠️ `/v1/responses` 在 Marathon 线 100% 失败（7 天 0 成功 / 4 失败）

错误体：

```
422 Kite Delayed submit failed: upstream returned HTTP 422:
{"detail":[{"type":"missing","loc":["body","messages"],"msg":"Field required",
 "input":{…,"instructions":"You are Codex, based on GPT-5…"}}]}
```

**完整的 Codex responses body 被原样发给了 Marathon submit 端点。**

根因在 `relay/channel/kitedelayed/adaptor.go:103`：

```go
func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	if !isKiteRouterChannel(info) {                       // ← 只有 /kite-router 后缀才转
		return a.Adaptor.ConvertOpenAIResponsesRequest(c, info, request)  // openai 适配器 = 原样透传
	}
	…
}
```

`isKiteRouterChannel` 只看 `base_url` 是否以 `/kite-router` 结尾 ⇒ **Marathon 线走原样透传**，
而上游 `/v1/delayed/chat/completions` 只认 `messages` ⇒ 必然 422。

**修法：把 responses→chat 转换做成无条件**（Marathon 端点同样只认 messages），1 处条件改动。
体量小（7 天 4 次），但**该路径 100% 失败** —— Codex 用户请求 `z-ai/glm-5.2`（Marathon 独占）必挂。

### 其余错误均为一次性 / 客户端侧

| 状态码 | 次数(7d) | 文案 | 定性 |
|---|---|---|---|
| 499 | 120 | `poll canceled by client: context canceled` | 客户端主动放弃（Marathon 异步慢）；09-23/09-24 各 54/36 次 |
| 400 | 55 | `unsupported Marathon model` | **全部集中在 09-25 23:32–23:36 的 5 分钟内**，正是 10821 编辑 `models` 的窗口（审计：23:27/23:29/23:36/23:38/23:43 五次编辑）。改完不再复现 |
| 504 | 26 | `poll failed: context deadline exceeded` | 我们自己的 300s 轮询超时 |
| 502 | 25 | `job failed: model service request failed` / nginx 502 | 上游作业失败 |
| 503 | 14 | html 503 | 上游过载 |
| 422 | 4 | `/v1/responses` 未转换 | 见上，100% 失败 |

## 四、已排除的原因（不要再查）

| 排除项 | 证据 |
|---|---|
| **key 池有问题** | `kite_credits` 任务连续 6 轮：`keys_checked:23022`、`request_errors:0`、`low_balance_keys:0`、`auth_failed_keys:0`、`disabled_keys:0`、`enabled_keys:0` |
| **跨渠道 key 禁停漂移** | 两渠道 `multi_key_status_list` 均为 `{"0":3}`，`md5(key)` 相同 |
| **渠道配置错** | `pass_through_body_enabled=false` ⇒ `param_override` 生效；`max_tokens>8192` 已被钳制、`parallel_tool_calls` 已被 delete |
| **网关 panic** | `docker logs new-api --since 6h \| grep -ci panic` = **0** |
| **代码不是最新** | 镜像 label = `b6a3124d1`，与 `git log` 顶部一致；容器 17:57:48 启动 |

## 五、重试行为的两个观察

1. **单候选模型的重试纯属浪费**：`claude-*`、`gpt-5.6-*` 在 10867 上是唯一渠道 ⇒
   链恒为 `"10867","10867",…,"10867"`（6 次），每次都打同一个坏上游。
2. ⚠️ **统计「跨渠道重试」时不能用 `chain LIKE '%,%'`** —— `"10867","10867"` 里也有逗号，
   会得出假阳性（曾误报 30/65 为跨渠道）。正确判据是链里**去重后不同 id 的个数 > 1**；
   按此统计，共享模型（deepseek-v4-pro、kimi-k3）也只偶有跨渠道（`"10870","10821"`）。

## 六、可落地建议

1. **修 Marathon 线的 `/v1/responses` 转换**（代码，1 处条件）—— 唯一 100% 失败的真 bug。
2. **`deepseek/deepseek-v4-pro` 与 `moonshotai/kimi-k3` 从 10867 的 `models` 摘掉**
   （或给 10821 更高 `priority`）：两渠道三方 `weight=0` 均分，而错误率差 4–15 倍：

   | 模型 | 10821 | 10867 |
   |---|---|---|
   | `deepseek/deepseek-v4-pro` | 2.3% | **36.4%** |
   | `moonshotai/kimi-k3` | 4.5% | 14.6% |

   这两模型留在 10867 是净亏。
3. **claude / gpt 系列无解**：Marathon 不支持它们（`/v1/delayed/*` 对 gpt 回
   `unsupported Marathon model`），只能留在 10867 并接受 **60s 输出上限**。
4. **小瑕疵**：`/v1/responses` 上客户端断开被记成 **500 `context canceled`**
   （`adaptor.go:539` 的 `doKiteRouterResponses` 未判 `upstreamCtx.Err()`），
   语义应为 499。仅影响归因，不影响计费。

## 复现命令

```bash
# 直连 Kite Router（注意剥掉 /kite-router 后缀）
B=https://delayed-inference.prod.gokite.ai
K=$(ssh yutou-prod "docker exec new-api-postgres psql -U newapi -d newapi -t -A \
     -c \"SELECT split_part(key, chr(10), 2) FROM channels WHERE id=10867\"" | tr -d '\r\n')
curl -s -o /tmp/r.json -w 'http=%{http_code} t=%{time_total}s\n' -m 200 \
  -X POST "$B/v1/chat/completions" -H "Authorization: Bearer $K" \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-opus-5","messages":[{"role":"user","content":"Write a 4000-word essay about the history of computing. Do not stop early."}],"max_tokens":4000}'
# 期望：http=504 t=61.0xxxxxs
```
