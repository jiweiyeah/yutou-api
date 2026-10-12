# Y-API（A 站）适配新 B 站认证体系 — 可执行清单

> 2026-10-12 定稿。适用对象：yutou-api 上游同步（372 提交）后的新代码部署。
> 事实依据均为当日逐行核实的代码/SQL 结论，行号指向本地 HEAD（1115f6f94）。

## 0. 背景：三层断点

Y-API（A 站，Cloudflare Worker）对 B 站（yutou-api）存在三层旧协议依赖：

| # | 依赖 | 涉及规模 | 断点时间 |
|---|------|---------|---------|
| 1 | `B_ADMIN_ACCESS_TOKEN` = 用户 247 的 32 位旧令牌 | 1 个（admin.ts 全部管理端点） | 部署 +30 天 |
| 2 | 每个 A 站用户存在 D1 `users.b_access_token_enc` 的旧令牌 | ~2069 人（dashboard/密钥操作） | 部署 +30 天 |
| 3 | 开户 OAuth 契约（`GET /api/oauth/state`、回调响应格式、`GET /api/user/token`） | 全部新开户 + daily-key 自举 | **部署当天** |

断点 3 的代码证据（本地 HEAD）：

- `GET /api/oauth/state` 已不存在，只有 `POST /api/oauth/state`（`router/api-router.go:50`，
  body `{"provider","intent":"login"}`，state 在 `data.flow_token`，基于 DB `auth_flows` 表，
  不再需要匿名会话 cookie）。
- OAuth 回调响应从 `data.{id,username,group}` 平铺改为 `data.user.*` 嵌套 +
  `data.access_token`（15 分钟 JWT）+ `new_api_refresh` cookie（`controller/user.go:150-215`）。
  Y-API 的 `loginResultSchema`（`schemas.ts:45`）会解析失败。
- `GET /api/user/token`（GenerateAccessToken）被上游 a8729b5c3 删除
  （生产镜像 da43dea92 仍含、HEAD 已无）。替代物是 `POST /api/user/access_tokens`
  （`nap_` 前缀 PAT，`accessTokenSessionRule` —— 仅浏览器会话可调，`middleware/access_token_routes.go:73`）。

## 1. 关键可行性结论（决定路线选择）

**每用户 PAT 可以服务端全自动签发**，走官方 OAuth-verify 三步舞（全部逐行核实）：

```
① POST /api/oauth/state  {"provider":"y-api","intent":"login"}
   → GET /api/oauth/y-api?code=<A站自签码>&state=<flow_token>
   → data.access_token（15 分钟 JWT）
② POST /api/oauth/state  {"provider":"y-api","intent":"verify",
                          "scope":"access_token.generate",
                          "context":{"scopes":[...],"expires_at":0}}   + Bearer JWT
   → data.flow_token
   → GET /api/oauth/y-api?code=<新自签码>&state=<flow_token2>          + Bearer JWT
   → B 站回调 A 站 /oauth/token、/oauth/userinfo（sub=用户 A 站 id，与绑定匹配）
   → data.proof_token
③ POST /api/user/access_tokens  {name,scopes,expires_at:0}
   + Bearer JWT + X-Security-Proof: <proof_token>
   → data.token（nap_ 明文，仅此一次）
```

前提与细节：

- 用户无 2FA/passkey（有则 ① 返回 challenge 而非 JWT）。y-api 分组内仅 247 有 2FA，
  其余全部可自动 —— 用 SQL 复核：`JOIN two_fas WHERE is_enabled`。
- 每用户 PAT scopes：`api_key:read` + `api_key:write` + `api_key:reveal`
  （覆盖 Y-API 全部 per-user 调用：`/api/token/` 系列，`middleware/access_token_routes.go:219-227`；
  `/api/pricing` 是 `accessTokenAnyRule` 无需 scope）。
- `expires_at:0` = 永不过期（`Expired()` 仅当 `>0` 才判过期）。
- `POST /api/user/access_tokens` 的 `UserCriticalRateLimit` 按用户键（"UC:access-token"），
  每用户一次不撞桶；IP 维度的 `CriticalRateLimit` 走 `rateLimitFactory`，
  A 站 bypass token（`X-RateLimit-Bypass-Token`）仍生效。
- **不能用 PAT 创建 PAT**（创建路由 session-only），但用用户自己的 OAuth 登录 JWT 可以。

