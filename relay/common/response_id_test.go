package common

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这条 fixture 的形状是 2026-10-02 从生产 `/v1/chat/completions` 抓到的真响应
// （deepseek/deepseek-v4-flash），只删了与清洗无关的字段。
const leakyChatId = "chatcmpl-___prefill_addr_10.119.12.170:7101___decode_addr_10.119.12.175:7100_f3f6c0dbb5664dbd92a4db0213809c3b"

const scrubbedChatId = "chatcmpl-f3f6c0dbb5664dbd92a4db0213809c3b"

func TestScrubUpstreamIds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		// want 不是合法 JSON 时逐字节比较，别走 JSONEq（它解不了这种输入）。
		raw bool
	}{
		{
			name: "known_shape_keeps_prefix_and_request_hex",
			in:   `{"id":"` + leakyChatId + `","model":"deepseek/deepseek-v4-flash","choices":[]}`,
			want: `{"id":"` + scrubbedChatId + `","model":"deepseek/deepseek-v4-flash","choices":[]}`,
		},
		{
			// 上游换了前缀（不再是 chatcmpl-）时，前缀照原样留着，只摘地址段。
			name: "known_shape_without_known_prefix",
			in:   `{"id":"gen-___prefill_addr_10.234.1.7:8998___decode_addr_10.234.1.9:8998_94c5601d500642a3a2790f3e91213d2b"}`,
			want: `{"id":"gen-94c5601d500642a3a2790f3e91213d2b"}`,
		},
		{
			// 形状漂移：正则对不上，但 id 里还留着地址标记 ⇒ 整个值都不该发出去。
			name: "unknown_shape_with_address_marker_is_replaced_wholesale",
			in:   `{"id":"chatcmpl-___prefill_addr_10.119.12.170_extra"}`,
			want: `{"id":"redacted"}`,
		},
		{
			name: "clean_id_is_untouched",
			in:   `{"id":"gen-1790935486-AbCdEf","model":"qwen/qwen3.8-flash"}`,
			want: `{"id":"gen-1790935486-AbCdEf","model":"qwen/qwen3.8-flash"}`,
		},
		{
			name: "bare_hex_id_is_untouched",
			in:   `{"id":"f3f6c0dbb5664dbd92a4db0213809c3b"}`,
			want: `{"id":"f3f6c0dbb5664dbd92a4db0213809c3b"}`,
		},
		{
			name: "missing_id_is_untouched",
			in:   `{"model":"deepseek/deepseek-v4-flash","choices":[]}`,
			want: `{"model":"deepseek/deepseek-v4-flash","choices":[]}`,
		},
		{
			name: "non_string_id_is_untouched",
			in:   `{"id":42,"model":"deepseek/deepseek-v4-flash"}`,
			want: `{"id":42,"model":"deepseek/deepseek-v4-flash"}`,
		},
		{
			// 模型回答里出现 IP 是合法内容（用户问网络问题就会），不许动正文。
			name: "address_inside_model_output_is_preserved",
			in:   `{"id":"chatcmpl-abc","choices":[{"message":{"content":"the node is at 10.119.12.170:7101"}}]}`,
			want: `{"id":"chatcmpl-abc","choices":[{"message":{"content":"the node is at 10.119.12.170:7101"}}]}`,
		},
		{
			name: "invalid_json_is_untouched",
			in:   `{"id":"chatcmpl-___prefill_addr_10.0.0.1:1`,
			want: `{"id":"chatcmpl-___prefill_addr_10.0.0.1:1`,
			raw:  true,
		},
		{
			name: "empty_input_is_untouched",
			in:   ``,
			want: ``,
			raw:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(ScrubUpstreamIds([]byte(tc.in)))
			if tc.raw {
				assert.Equal(t, tc.want, got)
				return
			}
			assert.JSONEq(t, tc.want, got)
		})
	}
}

// 清洗与「改写模型名」那个渠道开关无关：关掉开关的渠道一样会把上游内网地址
// 发给调用方，所以这一条锁的是**开关为 false 时也必须清洗**。
func TestResponseIdScrubbedRegardlessOfChannelSetting(t *testing.T) {
	for _, responseModelName := range []bool{false, true} {
		t.Run(fmt.Sprintf("response_model_name=%t", responseModelName), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: responseModelName})

			finish := WrapResponseModelWriter(c, "public/model", types.RelayFormatOpenAI)
			c.JSON(http.StatusOK, gin.H{
				"id":      leakyChatId,
				"model":   "deepseek/deepseek-v4-flash",
				"choices": []any{},
			})
			finish()

			body := recorder.Body.String()
			assert.NotContains(t, body, "prefill_addr")
			assert.NotContains(t, body, "decode_addr")
			assert.NotContains(t, body, "10.119.12.170")

			// 开关只决定模型名要不要改写，不该影响 id。
			wantModel := "deepseek/deepseek-v4-flash"
			if responseModelName {
				wantModel = "public/model"
			}
			assert.JSONEq(t,
				fmt.Sprintf(`{"id":%q,"model":%q,"choices":[]}`, scrubbedChatId, wantModel),
				body)
		})
	}
}

// 流式：每个分块都带同一个泄漏 id，且分块边界可能切在 id 中间。
func TestResponseIdScrubbedInEveryStreamChunk(t *testing.T) {
	event := "data: {\"id\":\"" + leakyChatId + "\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	finish := WrapResponseModelWriter(c, "public/model", types.RelayFormatOpenAI)
	c.Header("Content-Type", "text/event-stream")

	// 按 17 字节切，保证有一段正好落在 id 里。
	for i := 0; i < len(event); i += 17 {
		end := i + 17
		if end > len(event) {
			end = len(event)
		}
		_, err := c.Writer.WriteString(event[i:end])
		require.NoError(t, err)
	}
	_, err := c.Writer.WriteString("data: [DONE]\n\n")
	require.NoError(t, err)
	finish()

	body := recorder.Body.String()
	assert.NotContains(t, body, "prefill_addr")
	assert.NotContains(t, body, "decode_addr")
	assert.Contains(t, body, "data: [DONE]")
	assert.Equal(t,
		"data: {\"id\":\""+scrubbedChatId+"\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
		body)
}
