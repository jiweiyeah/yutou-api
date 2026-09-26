# Kite Router 线：Codex 上下文预算与原生压缩设计

> 作用域：**仅 Kite Delayed 渠道（type 59）中走 Router 线的部分**（`base_url` 以 `/kite-router` 结尾）。  
> 其他渠道、其他类型的请求**行为完全不变**。  
> 状态：**①②③④ 均已实现、已部署、已通过生产验证**（见 §8 / §11）。
> 线上 commit `04b850a87`，镜像 revision 已核对，容器 healthy。
> 实现过程中对上游计费公式与 Codex 压缩协议做了两处 **实测修正**，
> §1 / §4 / §6.1 已按实测更新，原文的错误假设保留在 §11.3 作为记录。



---

## 1. 问题

上游 Kite Router 的每个 API key 归属一个独立账号，**每个账号只有 $5.00 一次性额度，无法充值**。  
上游在请求前做额度预检，不够直接返回：

```
402 {"detail":{"code":"insufficient_router_balance",
      "message":"insufficient Kite Router allowance",
      "required_microusd":5278695}}
```

实测标定出预检金额的公式（2026-09-26，38 个数据点，误差 < 0.4%）：

```
required ≈ 请求体字节数 × 输入价($/M) / 1e6  +  max_tokens × 输出价($/M) / 1e6  +  $0.004
```

> ⚠️ **两处修正（都是实测推翻了原设计）**：
>
> **① 提示项按「字节」计价，不是 token。** 同一 30k token 的提示，随机串（53,838 字节）
> required = $0.8887、英文散文（155,454 字节）= $2.4129、中文（40,660 字符 / 121,980 字节）
> = $1.9108 —— 三条**精确落在「字节 × 输入价」**上（把常数与输出预留扣掉后，系数是
> $15.000/M 字节，误差 < 0.05%），按 token 则完全对不上。
> 原设计写的「token × 输入价的 2 倍」之所以看着成立，是因为当初的标定文本恰好约
> **2 字节/token**：`tokens × 2 × 价` ≡ `bytes × 价`。换个文本密度就崩。
>
> **② 没有 ×2 这个系数**，见上。

- 输出预留按 `max_tokens`；**不传时按上游目录的 `max_output_tokens`（当前全为 8192）预留**  
  （标定：1024→2048→8192 三档差额恰好等于 `Δmax_tokens × 输出价`：$0.0768 / $0.5376）
- 常数项 $0.004 已用 2 KB 的极小请求验证（实测 $0.6487 vs 预测 $0.6484）

**后果**：`required` 随上下文**字节数**线性增长，一旦超过 $5，**池子里没有任何 key 能服务该请求**，  
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

| 要求                             | 做法                                                                      |
| ------------------------------ | ----------------------------------------------------------------------- |
| 非 Router 线（Marathon 线、其他 type） | 不进入任何新分支，代码路径与今天逐字节一致                                                   |
| 非 type 59 渠道                   | 同上（注意：判定本身不含 type，见下方备注）                                                |
| 不带 `additional_tools` 的普通请求    | 沿用既有 early-return（`hoistCodexLiteTools` 返回 nil）                         |
| 预算闸门                           | 只在 Router 线 + `/v1/responses`、`/v1/responses/compact` 两个 relay mode 下生效 |

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
reserve_tokens(model, max_tokens) = min(max_tokens, max_output_tokens(model))   // max_tokens 缺省时用目录值
required(prompt_bytes, model, max_tokens)
        = prompt_bytes × input_price(model) / 1e6
        + reserve_tokens(model, max_tokens) × output_price(model) / 1e6
        + BASE_USD
threshold_bytes(model) = (SAFE_BUDGET_USD − max_output_tokens(model) × output_price(model) / 1e6 − BASE_USD)
                       / (input_price(model) / 1e6)
