# Kilo Gateway API Token 实测报告

- 测试时间：2026-09-22 00:08–00:12 GMT+8
- 被测凭证：用户提供的 JWT（`eyJhbGciOiJIUzI1NiIs...`，**不落盘**），payload：

```json
{"env":"production","kiloUserId":"590a6d37-91bc-494e-b396-afe83b6af729",
 "apiTokenPepper":null,"version":3,"iat":1790005582,"exp":1947685582}
```

| 字段 | 值 |
|---|---|
| `iat` | 2026-09-21 15:46:22 UTC |
| `exp` | **2031-09-20 15:46:22 UTC**（有效期 **1825 天 = 5 年**） |
| `version` | 3 |

---

## 一、结论速览

| 项 | 结果 |
|---|---|
| 凭证是否有效 | ✅ 有效 |
| 支持的鉴权头 | `Authorization: Bearer <jwt>` **和** `x-api-key: <jwt>` 都可以 |
| 免费模型 | ✅ 200 正常出结果，`usage.cost = 0` |
| 付费模型 | ❌ **402 `Paid Model - Credits Required`，`balance: 0`** |
| 账户余额 | **$0**（`total_microdollars_acquired: 0`，`microdollars_used: 0`）→ 纯免费档 |
| 流式 | ✅ SSE（OpenRouter 风格 chunk） |
| 工具调用 | ✅ 标准 `tool_calls`，`finish_reason: "tool_calls"` |
| Anthropic `/v1/messages` | ✅ **支持**，返回 Anthropic 格式 |
| 并发 12 路 | ✅ 全部 200，未触限 |
| 限流响应头 | ❌ 完全没有 `X-RateLimit-*` / `Retry-After` |
| 账户风控状态 | 未被封（`is_bot: false`，`blocked_reason: null`） |

**一句话**：这是一把**零余额的纯免费档 token**，只能打 `:free` 模型和 `kilo-auto/free`；免费模型不需要这把 key 也能打（匿名），key 的作用只是**换来了账户身份和付费模型的入口**（但没有余额所以进不去）。

---

## 二、账户信息（`GET https://api.kilo.ai/api/user`）

```json
{
  "id": "590a6d37-91bc-494e-b396-afe83b6af729",
  "google_user_email": "zc12k76hju@ofaldgw.com",
  "google_user_name": "zc12k76hju",
  "created_at": "2026-09-21 15:41:41.665631+00",
  "microdollars_used": 0,
  "total_microdollars_acquired": 0,
  "next_credit_expiration_at": null,
  "signup_ip": "154.202.119.148",
  "is_bot": false,
  "is_admin": false,
  "blocked_reason": null,
  "blocked_at": null,
  "personal_account_disabled": false,
  "has_validation_stytch": false,
  "has_validation_novel_card_with_hold": false,
  "auto_top_up_enabled": false,
  "completed_welcome_form": false,
  "openrouter_upstream_safety_identifier": "14f4b55f...",
  "openrouter_downstream_safety_identifier": "JK7xzSJBQ0wz...",
  "vercel_downstream_safety_identifier": "XwXPE9q6aPiY..."
}
```

要点：
- `id` 与 JWT 里的 `kiloUserId` **一致** → 这把 key 就是该账号的。
- 注册时间 **2026-09-21 15:41 UTC**（测试前约 25 分钟），邮箱域名 `ofaldgw.com`（临时邮箱）。
- `signup_ip = 154.202.119.148` —— 这是本机 Clash 代理的出口 IP（与既往实测记录一致）。
- **零充值、零消费、零额度**，也没有任何验证（stytch / 绑卡）。
- `stripe_customer_id` 存在 → 注册时就在 Stripe 建了 customer（但没绑卡）。
- 网关会把账号身份 hash 成 OpenRouter / Vercel 的 safety identifier 透传给上游。
- `/api/user` 不带 key → 401 `{"error":"Unauthorized","success":false}`。

---

## 三、逐项实测

### 1) 免费模型（带 key）

```bash
curl -X POST https://api.kilo.ai/api/gateway/v1/chat/completions \
  -H "Authorization: Bearer $K" -H "Content-Type: application/json" \
  -d '{"model":"kilo-auto/free","messages":[{"role":"user","content":"reply ok"}],"max_tokens":20}'
# → HTTP 200，model 落到 nex-agi/nex-n2.5-pro:free，usage.cost = 0
```

