# yutou-api 项目备忘

**细节全在 skill 里，这里只留索引 + 会立刻踩坑的硬事实。**

## 环境
- Go 隔离：`export GOTOOLCHAIN=local GOPATH=~/.workbuddy-ai/binaries/go/gopath GOMODCACHE=~/.workbuddy-ai/binaries/go/modcache GOCACHE=~/.workbuddy-ai/binaries/go/build-cache`，再用 `~/.workbuddy-ai/binaries/go/go/bin/go build ./...`
- ssh `yutou-prod`（免密）；容器 `new-api`/`-postgres`/`-redis`；`/opt/new-api/` 只有二进制 → **排查改看本地 git**（custom 分支=线上）。
- 查库：`ssh yutou-prod 'docker exec -i new-api-postgres psql -U newapi -d newapi -t -A' <<'SQL' … SQL`；Grep 偶发漏匹配 → 用 Bash grep 复核。
- 取 key 直连上游：`channels.key` 是**明文**，多 key 以 `\n` 分隔 → `split_part(key,chr(10),N)` + `tr -d '\r'`；管理 API `GET /api/channel/<id>` **不回传 key**；root token 在 `users`(role=100)。

## ☠️ 地雷（必记）
- ⚠️ **Kite Router 上游的额度预检按「请求体字节」计价，不是 token**：
  `required ≈ 字节数 × 输入价/M + max_tokens × 输出价/M + $0.004`（38 点标定误差 <0.4%）。
  早先流传的 `tokens × 2 × 输入价` 只是标定文本恰好 2 字节/token 的巧合。astra 的 $4.5 线
  = **258,773 字节**（≈6.5 万 token 散文 / 13 万 token 代码）。详见 skill + `docs/kite-router-codex-context-budget.md`。
- ⚠️ **Codex 远程压缩是 v2 协议**：`POST /v1/responses` + input 末尾 `{"type":"compaction_trigger"}`，
  响应两条 SSE（`response.output_item.done` 带 compaction item + `response.completed`）。
  `/v1/responses/compact` 在 codex 0.157.1 里**零引用**。且**只有 provider 名配成 `azure`
  才会触发**（`openai` 是保留 id 会被拒；自定义名走客户端本地压缩）。已实现见
  `relay/channel/kitedelayed/router_{budget,balance,compact}.go`。
- ⚠️ **该上游只认裸目录 id**：`solar-pro4` ✅ / `upstage/solar-pro4` ❌（400 unsupported model）；
  渠道 `models` 的 `vendor/name` 必须靠 `model_mapping` 映射。
- 给 `status=1` 渠道写 `abilities` 里**不存在**的 group（**空 group 也炸**）→ ≤60s panic（`channel_cache.go:26-70` nil map，无 recover）。改 `channels."group"` 前跑 skill 自检 SQL 或走管理 API。
- `channels.status` ≠ `abilities.enabled` → 启用渠道**两表同改**。
- 写 JSON 列别用 `ssh "… psql -c \"…\""`（双引号被**静默剥掉** → 非法 JSON → override 静默失效而 SQL 报 `UPDATE 1`）。用 `<<'SQL'` heredoc + `$j$…$j$`，自检 `(col::jsonb IS NOT NULL)`。
- ⚠️ `weight=0` ≠ 零流量：按 request_path 过滤后只剩 1 个渠道时 `channel_cache.go:160-167` 直接返回、**不走权重**。
- ⚠️ 同一对外模型名 ≠ 同一真实模型：`model_mapping` 是**渠道级**的 → 同档均分表现为「同一模型时好时坏」。排查模型级异常**先横向比对各渠道 model_mapping**。
- 线上 `RedisEnabled` → `main.go:79-82` 强制内存缓存，**改配置最多 60s 生效**。
- ⚠️ **网关对大量参数不做校验，错值只能由上游拒**：`reasoning_effort` 是裸 `string`（`dto/openai_request.go:39`，无枚举校验），另有 30+ 个 `json.RawMessage` 透传字段（`user`/`metadata`/`logit_bias`/`prediction`/`thinking`…）同样原样转发 → 上游 400/422。**渠道 10850 的 422 全部来自 `reasoning_effort` 越界**（atria 只认全小写 `low|medium|high`；`xhigh`/`minimal`/`none`/`max` 一律 422），详见 skill。
- ⚠️ `ssh host 'bash -s' <<'EOS'` 里嵌套 `docker exec -i … psql -c "…"` → **`-i` 会把 heredoc 里剩余的脚本文本当 stdin 吃掉**，脚本静默截断（现象是前半段有输出、后半段凭空消失）。`-c` 查询用不带 `-i` 的 `docker exec`；要喂 SQL 就用 `-i` + 重定向文件。
- ⚠️ 写 JSON 列时 `$j$` **紧跟** `{` 会被外层 shell 当 `${...}` 展开（报 `Bad substitution`）→ 把 `{` 换到下一行。