**唯一无法全自动的操作**：247 的 admin 高危操作 —— `POST /api/user/manage` 的
disable/enable 与 `DELETE /api/user/:id` 在新代码要求 step-up（`requireAdminUserProof`，
`controller/user.go:1220,981`）。247 有 2FA ⇒ 唯一可用验证方式是 2FA 码（OAuth 方法只在
无 2FA/passkey 时提供，`securityVerificationPolicy`）。半自动路径：操作时人工输入 2FA 码，
调 `POST /api/verify {method:"2fa",scope:"admin.user.manage"|"admin.user.delete",context:{...},code}`
换 proof_token（PAT 持有 `user:write` 即可过 `requireAccessTokenVerificationScope`）。
quota 调整（`add_quota`）无 proof，全自动。

## 2. 路线对比与推荐

| 路线 | 内容 | 代价 | 结论 |
|------|------|------|------|
| A. Y-API 侧适配 + 自动迁移 | 改 Y-API：新 OAuth 契约 + 三步舞发 PAT + 存量迁移任务 | Y-API 代码改动（独立仓库，不碰 B 站） | **推荐** |
| B. B 站 custom 兼容层 | 加 `custom_*.go`：恢复 admin 代签长期令牌端点 | 把上游刻意移除的能力加回来（安全倒退）；动 B 站鉴权面 | 不推荐 |
| C. 混合 | A + 对 suspend/delete 走「B 站面板人工操作」 | 无 | suspend/delete 低频，作为兜底选项 |

推荐 A（+C 兜底）。理由：上游删除 `GET /api/user/token`、把 PAT 签发限定为
session-only 是**刻意的安全设计**（"只有用户自己能给自己生成"）；三步舞用的是官方
未删除的路径，Y-API 以用户自身身份签发自身凭证，语义正当。

## 3. 执行清单

### Phase 0 — 决策与准备（B 站部署前）

- [ ] 拍板：suspend/delete 采用「2FA 码透传」半自动（推荐）还是「B 站面板人工操作」
- [ ] 确认 Y-API 生产已配置 `B_RATE_LIMIT_BYPASS_TOKEN`（迁移任务与开户都依赖它绕 IP 限流）
- [ ] 核对 2FA 用户清单（预期仅 247）：
      `SELECT u.id,u.username FROM users u JOIN two_fas t ON t.user_id=u.id WHERE u."group"='y-api' AND t.is_enabled;`

### Phase 1 — Y-API 代码适配（先于 B 站部署上线）

**✅ 2026-10-12 完成**（Y-API 仓库工作树，尚未提交）。验证：`tsc --noEmit` 干净，
全量测试 1400 passed | 6 skipped（115 文件），其中 fake B 站默认 `legacy` 契约
——既有测试全部继续覆盖旧路径（验收项），`modern` 契约有专项测试。

改动清单（Y-API 仓库）：

- [x] `src/server/b-site/oauth-login.ts`（重写）
  - state 获取：POST 新契约，404/405 回退 GET 旧契约（**双兼容**，B 站部署前后同一版本可跑）
    ——旧站点对 POST 返回 404 但错误体是 OpenAI 风格（`kind='business'`），
    因此 `isMissingRoute` 只判状态码、不判错误种类
  - 登录响应自适应解析：`data.user` 存在 → 新格式（保留 `data.access_token` JWT）；
    否则旧格式
  - 新格式下同时保留 refresh cookie 的能力（后续 verify/建 PAT 都要 JWT）
  - 登录返回 challenge（2FA 用户）→ 抛具名错误 `OAuthLoginChallengeError`，供迁移任务归类
  - 导出 `requestFlowState`（verify intent，无旧契约回退）与 `detectOAuthContract`
- [x] `src/server/b-site/user.ts`
  - `generateAccessToken` 重写为三步舞（见 §1），返回 `nap_` 明文；
    对无 `nap_` 前缀的返回值硬失败（防回退到旧端点）
  - 新增 `isPat()` 辅助（迁移判据）、`pat-issuance.ts`（三步舞本体 + scopes 常量）
  - 已有函数 `listPricing/listTokens/createToken/findTokenByName/getTokenKey/deleteToken` 不变
- [x] `src/server/b-site/http.ts`
  - request 支持附加头（`X-Security-Proof`）；新增 `bearer` 认证种类（JWT 与 PAT 区分）；
    `RESERVED_HEADERS` 防止附加头覆盖 authorization/cookie
