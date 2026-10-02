package common

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 上游 PD 分离推理通道把节点的内网地址编进了响应 id
// （`chatcmpl-___prefill_addr_<ip>:<port>___decode_addr_<ip>:<port>_<32hex>`），
// 且流式响应的**每个分块**都带。实测 2026-10-02：`deepseek/deepseek-v4-flash`
// （`10.119.x:7100/7101`）与 `z-ai/glm-5.3-flash`（`10.234.x:8998`）都漏，
// `/v1/chat/completions` 与 `/v1/messages` 两个端点都漏，而 claude/qwen 系干净
// （`gen-…` / 裸 hex）。按网段和端口都不同，所以判定条件只能是形状，不能按模型枚举。
//
// 只清洗 `id` 字段，不 scrub 整段正文：模型的回答里出现 IP 地址是合法内容
// （用户问网络问题时就会），按全文替换会把答案改掉。
// 末端的 32 位 hex 本身就是可查单的请求标识，剥掉地址段不影响客服定位。

// leakyUpstreamId 已知的形状。只吃掉地址段，保留前缀（`chatcmpl-`）与末尾的 hex 标识。
//
// ⚠️ 字符类里写 `[0-9.]` 而不是 `[\d.]`：Go 的 RE2 **不会**在字符类里展开 `\d`
// （PCRE 会），`[\d.]` 会被当成字面量 `\`、`d`、`.` 三个字符，于是永远匹配不上——
// 表现出来就是每个 id 都落到下面的 `redacted` 兜底分支。
var leakyUpstreamId = regexp.MustCompile(
	`_{3}prefill_addr_[0-9.]+:[0-9]+_{3}decode_addr_[0-9.]+:[0-9]+_([0-9a-f]{8,})`,
)

// upstreamIdPlaceholder 是形状漂移时的兜底值：正则对不上、但 id 里还留着地址标记，
// 说明上游改了格式——那时宁可给一个占位符，也不要把地址原样发出去。
const upstreamIdPlaceholder = "redacted"

// ScrubUpstreamIds 摘掉一段协议 JSON 里 `id` 字段携带的上游节点地址。
//
// 输入不是合法 JSON、没有 `id`、`id` 不是字符串、或本来就干净时**原样返回**——
// 调用方据此判断有没有改动，避免为没变的载荷重建一遍响应帧。
func ScrubUpstreamIds(data []byte) []byte {
	if !gjson.ValidBytes(data) {
		return data
	}
	id := gjson.GetBytes(data, "id")
	if id.Type != gjson.String {
		return data
	}
	raw := id.String()
	clean := scrubUpstreamId(raw)
	if clean == raw {
		return data
	}
	updated, err := sjson.SetBytes(data, "id", clean)
	if err != nil {
		return data
	}
	return updated
}

// scrubUpstreamId 返回清洗后的 id。没有地址标记就原样返回。
func scrubUpstreamId(id string) string {
	if !strings.Contains(id, "prefill_addr") && !strings.Contains(id, "decode_addr") {
		return id
	}
	clean := leakyUpstreamId.ReplaceAllString(id, "$1")
	if strings.Contains(clean, "prefill_addr") || strings.Contains(clean, "decode_addr") {
		return upstreamIdPlaceholder
	}
	return clean
}