```

- **价格来源**：上游 `GET /v1/catalog`（**免鉴权**，实测带无效 key 也回 200）。  
  进程内按 router base URL 缓存（TTL 30min），拉取失败时先退回上次成功的快照、再退回内置兜底表  
  （8 个已知模型）并打 warn 日志，**不因取价失败而拒绝请求**（拿不到价的模型直接跳过闸门）。
- **prompt 字节数**：取**实际会发往上游的请求体长度**（转换后的 chat 请求 marshal 后的 `len`）。
  后续 `RemoveDisabledFields` / `param_override` 只会让请求体变小，所以这是上界（保守）。
  在转换之后、发上游之前测量，比原设计「转换之前按 token 估」更准。
- **token 数只用于给人看的提示**（错误文案、响应头），按 4 字节/token 换算 —— 这个方向偏保守
  （真实 token 数不会小于 `字节/4`），不会误导用户把阈值设得过大。

### 4.3 阈值表（2026-09-26 实测价格，按**字节**）

| 上游模型              | 输入 $/M | 输出 $/M | 上下文(tokens) | 固定输出预留  | **$4.5 触发线（字节）** | $5.0 硬天花板（字节） |
| ----------------- | ------ | ------ | ---------- | ------- | --------------- | ------------- |
| `gpt-6-astra`     | 15.00  | 75.00  | 1,050,000  | $0.6144 | **258,773**     | 292,107       |
| `gpt-5.6-sol`     | 6.00   | 30.00  | 1,050,000  | $0.2458 | **708,373**     | 792,627       |
| `claude-opus-5`   | 7.50   | 37.50  | 1,000,000  | $0.3072 | **558,506**     | 625,707       |
| `claude-sonnet-5` | 3.00   | 15.00  | 1,000,000  | $0.1229 | **1,457,706**   | 1,625,707     |
| `gpt-5.6-luna`    | 0.30   | 1.80   | 1,050,000  | $0.0147 | **14,937,514**  | 16,603,067    |
| `solar-pro4`      | 0.135  | 0.54   | 524,288    | $0.0044 | **33,270,935**  | 36,983,263    |

校验：`threshold_bytes(gpt-6-astra) = 258,773` → `required = 258773×15/1e6 + 0.6144 + 0.004 = $4.5000` ✅

> 表里只列当前渠道在售的模型；实现**不硬编码**，一律按 §4.2 从 `/v1/catalog` 动态算，
> 兜底表只在上游目录不可用时生效。
>
> **换算直觉**：`astra` 的 258,773 字节，对英文散文（≈4 字节/token）约 6.5 万 token，
> 对代码/JSON（≈2 字节/token）约 13 万 token。**同一份"token 预算"在不同内容密度下差 2 倍** ——
> 这就是为什么必须按字节管，而不能按 token 管。

## 5. 三块能力

### 5.1 A. 预算闸门（请求前）

在 Router 线的 `/v1/responses` 入口、**转换到 chat 之后、发上游之前**执行
（原设计写「转换到 chat 之前」，实现时改到之后：只有拿到真实的出站请求体才能量字节数，
效果相同 —— 都在发上游之前，都不会白跑一次往返）：

```
1. 量出站请求体字节数（转换后的 chat 请求 marshal 长度）
2. required ← §4.2 公式（用该请求实际上游模型的价格）
3. 若 required ≤ SAFE_BUDGET_USD          → 放行，并把估算结果挂到 context 供选 key 复用
4. 若 required > SAFE_BUDGET_USD：
   4a. 客户端支持压缩（见 §6）→ 客户端会在下一轮自己发压缩请求（v2：input 带 compaction_trigger）
   4b. 否则 → 返回 §7 的明确错误（不发给上游）
