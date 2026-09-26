# Kite Router 线：Codex 上下文预算与原生压缩设计

> 作用域：**仅 Kite Delayed 渠道（type 59）中走 Router 线的部分**（`base_url` 以 `/kite-router` 结尾）。
> 其他渠道、其他类型的请求**行为完全不变**。
> 状态：设计定稿，待实现。

---

## 1. 问题

上游 Kite Router 的每个 API key 归属一个独立账号，**每个账号只有 $5.00 一次性额度，无法充值**。
上游在请求前做额度预检，不够直接返回：

```
402 {"detail":{"code":"insufficient_router_balance",
      "message":"insufficient Kite Router allowance",
      "required_microusd":5278695}}
```

实测标定出预检金额的公式（2026-09-26，gpt-6-astra $15/M 入、$75/M 出）：

```
required ≈ prompt_tokens × 2 × 输入价  +  max_tokens × 输出价  +  $0.004
```

- 提示系数是**输入价的 2 倍**（标定：2507→25007 token 使 required 增 $0.675 = 22500 × $30/M）
- 输出预留按 `max_tokens`；**不传时按上游目录的 `max_output_tokens`（多为 8192）预留**
  （标定：8192→1024 使 required 恰好少 $0.5376 = 7168 × $75/M）

**后果**：`required` 随上下文线性增长，一旦超过 $5，**池子里没有任何 key 能服务该请求**，
换 key 完全无效（实测 6 次重试换 6 个 key 全 402）。用户侧表现为"跑一半突然 402"。

当前网关是**随机选 key**，且**把上游那句 402 原样透传**给客户端 —— 用户既看不懂，
也不知道该换模型还是压上下文。

## 2. 目标与非目标

**目标**

1. 单个请求**永远不超过上游 $5 上限**（否则必失败），留出安全余量。
2. 上下文接近上限时，用**协议原生机制**压缩，让长会话能继续，而不是失败。
3. 失败时给出**可执行的明确错误**，绝不静默截断、绝不透传上游原始错误。
4. 不把额度浪费在注定失败的请求上；避免随机选 key 撞上已耗尽 key。

**非目标**

- ❌ 不改动其他渠道、其他 API type 的任何行为。
- ❌ 不做服务端"主动"压缩（HTTP 请求-响应模型下服务端无法主动发起，见 §6.3）。
- ❌ 不静默改写用户对话（任何压缩都必须由客户端显式触发并可见）。
- ❌ 不碰协议转换层已有的 Codex Lite 桥（`relay/channel/kitedelayed/codex_lite.go`）。

## 3. 作用域与隔离

判定沿用现有函数，**所有新逻辑必须挂在这个判定之下**：

```go
// relay/channel/kitedelayed/adaptor.go:92
func isKiteRouterChannel(info *relaycommon.RelayInfo) bool {
    // 实际判定：kiteRouterBaseURL(info.ChannelBaseUrl) 是否成功
    //   = base_url 去掉尾部 "/" 后是否以 "/kite-router" 结尾（不区分大小写）
    //   ⚠️ 只看 base_url 后缀，**不看渠道类型**；当前只有 type 59 的渠道会这么配
}
```

隔离要求（逐条可验证）：

| 要求 | 做法 |
|---|---|
| 非 Router 线（Marathon 线、其他 type） | 不进入任何新分支，代码路径与今天逐字节一致 |
| 非 type 59 渠道 | 同上（注意：判定本身不含 type，见下方备注） |
| 不带 `additional_tools` 的普通请求 | 沿用既有 early-return（`hoistCodexLiteTools` 返回 nil） |
| 预算闸门 | 只在 Router 线 + `/v1/responses`、`/v1/responses/compact` 两个 relay mode 下生效 |

> **备注（待决策）**：`isKiteRouterChannel` 目前仅凭 `base_url` 后缀判定，理论上一个
> 非 type 59 的渠道只要把 base_url 配成 `…/kite-router` 也会命中。若担心误命中，
> 实现时可在**新逻辑内部**额外要求 `info.ChannelType == constant.ChannelTypeKiteDelayed`，
> 而不去改 `isKiteRouterChannel` 本身（改它会波及既有行为）。

## 4. 预算模型（$4.5 安全线 + 上游实时价格）

### 4.1 常量