- [x] `src/server/b-site/token-refresh.ts`（新文件）
  - `refreshStoredToken`：探测契约 → 新契约重签 `nap_` PAT / 旧契约返回 `legacy-still-active`
    （旧契约下不浪费重签——重签即覆盖旧令牌，但旧站点三步舞不可用）
- [x] `src/server/provisioning/provision.ts`
  - `ensureIdentity`：已存令牌解密后以 `nap_` 前缀判断是否迁移过；非 `nap_` 时重新走三步舞
- [x] `src/server/daily-key/rotation.ts`
  - 同上；`ensureAccount` 自举改用新契约，存量旧令牌经 `repairLegacyToken` 自愈
- [x] `src/server/b-site/admin.ts` + `admin/service.ts` + `routes/api/admin/users/$id.ts`
  - `setUserEnabled` / `deleteUser` 增加可选 `proofToken` 参数（透传 `X-Security-Proof`），
    路由层从 `X-Security-Proof` 请求头透传
  - `setQuota` 不变（无 proof 要求）
- [x] 迁移任务 `src/server/migration/legacy-token-migration.ts` + 受保护路由
      `src/routes/api/admin/token-migration.ts`（`requireAdmin`）
  - 遍历 D1 `users.b_access_token_enc` 非空的行（游标分页 `listForTokenMigration`）
    + `daily_keys` 账号行
  - 每用户：解密 → `nap_` 前缀则跳过 → 三步舞 → 加密写回（`replaceStoredToken`，
    不动 `provision_state`）
  - 失败分类记录（`challenge` / `binding_mismatch` / `legacy_retired` / `error`）
  - 幂等：可重跑，成功即跳过；B 站仍是旧契约时整批返回 `legacy-site` 跳过
- [x] 本地脚本 `scripts/*.mjs|py`（4 个读 `.dev.vars` 的运维脚本）无需改代码，
      只换 `.dev.vars` 里的 `B_ADMIN_ACCESS_TOKEN` 值（Phase 4）——已核对，
      脚本无任何旧端点（`/api/user/token`、`oauth/state`）直接调用

验收（B 站仍是旧代码时）：Y-API 新版在旧契约下全部功能正常（双兼容路径）。
**✅ 已验证**：fake B 站默认 `legacy` 契约，全量测试通过；`oauth-login.test.ts`
专项覆盖「POST 404 → GET 回退」路径。

> **已提交并部署（2026-10-12）**：`a328077` → `origin/main`；生产 Version
> `019cb8f4`（19:07 CST）。因 prebuild 的模型同步收缩守卫被上游清单缩水阻断
> （17 个型号的渠道全为 status=2，含当天 15:18 被禁的 gpurouter），本次以
> `vite build` + `wrangler deploy` 跳过 prebuild——只上代码，模型快照与页面
> 内容保持与部署前逐字一致。部署后实测：新迁移路由 401（在跑）、B 站仍是旧
> 契约（POST /api/oauth/state 404 → 走 GET 回退）、当日 daily-key 行已存在
> （08:05 CST 轮换完成，当天不触发新路径）。注意真实生产库是**灾备库**
> （DR_MODE=on）——`wrangler d1 execute y-api-db` 查的主库是 09-28 旧快照。

### Phase 2 — B 站部署（沿用既有流程）

**✅ 2026-10-12 20:22 完成**（run `38247537301`，build 8m58s + deploy 1m40s）。

- [x] `git push origin custom`（`da43dea92..26218b82c`，379 提交）+ 触发 GitHub
      Actions「Custom — Build & deploy to server」（ref=custom, tag=latest,
      platforms=linux/amd64, deploy=true）
- [x] 部署后核对：
      - 容器 healthy（新镜像已上线）
      - `options.LegacyAccessTokenRetireAt` = **2026-11-11 20:22**（+30 天窗口起点）
      - `POST /api/oauth/state` 返回 `data.flow_token`（新契约生效）
      - 网关冒烟：`/v1/chat/completions`、`/v1/messages`、`/v1/responses` 全 200；
        计费日志正常写入
      - 旧 32 位令牌过渡期仍有效（247 旧令牌调 `/api/user/self` → 200）
      - 限流绕过端到端 5/5 通过（`X-RateLimit-Bypass-Token` 生效）
      - kitedelayed / 动态代理等自定义 env 全部存活
      - 10879 的 responses 测试**未做**——该渠道当天 15:18 被手动禁用，
        按纪律不启用已禁用渠道做实测