5. 放行时选 key：只挑余额 ≥ required 的 key（余额来自 /v1/credits 缓存，见 §5.2）
6. 若连续探测的候选 key 余额都不够 → 返回 §7 的明确错误（不发上游）
```

**关键**：第 3~4 步必须在**发上游之前**完成 —— 否则就是"把注定失败的请求发出去、白等一次往返"，  
这正是当前"跑一半报错"的来源。

> **实现注记（与原设计的两点差异）**：
> 1. 闸门挂在 `ConvertOpenAIResponsesRequest` 里，覆盖 `/v1/responses`（含 v2 压缩）与
>    `/v1/responses/compact` 两个 relay mode。`/v1/chat/completions` **不在范围内**（按 §3 的作用域约定）。
> 2. 第 6 步的判定是「连续探测 8 个候选 key 都不够」而不是「池子里没有任何 key 够」——
>    池子有上万 key，不可能穷举。探测全部**成功返回余额**且都不够时才拒绝；任何一次取余额
>    失败都退回 middleware 已选定的 key（即今天的行为）。理由：8 个随机 key 同时不够的概率
>    在池子健康时 < 1e-10，在池子半废时才是真实信号。

### 5.2 余额缓存（配合选 key）

- 数据源：`GET /v1/credits`（免费）→ `balance`（字符串，形如 `"0.640369"`）
- 缓存：`channelId → keyIndex → {balance, fetchedAt}`，TTL **5min**（进程内，带容量上限防泄漏）
- 选 key 顺序：先确认 middleware 已选定的 key 够不够（够用就不动，省一次重选）；
  不够才用 `GetNextEnabledKey()` 抽候选，最多探测 **8** 个、总耗时上限 **2s**、单次取余额超时 **1.5s**
- **只在 `required > $0.50` 时才做**（可用 `KITE_ROUTER_BALANCE_FILTER_FLOOR_USD` 调）：
  小请求几乎所有 key 都撑得住，不值得为它付一次 credits 往返
- 触发刷新：402 返回时**立即**把该 key 记为 0 余额（本轮及之后 5min 内不再选它，到期自动重探）
- 收益：消除"随机撞上已耗尽 key"的偶发 402（实测池中约 3% 的 key 已耗尽）
- 兜底：缓存不可用、池子不是多 key、超时、渠道设置 `kite_router_balance_filter=false` →
  退回随机选 key（即今天的行为），**不阻塞也不拒绝请求**

### 5.3 B. 上下文压缩桥（协议原生）

见 §6。

### 5.4 C. 阈值下发

服务端**不主动**压缩，但要让客户端知道何时该压：

- **已实现**：接近安全线（`required > 0.8 × SAFE_BUDGET`）时在响应里带提示头  
  `X-Kite-Context-Budget: used=3.72/4.50 model=openai/gpt-6-astra prompt_bytes=300000 threshold_bytes=258773 threshold_tokens=64693 max_tokens=8192`
- **未实现（有意）**：原设计要在 `/v1/models` 暴露 `threshold_tokens`。**没有做**，原因：
  `/v1/models` 是全站共享的用户面接口，往里塞 Kite 专属字段既泄露渠道内部信息，
  也违反 §3「其他渠道行为完全不变」的隔离承诺；而 Codex 根本不读这个接口的元数据
  （它用自己的内置 catalog / `model_catalog_json`）。
- **客户端侧才是真正有效的那一环**（见 §11.2）：
  `config.toml` 里设 `model_auto_compact_token_limit`（或 `model_catalog_json` 的
  `auto_compact_token_limit`），让 Codex 在撞上 $4.5 线之前就自动压缩。

## 6. 压缩：用协议原生机制，不自定义

### 6.1 协议契约（**已核对 codex rust-v0.157.1 源码**）

> 🔴 **重大修正**：原设计假设客户端会调 `POST /v1/responses/compact`。**错的。**
> 在 codex 0.157.1 的源码包里 `grep -rn "responses/compact"` **零命中** ——
> 该端点已不在协议里。Codex 的远程压缩走的是 **v2 协议**：一次**普通的
> `POST /v1/responses`**，只是 input 末尾多一条 `{"type":"compaction_trigger"}`。

**请求**（v2，Codex 实际会发的）

```
POST /v1/responses
{ "model": "...",
  "input": [ ...完整历史..., {"type":"compaction_trigger"} ],
  "instructions": "...", "tools": [...], "stream": true, ... }
```

**响应**（v2）：一个普通的 Responses **事件流**，只需两件事
（`codex-rs/core/src/compact_remote_v2.rs` 的 `collect_compaction_output` 只认这两条）：

```
event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"compaction","id":"cmp_...","encrypted_content":"<不透明串>"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_...","usage":{...}}}
```

**请求**（v1，保留支持）：`POST /v1/responses/compact`（new-api 已路由 `router/relay-router.go:104`），  
响应是 JSON 信封 `{"id":..,"object":"response.compaction","output":[<compaction item>],"usage":{..}}`。
**当前 Codex 不会走这条**，实现它是为了兼容其他客户端 + 保持 new-api 既有路由可用。

**客户端语义**：Codex 取到 compaction item 后把它 push 进保留历史
（`build_v2_compacted_history`）——**用它替换整个历史**，之后每轮只带这个块。
item 定义：`codex-rs/protocol/src/models.rs:1227` `ResponseItem::Compaction { id, encrypted_content }`  
（`#[serde(alias = "compaction_summary")]`）；`compaction_trigger` 的注释写得很直白：  
「Compaction triggers are request controls, not durable response items」（`models.rs:1240`）。

