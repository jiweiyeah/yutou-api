# Kilo Gateway 免费模型清单 (isFree=true)

- 来源: `https://api.kilo.ai/api/gateway/models`
- 抓取时间: 2026-09-21 23:55 
- 全量模型数: **380**，其中 `isFree=true`: **21**

| # | model id | 名称 | 上下文 | 最大输出 | 输入模态 | 支持 tools | 到期 | 提示词可能被记录 |
|---|---|---|---|---|---|---|---|---|
| 1 | `kilo-auto/free` | Auto Free | 256K | 10K | text | ✅ | - | ⚠️ 是 |
| 2 | `poolside/laguna-s-2.1:free` | Poolside: Laguna S 2.1 (free) | 262144 | 32768 | text | ✅ | - | ⚠️ 是 |
| 3 | `nvidia/nemotron-3-ultra-550b-a55b:free` | NVIDIA: Nemotron 3 Ultra (free) | 1000K | 65536 | text | ✅ | - | ⚠️ 是 |
| 4 | `dots-studio/dots-3-note-preview:free` | Dots Studio: Dots3-Note Preview (free) | 512K | 460800 | text,image | ✅ | 2026-09-30 | ⚠️ 是 |
| 5 | `nex-agi/nex-n2.5-pro:free` | Nex AGI: Nex-N2.5-Pro (free) | 262144 | 235929 | text,image | ✅ | - | ⚠️ 是 |
| 6 | `inclusionai/ling-3.0-flash-vl:free` | inclusionAI: Ling 3.0 Flash VL (free) | 262144 | 32768 | text,image,video | ✅ | - | ⚠️ 是 |
| 7 | `nex-agi/nex-n2.5-mini:free` | Nex AGI: Nex-N2.5-Mini (free) | 262144 | 235929 | text,image | ✅ | - | ⚠️ 是 |
| 8 | `inclusionai/ling-3.0-flash-sante:free` | inclusionAI: Ling 3.0 Flash Sante (free) | 262144 | 32768 | text | ✅ | - | ⚠️ 是 |
| 9 | `inclusionai/ling-3.0-flash-fin:free` | inclusionAI: Ling 3.0 Flash Fin (free) | 262144 | 32768 | text | ✅ | - | ⚠️ 是 |
| 10 | `qwen/qwen3.8-27b:free` | Qwen: Qwen3.8 27B (free) | 262144 | 235929 | text,image,video | ✅ | - | ⚠️ 是 |
| 11 | `liquid/lfm-2.5-2.6b:free` | LiquidAI: LFM2.5-2.6B (free) | 65536 | 8192 | text | ✅ | - | ⚠️ 是 |
| 12 | `nvidia/nemotron-3.5-lightning:free` | NVIDIA: Nemotron 3.5 Lightning (free) | 1000K | 65536 | text | ✅ | - | ⚠️ 是 |
| 13 | `thinkingmachines/inkling-small:free` | Thinking Machines: Inkling Small (free) | 1048576 | 262144 | text,image,audio | ✅ | - | ⚠️ 是 |
| 14 | `poolside/laguna-xs-2.1:free` | Poolside: Laguna XS 2.1 (free) | 262144 | 32768 | text | ✅ | - | ⚠️ 是 |
| 15 | `cohere/north-mini-code:free` | Cohere: North Mini Code (free) | 256K | 64K | text | ✅ | - | ⚠️ 是 |
| 16 | `z-ai/glm-5.2:free` | Z.ai: GLM 5.2 (free) | 32768 | 29491 | text | ❌ | - | ⚠️ 是 |
| 17 | `nvidia/nemotron-3.5-content-safety:free` | NVIDIA: Nemotron 3.5 Content Safety (free) | 128K | 8192 | text,image | ❌ | - | ⚠️ 是 |
| 18 | `nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free` | NVIDIA: Nemotron 3 Nano Omni (free) | 256K | 65536 | text,audio,image,video | ✅ | - | ⚠️ 是 |
| 19 | `nvidia/nemotron-3-super-120b-a12b:free` | NVIDIA: Nemotron 3 Super (free) | 262144 | 235929 | text | ✅ | - | ⚠️ 是 |
| 20 | `openrouter/free` | OpenRouter Free Models Router | 200K | - | text,image | ✅ | - | ⚠️ 是 |
| 21 | `stepfun/step-3.7-flash:free` | StepFun: Step 3.7 Flash (free) | 262144 | 262144 | text,image | ✅ | - | ⚠️ 是 |