```
SAFE_BUDGET_USD = 4.50   // 压缩/拒绝的触发线，距 $5 上限留 10% 余量
HARD_CAP_USD    = 5.00   // 上游账号硬上限，仅用于兜底判断
BASE_USD        = 0.004  // 实测的常数项
```

**为什么是 4.5 而不是 5.0**：`required` 是上游的**预检**值，与实收存在偏差（预留是保守的）；
且 token 估算本身有误差。留 10% 余量避免"刚好卡在 5.00 边缘"的抖动导致偶发 402。

### 4.2 公式（全部用上游实时价格）

```
fixed_output_usd(model) = max_output_tokens(model) × output_price(model) / 1e6
required(prompt, model) = prompt × 2 × input_price(model) / 1e6 + fixed_output_usd(model) + BASE_USD
threshold_tokens(model) = (SAFE_BUDGET_USD − fixed_output_usd(model) − BASE_USD) / (2 × input_price(model) / 1e6)
```

- **价格来源**：上游 `GET /v1/catalog`（免费）。进程内缓存（建议 TTL 30min），
  拉取失败时回退到内置兜底表并打 warn 日志，**不因取价失败而拒绝请求**。
- **prompt token 数**：用网关已有的 token 计数（`types.TokenCountMeta`）估算，
  与上游口径的偏差用 §4.1 的余量吸收。

### 4.3 阈值表（2026-09-26 实测价格）

| 上游模型 | 输入 $/M | 输出 $/M | 上下文 | 固定输出预留 | **压缩触发阈值（$4.5）** | $5.0 硬天花板 |
|---|---|---|---|---|---|---|
| `gpt-6-astra` | 15.00 | 75.00 | 1,050,000 | $0.6144 | **129,386** | 146,053 |
| `gpt-5.6-sol` | 6.00 | 30.00 | 1,050,000 | $0.2458 | **354,186** | 395,853 |
| `claude-opus-5` | 7.50 | 37.50 | 1,000,000 | $0.3072 | **279,253** | 312,586 |
| `claude-sonnet-5` | 3.00 | 15.00 | 1,000,000 | $0.1229 | **728,853** | 812,186 |
| `gpt-5.6-luna` | 0.30 | 1.80 | 1,050,000 | $0.0147 | **永不触发**（阈值 > 上下文窗口） | 永不触发 |
| `solar-pro4` | 0.14 | 0.54 | 524,288 | $0.0044 | **永不触发** | 永不触发 |

校验：`threshold(gpt-6-astra) = 129,386` → `required = 129386×30/1e6 + 0.6144 + 0.004 = $4.5000` ✅

> 表里只列当前渠道在售的模型；实现时**不硬编码**，一律按 §4.2 动态算。

## 5. 三块能力

### 5.1 A. 预算闸门（请求前）

在 Router 线的 `/v1/responses` 入口、**转换到 chat 之前**执行：

```
1. 估算 prompt_tokens
2. required ← §4.2 公式（用该请求实际上游模型的价格）
3. 若 required ≤ SAFE_BUDGET_USD          → 放行
4. 若 required > SAFE_BUDGET_USD：
   4a. 若客户端支持压缩（见 §6）→ 由客户端触发 /v1/responses/compact，压缩后重试本轮
   4b. 否则 → 返回 §7 的明确错误（不发给上游）
5. 放行时选 key：只挑余额 ≥ required 的 key（余额来自 /v1/credits 缓存，见 §5.2）
6. 若没有任何 key 余额 ≥ required → 返回 §7 的明确错误（不发上游）
```

**关键**：第 3~4 步必须在**发上游之前**完成 —— 否则就是"把注定失败的请求发出去、白等一次往返"，
这正是当前"跑一半报错"的来源。

### 5.2 余额缓存（配合选 key）

- 数据源：`GET /v1/credits`（免费、快）→ `balance`
- 缓存：`keyIndex → {balance, fetchedAt}`，TTL 建议 5min；请求前按 `required` 过滤候选 key
- 触发刷新：402 返回时**立即**把该 key 余额置为 `required` 以下并降权
- 收益：消除"随机撞上已耗尽 key"的偶发 402（实测池中约 3% 的 key 已耗尽）
- 兜底：缓存不可用时退回随机选 key（即今天的行为），不阻塞请求

### 5.3 B. 上下文压缩桥（协议原生）

见 §6。