**🔴 触发条件（原设计 §10.3 的前置调研，结论如下 —— 已实测）**：

```rust
// codex-rs/model-provider/src/provider.rs:410
let remote_compaction = if self.info.is_openai()                                  // name == "openai"
    || is_azure_responses_provider(&self.info.name, self.info.base_url.as_deref())
{ RemoteCompactionSupport::V2 } else { RemoteCompactionSupport::Unsupported };
```

判定**只看 provider 名字**或 base_url 的 Azure 特征串。实测三种配法的结果：

| 客户端配法                                       | 结果                                                                                                        |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| `model_providers.custom` + 我们的 base_url     | `Unsupported` → **本地压缩**（客户端自己摘要），**永远不会发远程压缩请求**                                                           |
| `model_providers.openai` + 我们的 base_url     | ❌ **配置直接报错**：`model_providers contains reserved built-in provider IDs: 'openai'. Built-in providers cannot be overridden.` —— 这条路被 Codex 主动堵死 |
| `model_providers.azure`（`name="azure"`）+ 我们的 base_url | ✅ `RemoteCompactionSupport::V2` → **真的会发 v2 压缩请求**（`azure` 不在保留 id 列表里，只有 openai / bedrock / ollama-oss / lmstudio-oss 被保留） |

⇒ **结论：自定义 provider 用户默认走本地压缩；只有把 provider 名配成 `azure` 才会走我们的远程压缩桥。**
这不改变 §5.1 闸门的价值（它对所有用户都生效，把神秘 402 变成可读错误），但意味着
**§11.2 的客户端阈值配置才是普适解法**，远程压缩桥是给"愿意配 azure 名"的用户准备的加速档。

**实测证据（2026-09-26，真实 codex 0.157.1 + 真实上游）**：两轮会话（`codex exec` + `codex exec resume --last`）
配 `-c model_provider="azure" -c model_auto_compact_token_limit=1`，本地网关日志：

```
8  19:32:30  client=openai/gpt-5.6-luna  upstream=gpt-5.6-luna  pt=10193  ← 第 1 轮
9  19:32:36  client=openai/gpt-5.6-luna  upstream=solar-pro4     pt=7186   ← v2 压缩调用（走便宜模型）
10 19:32:37  client=openai/gpt-5.6-luna  upstream=gpt-5.6-luna  pt=10270  ← 带着 compaction item 续跑
```

上游自己的账本（`GET /v1/credits`）同步出现 `Kite Router usage: solar-pro4 -$0.001013` ——
**压缩调用确实打到了 solar-pro4**，之后 Codex 正常输出并退出码 0。
（抓包另证：Codex 发出的压缩请求是 `POST /v1/responses`，body 46,644 字节，内含 `"type":"compaction_trigger"`。）

### 6.2 服务端实现（Router 线专属）

上游**两条都没有**（`GET /openapi.json` 里只有 `/v1/chat/completions` 与 `/v1/delayed/*`、
`/v1/marathon/*`），所以压缩由我们实现。v1/v2 共用同一条流水线，只是最后的响应形态不同：

```
1. 收压缩请求（input = 完整历史）
   - v2：/v1/responses，摘掉 input 末尾的 {"type":"compaction_trigger"}
   - v1：/v1/responses/compact（清掉 previous_response_id，转换层不接受 stateful 字段）
2. 把 input 转成 chat messages（复用既有 responses→chat 转换；
   转换前先跑 codex lite 归一化，否则 custom_tool_call_output 会变成空消息）
3. 前置一段 system 摘要指令：把历史压缩成结构化摘要
   —— 保留：用户目标、已完成的改动与文件路径、关键决策、未决问题、下一步
   —— 丢弃：冗余的工具输出、重复内容、已解决的死路、寒暄
4. 用【压缩专用模型】调上游 /v1/chat/completions（stream=false，max_tokens=4096，不带工具）
5. 把摘要包成 compaction item：
   v2 → 事件流：response.output_item.done(item) + response.completed
   v1 → JSON 信封：{"object":"response.compaction","output":[item],"usage":{...}}
   encrypted_content 用 §6.4 编码；usage 按上游真实 usage 回填
```

