# 受信任调用方的限流豁免（RATE_LIMIT_BYPASS_TOKEN）

## 问题

A 站（Y-API）的所有服务端请求都从 Cloudflare Workers 的同一个出口 IP 打到本站。
`/api/oauth/state`、`/api/oauth/:provider`、`POST /api/token/:id/key` 挂着
`CriticalRateLimit`（默认 20 次 / 20 分钟，按客户端 IP 计数），于是整站新用户的
开户（每人 2 次）与欢迎邮件取密钥（每人 1 次）共用一个桶，持续吞吐只有 1 次/分钟。
2026-09-11/12 实测 35–60% 的 `/api/oauth/*` 请求返回 429，开户被拖到 30–100 分钟。

## 机制

- 环境变量 `RATE_LIMIT_BYPASS_TOKEN`（`common/init.go` 读入，`common.RateLimitBypassToken`）。
  为空即关闭，请求头被完全忽略，行为与上游一致。
- 请求头 `X-RateLimit-Bypass-Token` 与之常量时间相等时，`rateLimitFactory` 生成的
  **IP 维度**限流（GW / GA / CT / DW / UP）直接放行且不计数（`middleware/custom_rate_limit_bypass.go`）。
- 不影响：用户维度限流（`userRateLimitFactory`，如 SR）、模型维度限流
  （`ModelRequestRateLimit`）、邮件验证码限流、Turnstile、鉴权。

## 配置

服务器 `/opt/new-api/docker-compose.yml` 的 `new-api.environment` 加一行：

```yaml
      - RATE_LIMIT_BYPASS_TOKEN=<随机 ≥32 字符>
```

A 站侧 `wrangler secret put B_RATE_LIMIT_BYPASS_TOKEN` 写入同一个值。两边任一侧缺失
都只是退回按 IP 限流，不会报错——排查时看 nginx 访问日志里 `/api/oauth/state` 的 429。

## 轮换

换值时先改 A 站 secret 再改本站：A 站发新值、本站还认旧值的窗口里请求只是按 IP
限流，不会失败。