### 5.4 C. 阈值下发

服务端**不主动**压缩，但要让客户端知道何时该压：

- 在 `/v1/models` 与渠道说明里暴露每个模型的 `threshold_tokens`（§4.3）
- 在接近阈值时（例如 `required > 0.8 × SAFE_BUDGET`）于响应中附带提示头
  `X-Kite-Context-Budget: used=3.8/4.5 model=gpt-6-astra compact-at=129386`
- 客户端侧推荐配置：`model_catalog_json` 里把 `auto_compact_token_limit` 设成
  §4.3 的阈值（或略小），让 Codex 在超限前自动调用 compact 端点

## 6. 压缩：用协议原生机制，不自定义

### 6.1 协议契约（来自官方源码，非猜测）

**请求** `POST /v1/responses/compact`（new-api 已路由：`router/relay-router.go:104`）

```json
{ "model": "...", "input": [...], "instructions": ..., "previous_response_id": ...,
  "tools": [...], "parallel_tool_calls": ..., "reasoning": {...}, "service_tier": ...,
  "prompt_cache_key": ..., "text": ... }
```

**响应**（`dto.OpenAIResponsesCompactionResponse`，handler 为**纯透传**）

```json
{ "id": "...", "object": "...", "created_at": 0,
  "output": [ { "type": "compaction", "id": "cmp_...", "encrypted_content": "<不透明串>" } ],
  "usage": { ... } }
```

**客户端语义**：Codex 取该 item 后 `retained.push(compaction_output)`
（`codex-rs/core/src/compact_remote_v2.rs:529`）——**用它替换整个历史**，
之后每轮只带这个块。触发类型 `CompactionTrigger::Auto`（由模型目录的
`auto_compact_token_limit` 控制）或 `Manual`。
item 定义：`codex-rs/protocol/src/models.rs:1228` `ResponseItem::Compaction { id, encrypted_content }`
（`#[serde(alias = "compaction_summary")]`）。

### 6.2 服务端实现（Router 线专属）

上游**没有** `/v1/responses/compact`（OpenAPI 无此路径），所以压缩由我们实现：

```
1. 收 compact 请求（input = 完整历史）
2. 把 input 转成 chat messages（复用既有 responses→chat 转换）
3. 前置一段摘要指令（system）：把历史压缩成结构化摘要
   —— 保留：用户目标、已完成的改动、关键决策、未决问题、文件路径、下一步
   —— 丢弃：冗余的工具输出、重复内容
4. 用【压缩专用模型】调上游 /v1/chat/completions
5. 把摘要包成 compaction item 返回：
   {"type":"compaction","id":"cmp_<随机>","encrypted_content":"<§6.4 编码>"}
   usage 按上游真实 usage 回填
```

**压缩专用模型**：固定选该渠道中"阈值永不触发"里**最便宜**的那个（当前是 `gpt-5.6-luna`）。
理由见 §6.3。可通过渠道配置覆盖，但**不允许**配成会触发阈值的模型。

### 6.3 为什么压缩必须用便宜模型（硬约束）

压缩请求本身要把**完整历史**发给上游 —— 用错模型会死锁：

| 压缩 155k 历史用的模型 | 该请求 required | 结果 |
|---|---|---|
| `gpt-5.6-luna`（$0.30/M 入） | **$0.112** | ✅ 通过 |
| `gpt-6-astra`（$15/M 入） | **$5.27** | ❌ 压缩调用自己就超限 —— 要压的正是那个超限的上下文 |

**总账**（155k 会话在 astra 上本来必死）：

```
luna 压缩 155k 历史          $0.112
astra 续跑压缩后的 ~5k 摘要   $0.764
────────────────────────────────────
合计                         $0.876     （原本：无解）
```

**即：把 astra 的 129k（$4.5 线）天花板真正绕过去了。**

### 6.4 `encrypted_content` 编码（我们的私有格式）

客户端把它当**不透明串**原样回传，因此内容由我们定义，**网关无需存储任何会话状态**：

```
kr1:<base64url(JSON)>
JSON = { "v":1, "model":"gpt-6-astra", "at":<unix>, "tokens":<摘要后估算 token>,
         "summary":"<摘要正文>" }
```