**压缩专用模型**：在该渠道 `models` 里挑「**阈值永不触发**」（`threshold_bytes ≥ 上下文 × 4`）
的一档中最便宜的；若一个都没有，退化为「最便宜的那个」而不是报错。
判定用输入价排序（压缩成本几乎全在输入侧）。当前 10867 在售模型里会选中 `upstage/solar-pro4`
（$0.135/M 入，比 luna 还便宜一半）。

### 6.3 为什么压缩必须用便宜模型（硬约束）

压缩请求本身要把**完整历史**发给上游 —— 用错模型会死锁：

| 压缩 300 KB 历史用的模型          | 该请求 required | 结果                           |
| ------------------------- | ------------ | ---------------------------- |
| `solar-pro4`（$0.135/M 入） | **$0.048**   | ✅ 通过                         |
| `gpt-5.6-luna`（$0.30/M 入） | **$0.098**   | ✅ 通过                         |
| `gpt-6-astra`（$15/M 入）    | **$5.12**    | ❌ 压缩调用自己就超限 —— 要压的正是那个超限的上下文 |

**总账**（300 KB 会话在 astra 上本来必死）：

```
solar-pro4 压缩 300KB 历史      $0.048
astra 续跑压缩后的 ~5KB 摘要     $0.08
────────────────────────────────────
合计                            $0.13      （原本：无解）
```

**即：把 astra 的 258,773 字节（$4.5 线）天花板真正绕过去了。**

### 6.4 `encrypted_content` 编码（我们的私有格式）

客户端把它当**不透明串**原样回传，因此内容由我们定义，**网关无需存储任何会话状态**：

```
kr1:<base64url(JSON)>
JSON = { "v":1, "model":"gpt-6-astra", "at":<unix>, "tokens":<摘要后估算 token>,
         "summary":"<摘要正文>" }
```

- 前缀 `kr1:` 为版本号，便于将来演进；不认识的前缀按"无法解码"处理（§6.5）
- 下一轮请求里，转换层遇到 `input` 中的 `{"type":"compaction"}` 项时：  
  解码 → 取出 `summary` → **就地替换成一条 `role:"developer"` 消息**（前缀一句说明
  "Conversation history compressed by the API gateway…"，让模型知道这是压缩过的历史，
  不是用户新说的话）。用 `developer` 而不是 `system`：上游实测两者都收，
  但 `system` 槽位要留给 `instructions`。
- 解码失败（非本网关签发 / 版本不认识 / 空摘要）→ **按明确错误返回**（`code: compaction_block_invalid`），
  不猜、不丢弃

### 6.5 压缩救不了的情况

**单条输入本身就超阈值**（例如一条 200k token 的粘贴），压缩请求自己会被上游拒。  
此时**不尝试压缩**（省一次往返），直接返回 §7 的错误。  
⇒ 所以 §5.1 的顺序是"**先估预算，再决定是否压缩**"，而不是"先压缩再看行不行"。

## 7. 明确错误（兜底，绝不静默）

统一用 `types.NewErrorWithStatusCode` + `ErrOptionWithMetadata`，HTTP 400，并带上可执行信息。
**实际实现输出的文案**（`relay/channel/kitedelayed/router_budget.go`）：

```
模型 openai/gpt-6-astra 在 $4.50 安全线下的最大上下文约 258773 字节（约 64693 token），
当前请求约 300000 字节（约 0 token）。请任选其一：① 压缩上下文（Codex 在接近上限时会自行压缩，
也可手动触发）；② 改用 upstage/solar-pro4（同上下文约 $0.04）；③ 新开会话；
```

`metadata`（客户端可编程处理）：

```json
{"code":"context_budget_exceeded","model":"openai/gpt-6-astra","required_usd":5.118,
 "safe_budget_usd":4.5,"threshold_bytes":258773,"threshold_tokens":64693,
 "prompt_bytes":300000,"prompt_tokens":155000,"max_tokens":8192}
```

