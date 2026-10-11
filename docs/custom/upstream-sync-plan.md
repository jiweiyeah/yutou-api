# 上游代码同步方案（custom ← upstream/main）

> 适用场景：把 `QuantumNous/new-api` 的最新代码合并进本 fork 的 `custom` 分支。
> 本文基于 2026-10-11 的一次完整盘点撰写（落差 372 提交，实测 71 个冲突文件）。
> 执行前请先读 `CLAUDE.md` Rule 8（分支角色与同步纪律），本文不重复那部分。

---

## 1. 现状盘点

### 1.1 版本落差

| | commit | 日期 |
|---|---|---|
| 分叉点（共同祖先） | `5a6c53d49` | 2026-07-18 |
| 本地 `custom` HEAD | `da43dea92` | 2026-10-02 |
| 上游 `main` | `0f38a14a5` | 2026-10-10 |

**落后 372 个提交。** CLAUDE.md Rule 8 要求"至少每周同步"，实际已漂移近三个月 —— 正是规则里警告的"指数级冲突成本"区间。

### 1.2 我们的自定义规模

相对分叉点共 **188 个文件**：新增 88 / 修改 99 / 删除 1。

### 1.3 上游这 372 个提交在做什么

- **按类型**：fix 181、feat 115、refactor 22、chore 15、perf 10、docs 10、test 5、style 3、build 2
- **按模块**：web 55、relay 18、billing 13、plugins 12、channels 12、auth 12、pricing 8、relaykit 7、model 7、jsplugin 6、relayconvert 6、task 6

**三个结构性重构**（决定同步顺序，必须先做）：

| 重构 | 变化 |
|---|---|
| 前端双主题合并 | `web/classic/` + `web/default/` → 单一 `web/src/`，包清单变 `web/package.json` |
| 协议转换层模块化 | `service/relayconvert/` → `relaykit/`（独立 Go module）；`dto/channel_settings.go` → `relaykit/dto/channel_settings.go` |
| task adaptor 插件化 | 内置 adaptor → 沙箱 JS 插件（`plugins/` + `jsplugin`） |

**附带的环境变化**：

- 主 `go.mod` 的 `go` 指令从 `1.25.1` → **`1.26`**
- 主 `go.mod` 新增两个本地模块：
  ```
  require github.com/QuantumNous/new-api/relaykit v0.0.0
  require github.com/QuantumNous/new-api/tokenkit  v0.0.0
  replace github.com/QuantumNous/new-api/relaykit => ./relaykit
  replace github.com/QuantumNous/new-api/tokenkit  => ./tokenkit
  ```
  `relaykit` 运行时依赖只有 uuid / lo / gjson / sjson；`tokenkit` 是重写后的 token 估算器。

### 1.4 冲突面（实测，非估算）

```bash
git merge --no-commit --no-ff upstream/main
```

| 状态 | 数量 | 含义 |
|---|---:|---|
| UU | 49 | 双方都改 |
| UD | 17 | 我们改了、上游删了（前端旧路径 14 + relayconvert 3） |
| AA | 3 | 双方都新增 |
| AU | 2 | 我们新增、上游也新增 |
| **合计** | **71** | |

可自动合并的部分：984 rename / 920 add / 573 delete / 448 modify。

### 1.5 自定义标记覆盖率（决定冲突解决难度）

| | 数量 |
|---|---:|
| 带 `CUSTOM START` 标记的文件 | 37 |
| 我们修改的官方文件 | 99 |
| ├ 带标记（可直接定位改动） | 31 |
| └ **无标记（需人工辨认）** | **68** |

**68 个无标记文件是本次同步的主要风险点** —— 三方合并时无法自动区分"我们的改动"和"上游的改动"，必须逐块人工判断。

### 1.6 同步完成后的实测结果（2026-10-11）

合并完成后，以 `git diff upstream/main` 复核"我们相对上游仍然不同的文件"：

| | 数量 |
|---|---:|
| 与 `upstream/main` 仍不同的文件 | 208 |
| ├ 上游不存在的纯自定义文件（无需标记） | 112 |
| └ 上游存在、需带标记的文件 | 96 |
| &nbsp;&nbsp;├ 本次已补 `CUSTOM START/END` | 89 |
| &nbsp;&nbsp;└ 无法内嵌注释（`web/src/i18n/locales/*.json`） | 7 |

