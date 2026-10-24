package common

// FreeModelRateLimit* 控制「免费模型」这一维度的限流：只对模型价格/倍率为 0
// 的请求生效，付费模型不受影响。
//
// 背景：ModelRequestRateLimit 按用户分组配置（y-api / y-api-boost），y-api-paid
// 不在其中，走全局默认值（不限总量、成功数上限 1 亿），于是付费用户对免费模型
// 没有任何约束——2026-10-24 实测单个 y-api-paid 用户一天调用 21,464 次免费模型，
// 占全平台免费模型流量 60% 以上，同时把 Redis 的 MRRLS 列表撑到 7 万条（上限 1 亿
// 让 LTrim 完全失效，列表无限增长）。
//
// 本机制独立于 ModelRequestRateLimit，用环境变量配置，默认关闭：
//
//   - FREE_MODEL_RATE_LIMIT_ENABLED      开关，默认 false
//
//   - FREE_MODEL_RATE_LIMIT_GROUPS       生效的分组，逗号分隔，如 "y-api-paid"；
//     为空表示对所有分组生效
//
//   - FREE_MODEL_RATE_LIMIT_COUNT        窗口内允许的免费模型请求数，默认 0（不限）
//
//   - FREE_MODEL_RATE_LIMIT_WINDOW_MIN   窗口长度（分钟），默认 1440（24 小时）
//
//   - FREE_MODEL_RATE_LIMIT_CONCURRENCY  单个用户在途免费模型请求数上限，默认 0（不限）
//
// 计数口径是「请求数」而非「成功数」：失败的请求同样占用额度，避免客户端用
// 重试把上游打穿。
var (
	FreeModelRateLimitEnabled     = false
	FreeModelRateLimitGroups      = ""
	FreeModelRateLimitCount       = 0
	FreeModelRateLimitWindowMin   = 1440
	FreeModelRateLimitConcurrency = 0
)