另一条同类错误：池子余额不足时 `code: insufficient_router_balance`，metadata 带
`required_usd / channel_id / multi_key_index`。

**绝不做**：截断 prompt、丢历史消息、静默降级到别的模型、把上游 402 原样透传。

> **实现注记**：错误类型用 `NewErrorWithStatusCode`，它的 `ToOpenAIError()` 原本**不携带**
> `Metadata`（只有 OpenRouter 风格的 `WithOpenAIError` 路径会带）。为此给
> `types/error.go` 加了 `ErrOptionWithMetadata` 选项，并让默认分支把 `e.Metadata` 透出去 ——
> 这是本次唯一的跨包改动（纯增量，无行为变化）。

## 8. 验证方案与结果

**单元测试**（`relay/channel/kitedelayed/router_budget_test.go`，22 个用例全绿）

- ✅ 阈值计算：断言 §4.3 六个模型的**字节**阈值，并回代校验 `required == 4.5000`
- ✅ 公式回归：6 个实测点（astra/sol/kimi 不同 max_tokens 与不同字节数）误差 < 1%
- ✅ `encrypted_content` 编解码往返；坏前缀 / 坏 base64 / 非 JSON / 版本不识别 / 空摘要 → 明确错误
- ✅ compaction item ↔ developer 消息往返；无压缩块时**逐字节不变**
- ✅ **作用域隔离回归**：Marathon 线转换结果与原生 openai 适配器**逐字节相同**，
  且**零** catalog/credits 旁路请求
- ✅ 预算闸门：4.4999 / 4.5000 / 4.5001 边界；超限时**不产生任何上游 chat 请求**（假上游计数为 0）
- ✅ 取价失败（模型不在目录）→ 放行，不拒绝
- ✅ 余额过滤：全池不足 → `NoneAffordable` 且不产生 chat 请求；有够用 key → 选中它；
  当前 key 够用 → 不换 key；小请求 → 零 credits 往返
- ✅ 上游 402 `insufficient_router_balance` → 可读错误 + 该 key 被记为耗尽；
  其他 402 → 不动余额缓存
- ✅ v2 端到端：`compaction_trigger` → 便宜模型 → compaction item → 下一轮解码回消息

**本地端到端**（`yutou-api-local-channel-verify` 配方 + 真实 `codex exec` + 真实上游）—— ✅ 已跑通：

- ✅ 直接打 v2：`/v1/responses` + `compaction_trigger` → 上游 `solar-pro4` 产出摘要 →
  回出 compaction item（`kr1:` 可解码出中文摘要）
- ✅ 回传续跑：把 compaction item 放回 input → 模型准确复述摘要内容（证明摘要真的进了上下文）
- ✅ 真实 Codex：`codex exec` + `codex exec resume --last`，压缩请求落 `solar-pro4`、
  上游账本对得上、续跑成功、退出码 0（证据见 §6.1 末）
- ✅ 抓包确认：Codex 发的是 `POST /v1/responses` + `"type":"compaction_trigger"`（46,644 字节）

**生产**（2026-09-26 已执行，commit `04b850a87`，镜像 revision 与容器均核对过）

验证手法：新建**临时渠道 `10871`**（type 59，只声明机队无人服务的模型名 `stealth/ox-alpha`
→ 任何真实用户都请求不到）+ 临时令牌，验完把渠道与令牌都删干净。
详见 skill `yutou-api-prod-channel-ops` 的「用临时渠道做零风险生产验证」。

- ✅ **预算闸门**：300 KB 请求 → `HTTP 400` + 响应头
  `X-Kite-Context-Budget: used=5.1194/4.50 model=gpt-6-astra prompt_bytes=300065 threshold_bytes=258773 threshold_tokens=64693 max_tokens=8192`
  + `code=context_budget_exceeded` + 完整 `metadata`。
  带 `compaction_trigger` 的 300 KB 请求同样被拒（`used=4.8104`，压缩也压不进 4.5 线 ⇒ §6.5 生效）。
  **日志侧证据**：两条 `type=5`、`upstream_model_name` 为空 ⇒ 确实在发上游前拒掉，**零成本**。
- ✅ **v2 压缩**：`POST /v1/responses` + `compaction_trigger` → `200 text/event-stream`，
  `response.output_item.done` 带 compaction item，`kr1:` 可解出
  `{"v":1,"model":"solar-pro4",...,"summary":"已 python3 /tmp/parser.py，stdout 全 PASS。"}`