**标记审计口径**：`CUSTOM START`（块标记）与 `CUSTOM:`（行内标记）任一命中即视为已标记。

**已知例外（7 个 locale JSON）**：flat JSON 不支持注释，无法内嵌标记。已改为保证这 7 个文件相对上游**只有纯新增、无删除、无挪位**，合并冲突面因此退化为「两边各加各的键」，可直接 union 解决。

**顺带修掉的两类历史遗留**（不属于本次合并引入，但审计时暴露）：

- `controller/misc.go` 的三处 `GetStatus` 字段只是**位置重排**（与上游同值），已还原成上游顺序，把该文件相对上游的差异从 13 行压到 3 行。
- `web/src/i18n/locales/*.json` 有 2 条上游键被挪到别处，已归位。

**审计方法（可复现）**：

```bash
# 与上游仍不同的文件
git diff --name-only upstream/main > /tmp/modified_vs_upstream.txt

# 其中「上游也存在、但没有标记」的 = 需要补标记的
while read -r f; do
  git cat-file -e upstream/main:"$f" 2>/dev/null || continue
  [ -f "$f" ] && ! grep -q "CUSTOM START\|CUSTOM:" "$f" && echo "$f"
done < /tmp/modified_vs_upstream.txt
```

---

## 2. 同步策略

### 2.1 已有机制（不要重新发明）

`CLAUDE.md` Rule 8 与 `.github/workflows/custom-sync-upstream.yml` 已经定义了标准流程：

- `main` 是 `upstream/main` 的纯镜像，只接受 fast-forward
- `custom` 承载全部自定义，用 `git merge main` 吸收上游，**绝不 rebase**
- workflow 遇到冲突时**失败退出**并提示本地解决，不 force-push、不提交冲突标记

### 2.2 为什么这次不能直接跑 workflow

落差 372 提交、71 个冲突、3 个结构性重构。一次性 merge 只会得到一个无法编译、无法 review 的分支。**必须拆阶段，每阶段独立可编译、可上线。**

### 2.3 分层策略

| 层 | 适用场景 | 手段 |
|---|---|---|
| 日常 | 零散修复、安全补丁 | cherry-pick 单个 commit（如 `b941253ae`） |
| 本次 | 结构性同步 | 分阶段 merge，见第 3 节 |
| 长期 | 避免再次漂移 | 恢复每周 workflow（`custom-sync-upstream.yml` 的 `schedule` 目前是注释状态） |

### 2.4 决策前提

全量同步的代价：71 个冲突文件、前端整体迁移、模块路径全变、`kitedelayed` 这类上游没有的自定义模块要自行维护。

如果目标只是"拿到某个具体修复"，**不要走全量同步**，直接 cherry-pick。全量同步的正当理由只有两个：

1. 需要上游的结构性能力（relaykit 的协议转换、tokenkit 的估算精度、插件化 task）
2. 自定义代码已经开始与上游产生难以调和的分歧，拖下去成本更高

---

## 3. 阶段计划

### 阶段 A：准备与基线

```bash
# 1. 记录基线
git checkout custom
git tag sync-base-20261011
git push origin sync-base-20261011

# 2. 建工作分支（不要直接在 custom 上操作）
git checkout -b sync/upstream-20261011 custom

# 3. 刷新上游
git fetch upstream --prune
```

**基线快照**（阶段 E 验收要用同一套）：

- `go build ./...` 通过
- 前端 `bun run build` 通过
- 关键路径冒烟：`/v1/chat/completions`、`/v1/messages`、`/v1/responses`、`/v1beta/models/*`、计费写入 `logs`、系统任务跑一轮

### 阶段 B：结构性重构

> 顺序不能乱。先把目录结构对齐，后面的冲突解决才有稳定的坐标。

#### B1. 前端目录合并（web/classic + web/default → web/src）

上游把两个主题合成一个。我们改过的 14 个前端文件（`web/classic/*` 6 个、`web/default/*` 8 个）在上游已被删除，属于 UD 冲突。

处理方式：

```bash
# 上游的新结构
git ls-tree -d --name-only upstream/main web/          # → public scripts src

# 我们改过的前端文件
git diff --name-only sync-base-20261011..custom -- web/

# 逐个找新位置（上游做了 rename + 改内容，git 能识别一部分）
git log --follow --oneline upstream/main -- web/src/features/<对应路径>
```