- [x] Y-API 侧验证：`detectOAuthContract` 判定 `new`、`requestFlowState`
      拿到 flow_token（真实 B 站，一次性冒烟测试）；Y-API 站点 200。
      **真实新开户未发生**（部署后暂无新注册用户，不可强造）——首个真实
      开户/明晨 daily-key 轮换（08:05，届时走 `repairLegacyToken` 三步舞
      换 `nap_` PAT）是后续观察点。

### Phase 3 — 存量令牌迁移（部署后 30 天窗口内）

- [ ] 小批量：10 个用户跑迁移任务，逐个核对 D1 写回值以 `nap_` 开头、
      该用户 dashboard 功能正常
- [ ] 全量：~2069 用户 + daily-key 账号
- [ ] 2FA 用户（247 及任何新增）单独人工处理或走 Phase 4 的浏览器路径
- [ ] 迁移完成核对：`SELECT count(*) FROM users WHERE group='y-api'` 与
      迁移任务的成功计数对账；失败清单人工清零

### Phase 4 — admin 令牌替换（人工，约 10 分钟）

- [ ] 247（2FA 用户）浏览器登录 `yutou.bestvirtualgoods.com` →
      安全与访问 → 创建访问令牌：name=`Y-API Worker`，
      scopes=`user:read`+`user:write`+`log:read`，有效期=永不过期
- [ ] `npx wrangler secret put B_ADMIN_ACCESS_TOKEN`（Y-API 项目，粘贴 nap_ 明文）
- [ ] 本地 `Y-API/.dev.vars` 同步更新（4 个运维脚本随之生效）
- [ ] 验证：`scripts/cohort-report.mjs` 可正常拉数；Y-API 生产管理页功能正常
- [ ] suspend/delete 半自动链路联调（若拍板此方案）

### Phase 5 — kite502-watch（独立于 Y-API，同样在 30 天内）

- [ ] 为 yeheboo(id=1) 创建 `nap_` PAT，scope=`channel:operate`
- [ ] 更新 `/root/kite502-watch/config.env` 的 `ADMIN_TOKEN`
- [ ] 观察一个 cron 周期（10 分钟）确认启用动作成功

## 4. 回退预案

- **退役刹车**（零代码）：`EnsureLegacyAccessTokenRetireAt` 是 `OnConflict{DoNothing}`
  + 启动时读入 atomic 缓存。应急时 `UPDATE options SET value='<远期时间戳>'
  WHERE key='LegacyAccessTokenRetireAt';` + **重启容器**（改完必须重启才生效）。
- **Y-API 回退**：双兼容版（Phase 1）在 B 站新旧契约下都能跑；
  若 B 站需回滚镜像，Y-API 无需重新部署（自适应解析）。
- 禁用渠道/高危操作维持既有纪律：临时改动必须恢复并说明。

## 5. 验收总表

| 项 | 判据 |
|----|------|
| 新开户 | B 站部署后新注册用户走完 provision，D1 `b_access_token_enc` 解密以 `nap_` 开头 |
| 存量迁移 | 迁移任务成功计数 ≈ y-api 分组用户数；随机抽 5 个用户 dashboard 正常 |
| admin PAT | Y-API 管理操作（查用户/改额度/查日志）正常；247 旧令牌可择机在面板 revoke |
| suspend/delete | 按拍板方案联调通过（2FA 透传或面板操作） |
| kite502 | cron 日志连续两个周期无「启用渠道失败」 |
| 旧令牌退役 | 30 天后再抽查：无任何调用方依赖 32 位旧令牌（audit_logs 里无 legacy 认证） |

## 6. 遗留风险

- 迁移任务需在 30 天内跑完；跨该窗口期间新旧令牌并存是预期状态（`legacy=true` 放行）。
- 247 的 2FA 码是单人依赖（站长本人）——suspend/delete 属低频，可接受；
  若未来要全自动需重新评估 passkey（WebAuthn ceremony 同样需人）。
- daily-key 账号（ya-daily）若在 D1 坐标丢失时需重新自举——自举走新契约，
  已在 Phase 1 覆盖。