- ✅ **回传续跑**：把 compaction item 放回 input + 新提问 → 模型答「跑完了，全部通过。」
- ✅ **日志如实**：压缩调用那行 `upstream_model_name=solar-pro4`（日志字段修复生效）
- ✅ **无 panic**（`docker logs new-api --since 12m | grep -ci panic` = 0）
- ✅ **未影响生产**：`10867` 全程未动、仍 `status=2`；临时渠道/令牌/abilities 行已全部清理
- 💰 整个过程**上游花费 $0.000072**（闸门拦下的两条 300 KB 请求零成本）

## 9. 实施顺序与状态

| 阶段 | 内容                                       | 状态                             |
| -- | ---------------------------------------- | ------------------------------ |
| ①  | §7 明确错误（含 §4.2 公式与阈值计算）                  | ✅ 完成（公式按实测修正为字节制）              |
| ②  | §5.2 余额缓存 + 按 required 选 key             | ✅ 完成（带门槛 + 总时间预算 + 全链路兜底）      |
| ③  | §6 压缩桥（v2 `compaction_trigger` + v1 端点）   | ✅ 完成（协议已按 0.157.1 源码核对并纠正）     |
| ④  | §5.4 阈值下发 + 文档                           | ✅ 完成（响应头；`/v1/models` 有意不做，见 §5.4） |

每阶段独立可发布、独立可回退。① 先上能把"神秘 402"立刻变成可读错误。

**涉及文件**（除 `docs/` 外）：

| 文件                                                  | 内容                                  |
| --------------------------------------------------- | ----------------------------------- |
| `relay/channel/kitedelayed/router_budget.go`（新）      | 价格目录缓存、预算模型、闸门、明确错误、响应头             |
| `relay/channel/kitedelayed/router_balance.go`（新）     | `/v1/credits` 余额缓存、按 required 选 key、402 翻译 |
| `relay/channel/kitedelayed/router_compact.go`（新）     | v1/v2 压缩桥、`encrypted_content` 编解码   |
| `relay/channel/kitedelayed/router_budget_test.go`（新） | 22 个用例                               |
| `relay/channel/kitedelayed/adaptor.go`              | 三处挂钩（转换/DoRequest/DoResponse）         |
| `relay/responses_handler.go`                        | compact 端点放行 `APITypeKiteDelayed`    |
| `types/error.go`                                    | 新增 `ErrOptionWithMetadata` + 透出 metadata |
| `dto/channel_settings.go`                           | 渠道级 `kite_router_safe_budget_usd` / `kite_router_balance_filter` |

## 10. 待定问题（已结清）

1. **压缩调用的计费归属** → 压缩走**用户请求的模型名**计费（`ResponsesHelper` 的既有口径），
   即用户按自己选的那个模型的价格付费。压缩本身的上游成本（$0.05 量级）由池子承担。
   渠道定价未变。
2. **摘要质量** → 摘要指令要求「目标 / 已做（含文件路径与命令）/ 决策与理由 / 未决 / 下一步」，
   丢弃冗余工具输出。**未做真实长会话的质量对比实验**（本地 key 余额不足以跑 300KB 会话），
   列为遗留项。
3. **Codex 的触发条件** → ✅ 已查清并实测，见 §6.1。结论：**provider 名为 `azure` 才会走我们的
   远程压缩**（`openai` 被 Codex 以保留 id 拒绝，自定义名走本地压缩）。因此本功能的受众
   比原设计假设的小，真正的普适解法是 §11.2 的客户端阈值配置。
4. **安全线是否可配** → ✅ 已实现。渠道 `setting` 里加 `kite_router_safe_budget_usd`（默认 4.5，
   超出 `(0, 5.0]` 自动回退默认并打 warn），另有环境变量 `KITE_ROUTER_SAFE_BUDGET_USD`。

## 11. 实现现状、运维前提与遗留

### 11.1 运维前提（**部署前必须做**）