- 前缀 `kr1:` 为版本号，便于将来演进；不认识的前缀按"无法解码"处理（§6.5）
- 下一轮请求里，转换层遇到 `input` 中的 `{"type":"compaction"}` 项时：
  解码 → 取出 `summary` → 作为一条 `system`/`developer` 消息插到消息序列最前
- 解码失败（非本网关签发 / 版本不认识）→ **按明确错误返回**，不猜、不丢弃

### 6.5 压缩救不了的情况

**单条输入本身就超阈值**（例如一条 200k token 的粘贴），压缩请求自己会被上游拒。
此时**不尝试压缩**（省一次往返），直接返回 §7 的错误。
⇒ 所以 §5.1 的顺序是"**先估预算，再决定是否压缩**"，而不是"先压缩再看行不行"。

## 7. 明确错误（兜底，绝不静默）

统一用 `types.NewErrorWithStatusCode`，HTTP 400，并带上可执行信息：

```
当前请求需要上游额度约 $5.27，本渠道单账号上限 $5.00。
模型 gpt-6-astra 在 $4.50 安全线下的最大上下文为 129,386 token，当前约 155,000 token。
请任选其一：① 压缩上下文（Codex 会自动调用 /v1/responses/compact）；
② 改用 gpt-5.6-luna（同上下文约 $0.11）；③ 新开会话。
```

字段建议：`code: context_budget_exceeded`，并在 `metadata` 里带
`required_usd / safe_budget_usd / threshold_tokens / prompt_tokens / model`，
方便客户端做自动化处理。

**绝不做**：截断 prompt、丢历史消息、静默降级到别的模型、把上游 402 原样透传。

## 8. 验证方案

**单元测试**（`relay/channel/kitedelayed/`）
- 阈值计算：给定价格表，断言 §4.3 的六个值
- `encrypted_content` 编解码往返；坏前缀 / 坏 base64 / 版本不识别 → 明确错误
- **作用域隔离回归**：非 Router 线、非 type 59、无 `additional_tools` 的请求，
  请求体与响应**逐字节不变**（沿用现有 early-return 断言风格）
- 预算闸门：required 恰好 = 4.5 / 4.49 / 4.51 的边界
- 余额过滤：所有 key 余额 < required → 返回 §7 错误且**不产生上游请求**（用假上游计数）

**本地端到端**（`yutou-api-local-channel-verify` 配方 + 真实 `codex exec`）
- 构造一个超阈值会话 → 观察 Codex 是否调用 `/v1/responses/compact` → 是否成功续跑
- 用假上游抓包确认：压缩请求发给了 luna、续跑请求带的是 compaction item

**生产**
- 部署后打真实请求：非流式 / 流式 / compact 三条路径
- 核对日志：`request_path=/v1/responses/compact` 出现、`use_channel` 正确、
  **无 panic**（参考 `responses_handler.go` 的 `*dto.Usage` 断言教训）

## 9. 实施顺序

| 阶段 | 内容 | 风险 |
|---|---|---|
| ① | §7 明确错误（含 §4.2 公式与阈值计算） | 低 —— 纯增量、无上游行为变化 |
| ② | §5.2 余额缓存 + 按 required 选 key | 中 —— 改选 key 逻辑，需回退开关 |
| ③ | §6 压缩桥（compact 端点 + `encrypted_content` 编解码） | 中高 —— 新端点、新协议路径 |
| ④ | §5.4 阈值下发 + 文档 | 低 |

每阶段独立可发布、独立可回退。① 先上能把"神秘 402"立刻变成可读错误。

## 10. 待定问题

1. **压缩调用的计费归属**：压缩消耗上游额度（155k 历史约 $0.11），由谁承担？
   建议按上游真实 usage 计费，并向用户明示这是压缩产生的额外消耗。
2. **摘要质量**：`luna` 压缩 155k 上下文的质量需实测（可用真实 Codex 会话对比压缩前后任务完成度）。
3. **Codex 的触发条件**：`/v1/responses/compact` 目前**零调用**（近 7 天，全站 24h 分布
   chat 18754 / responses 357 / messages 12）。需确认 Codex 在什么条件下才会走远程压缩
   （是否要求服务端/模型元数据声明支持），否则做完没人调。**这是 ③ 的前置调研。**
4. **安全线是否可配**：`SAFE_BUDGET_USD` 建议做成渠道级配置（默认 4.5），
   以便上游价格或额度策略变化时无需发版。
