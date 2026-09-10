package common

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseModelProtocols(t *testing.T) {
	const alias = "openai/gpt-6-astra"
	for _, tc := range []struct {
		name   string
		format types.RelayFormat
		body   string
		want   string
	}{
		{"chat", types.RelayFormatOpenAI,
			`{"model":"upstream","choices":[{"message":{"content":"upstream","model":"tool-data"}}],"usage":{"total_tokens":15},"vendor_field":1}`,
			`{"model":"openai/gpt-6-astra","choices":[{"message":{"content":"upstream","model":"tool-data"}}],"usage":{"total_tokens":15},"vendor_field":1}`},
		{"messages", types.RelayFormatClaude,
			`{"type":"message","model":"upstream","content":[{"type":"text","text":"upstream"}]}`,
			`{"type":"message","model":"openai/gpt-6-astra","content":[{"type":"text","text":"upstream"}]}`},
		{"message_start", types.RelayFormatClaude,
			`{"type":"message_start","message":{"model":"upstream","usage":{"input_tokens":11}}}`,
			`{"type":"message_start","message":{"model":"openai/gpt-6-astra","usage":{"input_tokens":11}}}`},
		{"responses", types.RelayFormatOpenAIResponses,
			`{"object":"response","model":"upstream","output":[],"metadata":{"model":"keep"}}`,
			`{"object":"response","model":"openai/gpt-6-astra","output":[],"metadata":{"model":"keep"}}`},
		{"response_completed", types.RelayFormatOpenAIResponses,
			`{"type":"response.completed","response":{"model":"upstream","usage":{"output_tokens":4}}}`,
			`{"type":"response.completed","response":{"model":"openai/gpt-6-astra","usage":{"output_tokens":4}}}`},
		{"compact", types.RelayFormatOpenAIResponsesCompaction,
			`{"object":"response.compaction","model":"upstream","output":[]}`,
			`{"object":"response.compaction","model":"openai/gpt-6-astra","output":[]}`},
		{"embedding", types.RelayFormatEmbedding,
			`{"model":"upstream","data":[{"embedding":[0.1,0.2]}]}`,
			`{"model":"openai/gpt-6-astra","data":[{"embedding":[0.1,0.2]}]}`},
		{"gemini", types.RelayFormatGemini,
			`{"modelVersion":"gemini-version","candidates":[],"usageMetadata":{"totalTokenCount":15}}`,
			`{"modelVersion":"openai/gpt-6-astra","candidates":[],"usageMetadata":{"totalTokenCount":15}}`},
		{"realtime_session", types.RelayFormatOpenAIRealtime,
			`{"type":"session.created","session":{"model":"upstream","voice":"alloy"}}`,
			`{"type":"session.created","session":{"model":"openai/gpt-6-astra","voice":"alloy"}}`},
		{"realtime_response", types.RelayFormatOpenAIRealtime,
			`{"type":"response.done","response":{"model":"upstream"}}`,
			`{"type":"response.done","response":{"model":"openai/gpt-6-astra"}}`},
		{"no_model", types.RelayFormatOpenAIImage,
			`{"data":[{"url":"https://example.com/upstream.png"}]}`,
			`{"data":[{"url":"https://example.com/upstream.png"}]}`},
		{"tool_input", types.RelayFormatClaude,
			`{"type":"content_block_delta","delta":{"partial_json":"{\"model\":\"upstream\"}"}}`,
			`{"type":"content_block_delta","delta":{"partial_json":"{\"model\":\"upstream\"}"}}`},
		{"unrelated_nested_model", types.RelayFormatOpenAIResponses,
			`{"type":"response.output_item.added","item":{"model":"keep"}}`,
			`{"type":"response.output_item.added","item":{"model":"keep"}}`},
		{"error", types.RelayFormatOpenAI,
			`{"error":{"message":"unavailable"},"model":"upstream"}`,
			`{"error":{"message":"unavailable"},"model":"upstream"}`},
		{"malformed", types.RelayFormatOpenAI, `{"model":"upstream"`, `{"model":"upstream"`},
		{"null_model", types.RelayFormatOpenAI, `{"model":null}`, `{"model":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			result := NewResponseModelRewriter(alias, tc.format).Rewrite(body)
			assert.Equal(t, tc.want, string(result))
			assert.Equal(t, tc.body, string(body), "upstream data must remain usable by billing")
		})
	}
	t.Run("escaped_client_name", func(t *testing.T) {
		result := NewResponseModelRewriter("alias/\"模型\"\n", types.RelayFormatOpenAI).Rewrite([]byte(`{"model":"upstream"}`))
		assert.JSONEq(t, `{"model":"alias/\"模型\"\n"}`, string(result))
	})
}

func TestResponseModelWriterJSON(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				write func(*testing.T, *gin.Context)
			}{
				{"gin_json", func(t *testing.T, c *gin.Context) {
					c.JSON(http.StatusOK, gin.H{"model": "upstream", "usage": gin.H{"total_tokens": 15}})
				}},
				{"fragmented_body", func(t *testing.T, c *gin.Context) {
					c.Header("Content-Type", "application/json")
					for _, part := range []string{`{"mo`, `del":"upstream",`, `"usage":{"total_tokens":15}}`} {
						n, err := c.Writer.WriteString(part)
						require.NoError(t, err)
						assert.Equal(t, len(part), n)
					}
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: enabled})
					finish := WrapResponseModelWriter(c, "public", types.RelayFormatOpenAI)
					tc.write(t, c)
					finish()
					model := "upstream"
					if enabled {
						model = "public"
					}
					assert.JSONEq(t, fmt.Sprintf(`{"model":%q,"usage":{"total_tokens":15}}`, model), recorder.Body.String())
				})
			}
		})
	}
}

func TestResponseModelWriterSSE(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format types.RelayFormat
		event  string
		want   string
	}{
		{"chat", types.RelayFormatOpenAI, "data: {\"model\":\"upstream\",\"choices\":[]}\n\n", "data: {\"model\":\"public\",\"choices\":[]}\n\n"},
		{"messages", types.RelayFormatClaude, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"upstream\"}}\n\n", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"public\"}}\n\n"},
		{"responses_crlf", types.RelayFormatOpenAIResponses, "id: 1\r\nevent: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"upstream\"}}\r\n\r\n", "id: 1\r\nevent: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"public\"}}\r\n\r\n"},
		{"multiline", types.RelayFormatOpenAI, "data: {\"model\":\ndata: \"upstream\"}\n\n", "data: {\"model\":\ndata: \"public\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: true})
			finish := WrapResponseModelWriter(c, "public", tc.format)
			c.Header("Content-Type", "text/event-stream")
			// CustomEvent and io.Copy may split a frame across writes.
			for _, part := range []string{tc.event[:7], tc.event[7 : len(tc.event)-1], tc.event[len(tc.event)-1:]} {
				_, err := c.Writer.WriteString(part)
				require.NoError(t, err)
				c.Writer.Flush()
			}
			assert.Equal(t, tc.want, recorder.Body.String(), "event must reach the client before the stream finishes")
			_, err := c.Writer.WriteString(": PING\n\ndata: [DONE]\n\n")
			require.NoError(t, err)
			finish()
			assert.Equal(t, tc.want+": PING\n\ndata: [DONE]\n\n", recorder.Body.String())
		})
	}
}

func TestResponseModelWriterPreservesOtherBodies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		encoding    string
		body        string
	}{
		{"error", 400, "application/json", "", `{"model":"upstream","error":"bad request"}`},
		{"audio", 200, "audio/mpeg", "", "\x00\x01{\"model\":\"upstream\"}"},
		{"compressed", 200, "application/json", "gzip", "\x1f\x8b\x08\x00"},
		{"malformed", 200, "application/json", "", `{"model":"upstream"`},
		{"unfinished_event", 200, "text/event-stream", "", "data: unfinished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: true})
			finish := WrapResponseModelWriter(c, "public", types.RelayFormatOpenAI)
			c.Header("Content-Encoding", tc.encoding)
			c.Data(tc.status, tc.contentType, []byte(tc.body))
			finish()
			assert.Equal(t, tc.status, recorder.Code)
			assert.Equal(t, tc.body, recorder.Body.String())
		})
	}
}

func TestResponseModelHTTPFramingAndChannelRetry(t *testing.T) {
	engine := gin.New()
	engine.GET("/", func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: true})
		finish := WrapResponseModelWriter(c, "public", types.RelayFormatOpenAI)
		finish() // An unsuccessful attempt wrote nothing; select another channel.
		common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponseModelName: c.Query("enabled") == "true"})
		finish = WrapResponseModelWriter(c, "public", types.RelayFormatOpenAI)
		defer finish()
		body := `{"model":"a-much-longer-upstream-model-name"}`
		c.Header("Content-Length", fmt.Sprint(len(body)))
		c.Header("Content-Type", "application/json")
		c.Writer.WriteHeaderNow()
		_, _ = io.Copy(c.Writer, strings.NewReader(body))
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	for _, enabled := range []bool{false, true} {
		resp, err := http.Get(fmt.Sprintf("%s/?enabled=%t", server.URL, enabled))
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err, "rewriting must not leave a stale Content-Length")
		want := `{"model":"a-much-longer-upstream-model-name"}`
		if enabled {
			want = `{"model":"public"}`
		}
		assert.JSONEq(t, want, string(body))
	}
}