## 列类型 / 常量
- `setting`/`settings`=text；`channel_info`=json（收尾 `::json`）；`system_tasks.result`=**text 且有空串** → 一律 `NULLIF(result,'')::jsonb->>'x'`；`logs.created_at`=bigint 秒（套 `to_timestamp(...) at time zone 'Asia/Shanghai'`）；`logs` **无** `status_code`（在 `other` 里）。
- 多 key `multi_key_status_list` map[index]status，**不在表里即启用**；1=启用/2=手动禁用/3=自动禁用；UI「删除禁用密钥」**只删 3**。
- 管理 API：`Authorization`=**裸 access_token**（无 `Bearer `）+ `New-Api-User: <userId>`。`tokens.key` 存去掉 `sk-` 的串，key 不能含 `-`。`QuotaPerUnit=500000`($1)。
- `logs.type`：1=充值/2=消费/3=管理/4=系统/5=错误；404 不触发自动禁用。`type=3` 只比对 models/group/type/base_url/key → 改 weight/priority/status/setting/param_override **不留痕**。
- ⚠️ **按渠道统计"成功数"别用 `other->>'channel_id'`**：只有 `type=5`（错误）行有顶层 `channel_id`；`type=2`（消费/成功）行的渠道在 `admin_info.use_channel` **数组**里 → 只筛 `channel_id` 会得出「成功数恒为 0、错误率 100%」的假象。正确写法：`other LIKE '%"<id>"%'` 或 `NULLIF(other,'')::jsonb->'admin_info'->'use_channel' ? '<id>'`。
- `other` 里 `content` 可能很长（上游把整段校验报错回写）→ 批量查错误构成时一律 `substr(content,1,80)`，否则输出能到 MB 级。

## 选路 / 重试 / 定价
- **priority=主备降级**；**weight=档内比例**，`Σw==0` 均分；weight=0 且同档有正权重 → 份额 0、**无保底**。二者是**渠道级**；让某模型只走某渠道 → 改 `channels.models`。
- **只有 type 58 按 request_path 过滤**（`channel_cache.go:247` 非 58 一律放行，`distributor.go:172` 同）→ type 8 会接到自己不支持的端点。⚠️ **type 8 的上游 URL = `base_url` 原样**（`openai/adaptor.go:317`，只替换 `{model}`，**完全不拼入站路径**）⇒ 一个 type 8 渠道 = 一个固定端点。**选型判定：上游有多个原生端点（chat/responses/messages…）→ 必用 type 58；只有一个端点 → type 8 够**。⚠️ 若上游根路径返回 200（首页 HTML），type 8 写 host 会**静默拿到 HTML 而不报错**（pgsgrove 即如此）。排查用 `NULLIF(other,'')::jsonb->>'request_path'`。
- ⭐ **多 key 渠道自动禁用是 key 级的、不连坐**：`processChannelError`(relay.go:253) 带 `usingKey` → `handlerMultiKeyUpdate`(model/channel.go:650-703) 只写该 index=3；usingKey 空才改渠道整体；全 key 禁用才渠道 AutoDisabled。
- ⭐ **`kite_credits` 任务是 Kite key 池唯一的低余额/死 key 防线**（09-26 回退把 `router_balance.go` 的余额感知选 key 一起删了，**没有别的兜底**）。2026-09-27 起它已是**权威状态源**：余额 ≤$0.10 禁用、≥$0.20 放回、中间死区不动；人工禁用(status=2)不碰；判定**跨渠道并集下发**（同一个 key 池挂 10821 Marathon + 10867 Kite Router 两个渠道，而 key 禁停状态是渠道级存的）。细节见 skill。
- ⚠️ **key 禁停状态 `multi_key_status_list` 是渠道级的**：`AutoDisableChannelKeys` / `EnableChannelKeys` / 管理 API 全是单渠道语义。凡是两个渠道共用 key 池，写入就必须自己做跨渠道对称，否则必然单向漂移。
- 定价只看 `options.ModelRatio`（**整体覆盖**，不合并），缺配 → `model_price_error`。渠道级系统提示词 **override=false 时只要请求自带 system 消息就整段失效**（头号「配了没生效」），`/v1/responses` 不生效。
- 判空跑看 `system_tasks.result` 计数（`channels=0`/`keys_checked=0`），别看 status；空跑第二副面孔 = **配置过期**（渠道被删、任务还扫硬编码 ID）→ 修法通常是**关任务**（`Enabled()==false`）。上游语义：401 真死 / 402 临时自愈 / 403 需人工 / 429 限流；只有 `DisableChannelKeyUntil` 可自愈。
- **atria 渠道布局（2026-09-23 晚起）**：**只剩一个渠道 `10864 atria-asi`（type 58）**，三条路由——`/v1/chat/completions` 与 `/v1/responses` 走 native 透传（`converter: none`），`/v1/messages` 走 `anthropic_messages_to_openai_chat_completions` 转换到 `/v1/chat/completions`（保持原计费口径）。`base_url` 只写 host（`https://api.atria-asi.ai`），`weight=0`——**单渠道下权重不参与**（`channel_cache.go:160-167` 单候选直接返回，不走权重）。旧的 `10850`（type 8）**已删除**。⚠️ 留个反面教材：type 8 的 `base_url` 写死完整 URL，会把**所有入站路径**都塞进 `/v1/chat/completions`——这正是 `/v1/responses` 曾经 100% 失败的原因。