**我们的前端自定义落点**：

| 我们的文件（旧） | 上游新位置 |
|---|---|
| `web/default/src/features/system-info/components/system-tasks-panel.tsx` | `web/src/features/system-info/components/` |
| `web/default/src/features/channels/components/error-rewrite-editor.tsx` | `web/src/features/channels/components/` |
| `web/default/src/features/usage-logs/components/columns/*.tsx` | `web/src/features/usage-logs/components/columns/` |
| `web/default/src/features/users/components/users-*.tsx` | `web/src/features/users/components/` |
| `web/classic/*`（6 个） | **目录已不存在** —— 上游把两个主题合并成单一前端 |

**决策点**：`web/classic` 目录在上游已经消失。若我们仍对外提供 classic 主题，要么自行维护一份（与上游长期分叉），要么借这次同步一并下线。

#### B2. relaykit 模块化

```bash
# 我们改过的 relayconvert 文件（3 个，上游已删除 → UD 冲突）
git diff --name-only sync-base-20261011..custom -- service/relayconvert/

# 上游对应位置
git ls-tree -r --name-only upstream/main relaykit/relayconvert/internal/
```

动作：

1. 接受上游删除 `service/relayconvert/`，代码改用 `relaykit/relayconvert`
2. 主 `go.mod` 加 `require` + `replace`（见 1.3）
3. `dto/channel_settings.go` 的 `AdvancedCustomConfig` 等类型迁到 `relaykit/dto/channel_settings.go`，引用处改 import
4. 我们的 `advancedcustom` 适配器要跟着改类型来源

#### B3. task adaptor 插件化

上游把内置 task adaptor 换成沙箱 JS 插件。检查我们是否有依赖内置 adaptor 的自定义（如 `service/atria_key_recovery_task.go` 等系统任务），确认它们不依赖被移除的接口。

### 阶段 C：自定义模块适配

逐模块对照（详见第 5 节清单）。原则：

- **上游没有的模块**（如 `relay/channel/kitedelayed/`）→ 保持原样，只改它引用的上游接口
- **上游也有的模块**（如 `relay/channel/advancedcustom/`）→ 以上游版本为基线，把我们的改动作为 patch 重放

### 阶段 D：冲突文件解决

按第 4 节的分类逐类处理。建议顺序：

1. 先解决 UD（17 个）—— 决策简单，接受删除 + 迁移改动
2. 再解决 AA/AU（5 个）—— 多半是"两边都新增同名文件"，取并集
3. 最后解决 UU（49 个）—— 需要逐行判断

### 阶段 E：验收与上线

1. 全量编译 + 测试：`go build ./...`、`go test ./...`
2. 前端构建：`bun run build`
3. 跑第 3 节阶段 A 记录的同一套冒烟
4. 本地起实例做端到端验证（可用 skill `yutou-api-local-channel-verify`）
5. 合并回 `custom`，按 `docs/custom/DEPLOYMENT-CHECKLIST.md` 走部署
6. 上线后观察：渠道错误率、计费 `logs` 写入、系统任务执行

---

## 4. 冲突解决指引

### 4.1 分类与处理原则

| 类别 | 数量 | 处理原则 |
|---|---:|---|
| 前端旧路径（UD） | 14 | 接受上游删除，把改动搬到 `web/src/` 对应文件；classic 需单独决策 |
| relayconvert（UD） | 3 | 接受上游删除，改动搬到 `relaykit/` |
| 带 `CUSTOM START` 标记（UU） | ~20 | 保留标记块语义，把上游新逻辑并进来 |
| 无标记（UU） | ~30 | 逐个 diff 判断，**顺手补标记** |

### 4.2 通用手法

**手法 1：先看两边各自改了什么**

```bash
BASE=$(git merge-base HEAD upstream/main)   # = 5a6c53d49
git diff $BASE..custom           -- path/to/file   # 我们的改动
git diff $BASE..upstream/main    -- path/to/file   # 上游的改动
```

**手法 2：无标记文件用"取上游 + 重放自定义"**

对 68 个无标记文件，直接三方合并容易丢代码。更稳的做法：

```bash
git checkout --theirs -- path/to/file        # 先取上游版本
git diff $BASE..custom -- path/to/file       # 找出我们原本的改动
# 逐块重放，并在每块外面补上：
# // ===== CUSTOM START: <原因> =====
# ...我们的代码...
# // ===== CUSTOM END =====
```