### 2) 付费模型（带 key）

```bash
-d '{"model":"deepseek/deepseek-v4.1-flash",...}'
# → HTTP 402
# {"error":{"title":"Paid Model - Credits Required",
#           "message":"Add credits to continue, or switch to a free model",
#           "balance":0,"buyCreditsUrl":"https://app.kilo.ai/profile"},
#  "error_type":"usage_limit_exceeded"}
```

`kilo-auto/efficient` 同样 402。**⚠️ 不存在的模型名（`no/such-model`）也返回 402 而不是 404** —— 排查模型名拼写时别被"没余额"误导。

### 3) 鉴权头形态

| 头 | 结果 |
|---|---|
| `Authorization: Bearer <jwt>` | ✅ 200 |
| `x-api-key: <jwt>` | ✅ 200 |
| 无任何头 + 免费模型 | ✅ 200（匿名通道） |
| 无任何头 + 付费模型 | 401 `PAID_MODEL_AUTH_REQUIRED` |
| 无效 key + 免费模型 | 401 `INVALID_TOKEN` ⚠️ 反而不如匿名 |

### 4) 流式 / 工具 / Anthropic 格式

- **流式**：`"stream":true` → 标准 SSE，chunk 形如
  `{"object":"chat.completion.chunk","choices":[{"delta":{"content":"","reasoning":"We",...}}]}`，
  带 `reasoning` 与 `reasoning_details` 字段（OpenRouter 扩展）。
- **工具调用**：传 `tools` 后正确返回
  `tool_calls: [{"id":"call_ab66...","function":{"name":"get_weather","arguments":"{\"city\": \"Beijing\"}"}}]`，
  `finish_reason: "tool_calls"`，`cost: 0`。
- **Anthropic 格式**：`POST /api/gateway/v1/messages` + `anthropic-version: 2023-06-01` → **200**，
  返回 `{"type":"message","role":"assistant","content":[{"type":"thinking"...}]}`。

### 5) 并发与限流

- 并发 12 路 `kilo-auto/free` → **12/12 HTTP 200**，无并发闸门。
- 响应头**完全没有**限流字段（只有 Vercel 的 `x-vercel-*`）。限流信息只能查文档。
- 官方口径：免费模型 **200 请求/小时/出口 IP**，匿名与已登录同池 → **这把 key 不提供任何额度扩容**。

### 6) 模型列表

| 请求 | 数量 |
|---|---|
| `GET /api/gateway/models`（无 key） | 379 |
| `GET /api/gateway/models`（带 key） | 379 |
| `GET /api/gateway/v1/models`（带 key） | 200，OpenAI 风格 `{"data":[...]}` |

带不带 key 拿到的列表一样（数量会随服务端调整变动，昨日为 380）。

---

## 四、观察到的异常

**HTTP 200 但 body 是 error** —— 首次调用 `nvidia/nemotron-3-super-120b-a12b:free` 时：

```json
{"id":"gen-1790006920-...","error":{"message":"Upstream error from Nvidia: Service temporarily overloaded",
 "code":503,"metadata":{"error_type":"provider_overloaded"}}}
```

HTTP 状态码是 **200**，但 body 里是错误对象。随后连打 3 次均正常。
⇒ 网关会把上游 5xx **包成 200 + error 对象**。接入时必须**检查 body 里是否存在 `error` 字段**，只看 HTTP 状态码会把失败当成功记账。

---

## 五、对 yutou-api 接入的含义

- 可作为一个 **OpenAI 兼容渠道**：`base_url = https://api.kilo.ai/api/gateway`，模型用 `:free` 后缀那 21 个 + `kilo-auto/free`；也支持 Anthropic `/v1/messages`。
- **⚠️ 多 key 无法扩容**：限流是 **per-IP 200/hr**，与 key 无关（匿名同池）。多 key 池在这个渠道上等于无效，扩容只能换出口 IP。这与 Braintrust「provider 层共享、换 key 不扩容」类似，但更彻底 —— 它连"多 key 分摊风险"都做不到，反而是**同 IP 的所有 key 共享同一份 200/hr**。
- 单 IP 上限 ≈ **4800 请求/天**，且 `kilo-auto/free` 只在 5 个上游间轮换，实际吞吐很低。
- 建议定位为**兜底/低权重渠道**，别指望扛量。
- 错误处理要按第「四」节那样额外检查 body 里的 `error` 字段。