1. **v1 compact 端点需要渠道声明模型 + 定价**：`/v1/responses/compact` 会走
   `middleware/distributor.go:413` 的 `WithCompactModelSuffix`，把选渠道用的模型名改成
   `<model>-openai-compact`。所以要么在渠道 `models` 里加 `openai/gpt-6-astra-openai-compact`，
   要么在 `ModelRatio` 里配通配键 `*-openai-compact`；否则渠道选不到 / 报 `model_price_error`。
   **v2 路径（Codex 实际会走的）不需要这些**。
2. **渠道 `models` 必须包含一个便宜的「永不触发」模型**，否则压缩会退化到最便宜的那个，
   甚至报 `compaction_model_unavailable`。当前 10867 在售的 `upstage/solar-pro4` 满足。
3. 渠道 10867 目前 `status=2`（已禁用）。本改动**不改任何渠道配置**，启用与否则由运营决定。

### 11.2 客户端侧（对多数用户最有效的一环）

自定义 provider 下 Codex 走**本地压缩**，触发线由 `model_auto_compact_token_limit` 决定
（`config.toml` 顶层键，也可 `-c` 覆盖；或 `model_catalog_json` 里的同名元数据字段）。
**没配时 Codex 用 fallback 元数据**，其阈值与我们的字节制上限并不对齐 —— 这正是"跑一半突然 402"。

推荐配置（按 §4.3 的 `threshold_tokens` 取略小值）：

```toml
# ~/.codex/config.toml
model = "openai/gpt-6-astra"
model_auto_compact_token_limit = 60000   # astra 的安全线约 6.5 万 token（散文口径）
```

**想要走网关侧压缩桥（v2）的用户**，把 provider 名配成 `azure`（`azure` 不在 Codex 的保留 id 列表里，
而 `openai` 会被拒绝）：

```toml
model_provider = "azure"
model_auto_compact_token_limit = 60000

[model_providers.azure]
name = "azure"          # ← 关键：Codex 只看这个名字来判断 remote_compaction 能力
base_url = "https://<我们的网关>/v1"
wire_api = "responses"
env_key = "YUTOU_API_KEY"
```

**⚠️ 一个容易踩的排查坑**：压缩调用把 body 里的 `model` 换成便宜模型，但
`info.UpstreamModelName` 是日志字段（不参与计费）—— 实现里已同步改写它，
所以日志能看到 `upstream_model_name=solar-pro4`。没有这一改时，日志会显示客户端模型名，
**看上去像"压缩没发生"**（本次排查因此多花了一轮）。

### 11.3 原设计中被实测推翻的两处（留作记录）

| 原设计                                       | 实测结论                                                    | 影响                          |
| ----------------------------------------- | ------------------------------------------------------- | --------------------------- |
| `required ≈ prompt_tokens × 2 × 输入价`      | 按**字节**计价：`required ≈ bytes × 输入价`；"×2" 只是当初标定文本恰好 2 字节/token 的巧合 | 阈值从「12.9 万 token」变成「25.9 万字节」，不可混用 |
| Codex 会调 `POST /v1/responses/compact`       | 0.157.1 源码里**没有这个路径**；远程压缩是 `/v1/responses` + `compaction_trigger` | 按原设计实现会做出**零调用**的死代码        |

### 11.4 遗留项

- ~~真实 Codex 的 v2 压缩端到端~~ → ✅ **已跑通**，证据见 §6.1 末（真实 codex 0.157.1 +
  真实上游，压缩调用落在 `solar-pro4`，上游账本对得上，续跑成功）。
- **摘要质量对比实验**（见 §10.2）：只验证了"压缩后能继续"，没做长会话的任务完成度对比。
- **`/v1/chat/completions` 路径没有闸门**：按 §3 的作用域约定没做。若发现非 Codex 客户端
  也在 Router 线上打大请求，需要把闸门扩到该 mode。
- **余额缓存不预热**：按需拉取（每个 key 5min TTL）。已有 `service/kite_credits_task.go`
  的定期扫描会遍历全池余额，将来可把它的结果灌进同一份缓存，省掉请求内的 credits 往返。
- **上游只认裸目录 id**：`upstage/solar-pro4` 会 400，必须过 `model_mapping` 映射成 `solar-pro4`。
  实现里压缩选型会自动过映射（`kiteRouterUpstreamModelName`），**但普通请求仍依赖运营把
  `model_mapping` 配全** —— 这是既有约定，不是本次引入。