**手法 3：用测试当护栏**

我们已有的测试可以直接当回归网：`service/error_rewrite_test.go`、`service/kite_credits_task_test.go`、`relay/channel/kitedelayed/adaptor_test.go`、`relay/proxy/degraded_manager_test.go` 等。合并后先跑这些。

**手法 4：不要用 `-X ours` / `-X theirs` 一把梭**

`git merge -X ours` 会在所有冲突处静默丢弃上游改动，反过来也一样。本次同步必须逐个看。

### 4.3 高风险文件（双方都大改，必须逐行合并）

| 文件 | 我们 +/- | 上游 +/- |
|---|---|---|
| `controller/user.go` | 40/4 | 430/447 |
| `controller/relay.go` | 44/4 | 432/258 |
| `controller/channel.go` | 136/15 | 406/276 |
| `model/user.go` | 20/100 | 396/178 |
| `model/channel.go` | 486/0 | 119/53 |
| `model/log.go` | 9/7 | 124/146 |
| `service/token_counter.go` | 47/0 | 36/179 |
| `relay/channel/openai/adaptor.go` | 160/0 | 256/108 |
| `middleware/rate-limit.go` | 10/0 | 158/93 |
| `controller/channel-test.go` | 20/0 | 228/128 |
| `model/channel_cache.go` | 46/15 | 26/46 |
| `relay/common/response_model.go` | 281/0 | 59/0 |

---

## 5. 自定义模块适配清单

相对分叉点新增的 88 个文件，按功能归组：

| # | 模块 | 主要文件 | 上游是否有 | 适配动作 |
|---|---|---|---|---|
| 1 | 动态代理 | `relay/proxy/degraded_manager.go`、`relay/proxy/helper.go`、`controller/proxy_stats.go` | 无 | 保持；改上游接口引用 |
| 2 | kitedelayed 渠道 | `relay/channel/kitedelayed/*`（5 个） | 无 | 保持；`ChannelType` 注册点需重接 |
| 3 | 渠道缓存监控 | `controller/channel_cache_stats.go`、`model/channel_cache_stats.go` | 无 | 保持 |
| 4 | Claude token 计数 | `controller/claude_count_tokens.go` | 无 | 评估是否被上游 `tokenkit` 覆盖，若是则退役 |
| 5 | 限流绕过 | `common/custom_rate_limit_bypass.go`、`middleware/custom_rate_limit_bypass.go` | 无 | 保持；`middleware/rate-limit.go` 冲突处重接 |
| 6 | 上游错误改写 | `service/error_rewrite.go`、`dto/channel_settings.go`、前端 `error-rewrite-editor.tsx` | 部分（`channel_error_rewrite` 相关类型在 `relaykit/dto`） | 类型来源改 `relaykit/dto` |
| 7 | 系统任务 / Key 池 | `service/atria_key_recovery_task.go`、`kite_credits_task.go`、`tokenharbor_key_scan.go` | 无 | 保持；确认不依赖被插件化的内置 task adaptor |
| 8 | keelcode token | `model/custom_keelcode_token.go`、`service/custom_keelcode_refresh.go` | 无 | 保持 |
| 9 | DeepSeek 免费额度 | `service/deepseek_free_tier.go` | 无 | 保持 |
| 10 | 响应模型名 / 响应 ID 清洗 | `relay/common/response_model.go`、`relay/common/response_id.go` | 部分 | `response_model.go` 是高冲突文件，逐行合并 |
| 11 | 用户聚合 | `model/user_aggregate.go` | 无 | 保持 |
| 12 | CI / 部署 | `.github/workflows/custom-*.yml` | 无 | 保持；注意上游也改 `.github/workflows/` 下的自有文件 |
| 13 | 运维脚本 | `scripts/*`（15 个） | 无 | 保持 |
| 14 | 合规文档 | `docs/legal/privacy-policy.md`、`user-agreement.md` | 无 | 保持 |
| 15 | 渠道级系统提示词 | 无独立文件（散落在 `setting`/`relay`） | 部分 | 检查上游是否已有等价实现，有则收敛 |

---

## 6. 回滚方案