## 备注
- 全部 21 个免费模型的 `pricing.prompt` / `pricing.completion` 均为 `0`（`kilo-auto/free` 为 `-1` 表示动态路由）。
- `mayTrainOnYourPrompts` 全部为 `true` —— 上游可能拿你的 prompt 做训练。
- `kilo-auto/free` 与 `openrouter/free` 是**路由型**模型（随机/择优转发到其它免费模型），不是单一模型。
- 带到期时间的：`dots-studio/dots-3-note-preview:free`
- 视觉/多模态：`dots-studio/dots-3-note-preview:free`, `nex-agi/nex-n2.5-pro:free`, `inclusionai/ling-3.0-flash-vl:free`, `nex-agi/nex-n2.5-mini:free`, `qwen/qwen3.8-27b:free`, `thinkingmachines/inkling-small:free`, `nvidia/nemotron-3.5-content-safety:free`, `nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free`, `openrouter/free`, `stepfun/step-3.7-flash:free`

---

## 免费层限制（2026-09-22 核实）

**官方口径**（`kilo.ai/docs/gateway/usage-and-billing`）：

> All free model requests (both anonymous and authenticated) are rate-limited by IP address.
> **Free models per IP: 200 requests per hour**

| 维度 | 限制 |
|---|---|
| 免费模型速率 | **200 请求/小时**，按**出口 IP**（匿名与已登录一视同仁） |
| 超限后果 | HTTP **429**，`{"error":{"message":"Rate limit exceeded for free models. Please try again later.","code":429}}` |
| 每日/每月上限 | **无**（官方文档未设） |
| 并发限制 | 文档未提及 |
| token 配额 | 无硬性配额；仅建议 Code 模式输出 ≤16k |
| 免费模型计费 | `:free` 条目 cost = 0，不扣余额 |
| 付费模型（无 key） | HTTP **401** `PAID_MODEL_AUTH_REQUIRED` — "You need to sign in to use this model." |
| 付费模型（余额 0） | HTTP **402** `Insufficient balance. Please add credits to continue.` |
| 组织成员日限额 | 可配 per-user daily spend cap，超限阻断至 **UTC 午夜**重置 |
| 组织模型白名单 | 不在 allow list 的模型 → HTTP **403** |

**直连复核**（`POST https://api.kilo.ai/api/gateway/v1/chat/completions`）：

```
无 key  + model=kilo-auto/free        → 200  ✅ 真出结果（本次落到 nex-agi/nex-n2.5-pro:free）
无 key  + model=anthropic/claude-sonnet-5 → 401  PAID_MODEL_AUTH_REQUIRED
假 key  + model=kilo-auto/free        → 401  INVALID_TOKEN   ⚠️ 带了无效 key 连免费模型都不给走
```

- **要匿名用免费模型，就不要带 `Authorization` 头** —— 带了会被校验，无效直接 401。
- 响应**不含** `X-RateLimit-*` 头，模型列表里 `per_request_limits` 恒为 `null` → 限流信息只能看文档，别指望响应头。
- `kilo-auto/free` 是路由层，`autoRouting.models` 只列出 **5 个**上游：`dots-studio/dots-3-note-preview:free`、`inclusionai/ling-3.0-flash-vl:free`、`nex-agi/nex-n2.5-pro:free`、`nvidia/nemotron-3-ultra-550b-a55b:free`、`poolside/laguna-s-2.1:free`。
- ⚠️ **数据条款**：21 个免费条目 `mayTrainOnYourPrompts` 全部为 `true`；Auto Free 可能路由到 NVIDIA 免费端点，受 NVIDIA API Trial 条款约束（trial 用途、会话会被记录）。**不要提交敏感/机密数据。**

来源：`kilo.ai/docs/getting-started/rate-limits-and-costs`、`/docs/getting-started/cost-controls-and-usage-safeguards`、`/docs/gateway/usage-and-billing`（开源源文件在 `Kilo-Org/kilocode` 的 `packages/kilo-docs/pages/`）。