## 非 OpenAI 形状的上游怎么接（Drex 这类决策模型）
- **入站路径不可自定义**（`router/relay-router.go` 全是固定路由）→ 上游自己的端点名（如 `/v1/systemone`）暴露不出去，只能借 `/v1/chat/completions` 等现有路由。
- `helper.GetAndValidateRequest`（`controller/relay.go:113`）在进 adaptor **之前**强制反序列化 + 校验 → 自定义 body 必须自带占位 `model` 和 `messages` 才过得了，否则 400 `field messages is required`。
- **逃生口 = 渠道设置 `pass_through_body_enabled`**（`compatible_handler.go:97-107`）：跳过转换/字段过滤/param_override，**原始字节直发**上游（`common/gin.go:86-130` 证明 storage 是原始 body 且 unmarshal 不改写）。配合 type 8（`base_url` 原样用）或 type 58（`advanced_routes` 配 `converter: none`）。
- 代价：响应侧 `OpenaiHandler` 见 `usage.prompt_tokens==0` 就用**估算值覆写 `usage`**（上游原生 usage 丢失，其余字段原样回传）；completion 侧无 `choices` → 恒 0，计费实际只剩输入侧估算；type 8 的上游模型更新检测会拼成 `{base_url}/v1/models` 而 404。
- ⚠️ **透传只对"知道要发什么"的自研调用方成立**。OpenAI 形状的客户端（Claude Code / Cursor / LobeChat / 任何 SDK）**接不了**——它们发 `messages` 期待 `choices[].message.content`，占位字段只骗得过网关校验、骗不过客户端解析。这正是社区把 Jev/Drex 接进 Claude Code 时走 **hook / MCP / 轮次路由** 而**不是**改 `base_url` 的原因（MCP 那条的桥是"LLM 自己填工具参数"，不是转换层）。
- **结论：new-api 是模型网关，决策模型不是模型。** 要正经托管就得**给它原生路径**（像 APIMaster 卖 `jev-latest` 那样暴露 `POST /v1/systemone`）——新增独立 relay 路由 + 新 API type，而不是复用 `RelayFormatOpenAI` 硬塞。

## 宿主机 CPU steal（2026-10-07 整机 hang 事故）
- ⚠️ **yutou-prod 是共享型实例，宿主机 CPU 争抢严重**：`/proc/stat` 累计 **steal=34.4%**，瞬时采样 7.0%~41.7%（4 核 Xeon Gold 6138 @2.00GHz）。**idle 57% 时仍有 34% steal** → 是宿主机主动抢占，不是本机负载所致。
- 2026-10-07 23:35:45（北京）整机完全失去响应，用户从控制台硬重启，中断 31 分钟（00:06:51 恢复）。判据：**`last -x` 无 shutdown 记录 + journal 文件 truncated** = 硬复位。
- ⚠️ **判挂死看「容器日志」断点，不是 syslog**：该机应用日志走容器 stdout，syslog 本来就稀疏（14:28→15:17 空 49 分钟属正常）。本次 dockerd 停写 15:35:45 UTC 才是真断点。
- ⚠️ **postgres `checkpoint complete: write=35~79s` 不是磁盘慢**：那是 `checkpoint_completion_target` 主动限速的设计行为（重启后第一条 `write=0.181s` 印证）；dd 实测顺序写 **87.9 MB/s**。VM 被暂停的铁证是 `clocksource: Long readout interval`（时钟跳 1.3s/4.7s/9.4s）与 `hrtimer: interrupt took 112625804 ns`。
- 待办隐患：无 `/etc/docker/daemon.json`（容器日志无轮转，json.log 已 425MB）；journal 占 1.8G 且无 SystemMaxUse；容器内存限额合计 4.2G > 物理 3.9G；根分区挂 `discard` 且 fstrim.timer enabled。

## 相关 skill
`yutou-api-prod-channel-ops`（最全）· `yutou-api-channel-cache-attribution` · `yutou-api-local-channel-verify` · `cline-api-free-models`