| 时机 | 手段 |
|---|---|
| 同步分支阶段 | `git checkout custom && git branch -D sync/upstream-20261011` |
| 已合并未上线 | `git revert -m 1 <merge-commit>` 或重置到 `sync-base-20261011` |
| 已上线 | 回滚二进制到上一版本 + `git reset --hard sync-base-20261011`（**仅限确认无协作者依赖时**） |

**禁止**：`git push --force` 到 `main` 或 `custom`（CLAUDE.md Rule 8）。

---

## 7. 验收清单

- [x] `go build ./...` 通过，Go 版本已升到 1.26
- [x] `go test ./...` 通过（尤其自定义模块的测试）
- [x] 前端 `bun run typecheck` + `bun run test` 通过（178 文件 / 2197 用例），`web/package.json` 单一入口
- [x] `relaykit` / `tokenkit` 的 `require` + `replace` 已就位，且 `GOWORK=off go build/test ./...` 独立通过
- [x] 四条协议路径冒烟：chat / messages / responses / gemini
- [x] 计费 `logs` 正常写入，`usage` 口径无异常跳变
- [x] 系统任务（`system_tasks`）能正常跑一轮
- [x] `relay/channel/kitedelayed` 与 `advancedcustom` 渠道可正常调用
- [x] 无标记文件已补 `CUSTOM START/END` 标记（见 §1.6；7 个 locale JSON 为已知例外）
- [x] 合并提交完成：`e27917637`（双亲 `da43dea92` + `0f38a14a5`），本地快进 `custom`
- [x] `custom-sync-upstream.yml` 的每周 `schedule` **保持关闭**（2026-10-11 决定，仅手动 `workflow_dispatch`）
- [ ] **推送 `origin/custom`** —— 待用户决定上线节奏
- [ ] **生产部署需用户明确许可**（本次改动范围大，禁止未授权上线）

### 7.1 本地端到端验证记录（2026-10-11）

在 `/tmp/na-e2e` 用真实二进制 + mock 上游（`127.0.0.1:39001`）跑通：

| 验证项 | 结果 |
|---|---|
| 四条协议路径（含流式） | 全部 200 |
| 计费 | 5 条 log 记录，每条 quota=38，`used_quota`=190 |
| 重试/故障转移 | policy trace 完整：`failure/upstream_failure/transport` → `retry/retry_status_matched/global` → `success/request_completed` |
| 多 key 渠道 | 3 key，`get_key_status` 正确 |
| 安全门 | 无 proof 返回 `SECURITY_PROOF_REQUIRED`，伪造 proof 返回 `SECURITY_PROOF_INVALID` |
| 渠道级 `error_rewrite` | 首次尝试与**重试路径**均生效（`retry: 2->1->4` 后命中渠道 4 并改写为 `REWRITE-MATCHED`） |

**排查记录**：`error_rewrite` 一度被误判为"重试时不生效"，实为复现用例本身无效 —— 连接被拒走的是
`api_request.go` 的 `HideErrMsg("upstream error: do request failed")`，错误类型是 `ErrorTypeNewAPIError`，
被 `isRewriteableUpstreamError` 有意排除（该机制只改写**携带上游响应**的错误）。改用上游真返回 503 后确认
改写正常，并补了 `middleware/distributor_channel_settings_test.go` 锁住"每次尝试刷新渠道级配置"这一契约。

---

## 8. 附录：常用命令

```bash
# 分叉点
BASE=$(git merge-base custom upstream/main)

# 我们的改动全貌
git diff --name-status $BASE..custom

# 上游的改动全貌
git diff --name-status $BASE..upstream/main

# 潜在冲突面（双方都改过的文件）
comm -12 <(git diff --name-only $BASE..custom | sort -u) \
         <(git diff --name-only $BASE..upstream/main | sort -u)

# 试合并并统计真实冲突（不提交，随时可 abort）
git merge --no-commit --no-ff upstream/main
git status --porcelain | awk '{print $1}' | sort | uniq -c
git diff --name-only --diff-filter=U
git merge --abort

# 找上游修复某个具体问题的 commit
git log upstream/main -S "<关键字>" --oneline -- <路径>

# 验证某个修复能否干净应用（不落盘）
git diff <commit>^ <commit> -- <路径> > /tmp/fix.patch
git apply --check /tmp/fix.patch
```

---

**文档维护**：每次完成一次同步后，更新第 1 节的版本落差表与第 1.4/1.5 节的实测数据。
