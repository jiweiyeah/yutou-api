package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newResponsesStreamContext builds the minimum relay context a responses stream
// handler needs. Usage is only reported in the final response.completed event,
// so a client that disconnects early leaves nothing to fall back on and the
// local estimate has to cover it.
func newResponsesStreamContext(t *testing.T, body string, estimatePromptTokens int) (*gin.Context, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
		IsStream:    true,
	}
	info.UpstreamModelName = "test-model"
	info.SetEstimatePromptTokens(estimatePromptTokens)
	return c, resp, info
}

func TestOaiResponsesStreamHandlerEstimatesUsageFromReasoningDeltas(t *testing.T) {
	// No response.completed event: this is the stream shape left behind when the
	// client goes away before the upstream reports usage.
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"test-model"}}`,
		``,
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","delta":"thinking about the question"}`,
		``,
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","delta":" and then answering it"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	c, resp, info := newResponsesStreamContext(t, body, 11)

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Greater(t, usage.CompletionTokens, 0,
		"reasoning deltas must count toward completion tokens, otherwise a truncated stream is billed as free")
	require.Equal(t, 11, usage.PromptTokens)
	require.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
}

func TestOaiResponsesStreamHandlerCountsOutputTextDeltas(t *testing.T) {
	body := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello there"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	c, resp, info := newResponsesStreamContext(t, body, 7)

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Greater(t, usage.CompletionTokens, 0)
	require.Equal(t, 7, usage.PromptTokens)
}

func TestOaiResponsesStreamHandlerBillsPromptWhenNothingArrived(t *testing.T) {
	// The client went away before any delta and before response.completed, so
	// there is no upstream usage and no generated text to estimate from. The
	// prompt was still processed upstream, and the chat path bills prompt tokens
	// unconditionally (ResponseText2Usage), so this path must not leave the whole
	// request unbilled.
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"test-model"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	c, resp, info := newResponsesStreamContext(t, body, 13)

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Equal(t, 13, usage.PromptTokens, "prompt tokens must be billed even when no output arrived")
	require.Equal(t, 0, usage.CompletionTokens)
	require.Equal(t, 13, usage.TotalTokens)
}

func TestOaiResponsesStreamHandlerPrefersUpstreamUsage(t *testing.T) {
	body := strings.Join([]string{
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","delta":"some reasoning that must not win"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"test-model","usage":{"input_tokens":21,"output_tokens":34,"total_tokens":55}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	c, resp, info := newResponsesStreamContext(t, body, 11)

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Equal(t, 21, usage.PromptTokens)
	require.Equal(t, 34, usage.CompletionTokens)
	require.Equal(t, 55, usage.TotalTokens)
}
