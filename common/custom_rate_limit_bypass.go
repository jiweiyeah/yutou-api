package common

// RateLimitBypassToken 是受信任的服务端调用方（A 站 Y-API）用来跳过 **IP 维度**
// 限流的共享密钥，由 InitEnv 从环境变量 RATE_LIMIT_BYPASS_TOKEN 读入；为空即关闭，
// 此时相关请求头被完全忽略，行为与上游一致。
//
// 背景：Y-API 的全部服务端请求都从 Cloudflare Workers 的同一个出口 IP 打到本站，
// CriticalRateLimit 的 20 次 / 20 分钟按 IP 计数，等于整站新用户的开户与取密钥
// 共用一个桶（2026-09-11/12 实测 35–60% 的 /api/oauth/* 请求 429，开户拖到
// 30–100 分钟）。用户维度（按 user id）与模型维度的限流不受本机制影响。
//
// 见 docs/custom/rate-limit-bypass.md。
var RateLimitBypassToken = ""
