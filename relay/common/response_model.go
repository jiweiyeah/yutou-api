package common

import (
	"bytes"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResponseModelRewriter changes only protocol model identifiers. It never mutates
// the upstream bytes, which may still be needed for usage and billing.
type ResponseModelRewriter struct {
	modelJSON []byte
	format    types.RelayFormat
}

func NewResponseModelRewriter(model string, format types.RelayFormat) *ResponseModelRewriter {
	modelJSON, _ := common.Marshal(model)
	return &ResponseModelRewriter{modelJSON: modelJSON, format: format}
}

func (r *ResponseModelRewriter) Rewrite(data []byte) []byte {
	if !gjson.ValidBytes(data) {
		return data
	}
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return data
	}
	eventType := gjson.GetBytes(data, "type").String()
	if eventType == "error" {
		return data
	}
	if upstreamError := gjson.GetBytes(data, "error"); upstreamError.Exists() && upstreamError.Type != gjson.Null {
		return data
	}
	paths := []string{"model"}
	switch r.format {
	case types.RelayFormatClaude:
		if eventType == "message_start" {
			paths = append(paths, "message.model")
		}
	case types.RelayFormatOpenAIResponses, types.RelayFormatOpenAIResponsesCompaction:
		if strings.HasPrefix(eventType, "response.") {
			paths = append(paths, "response.model")
		}
	case types.RelayFormatGemini:
		paths = append(paths, "modelVersion")
	case types.RelayFormatOpenAIRealtime:
		paths = append(paths, "session.model", "response.model")
	}
	for _, path := range paths {
		value := gjson.GetBytes(data, path)
		if value.Type != gjson.String || value.Raw == string(r.modelJSON) {
			continue
		}
		updated, err := sjson.SetRawBytes(data, path, r.modelJSON)
		if err == nil {
			data = updated
		}
	}
	return data
}

// WrapResponseModelWriter is called per relay attempt so a retry uses the
// selected channel's setting. Disabled channels keep their original writer.
// requestModel must be captured before mapping or billing suffix normalization.
//
// ===== CUSTOM START: 上游 id 里的节点地址无条件清洗（见 response_id.go） =====
// 两个改写共用一个 writer，但触发条件不同：
//   - 模型名改写只在渠道开了 `response_model_name` 时生效（`rewriter` 为 nil 即关闭）；
//   - 上游 `id` 里的节点地址清洗**无条件生效**——它跟那个渠道开关无关，关掉开关的
//     渠道一样会把上游内网地址发给调用方。所以 writer 一律装上。
//
// Realtime 不包：那条链路不写 `c.Writer`，上游消息由 `relay_realtime.go` 自己逐条
// 处理（那里另外调 `ScrubUpstreamIds`）；而且连接已被 hijack，一个按 Content-Type
// 猜模式、按 SSE 事件攒帧的 writer 套在它上面没有意义。
func WrapResponseModelWriter(c *gin.Context, requestModel string, format types.RelayFormat) func() {
	if format == types.RelayFormatOpenAIRealtime {
		return func() {}
	}

	var rewriter *ResponseModelRewriter
	if setting, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting); setting.ResponseModelName && requestModel != "" {
		rewriter = NewResponseModelRewriter(requestModel, format)
	}
	// ===== CUSTOM END =====

	original := c.Writer
	writer := &responseModelWriter{
		ResponseWriter: original,
		rewriter:       rewriter,
	}
	c.Writer = writer
	return func() {
		if err := writer.finish(); err != nil {
			logger.LogWarn(c, "failed to finish response rewrite: "+err.Error())
		}
		c.Writer = original
	}
}

// responseModelWriter handles all HTTP adapter output, including direct c.JSON
// and c.Writer writes. SSE buffering ends at each event, never at stream EOF.
//
// ===== CUSTOM START: 多一路 id 清洗（`rewriter` 可以为 nil，清洗那一路永远在跑） =====
// ===== CUSTOM END =====
type responseModelWriter struct {
	gin.ResponseWriter
	rewriter *ResponseModelRewriter
	mu       sync.Mutex
	pending  []byte
	mode     string
}

// ===== CUSTOM START: id 清洗与模型名改写共用一条改写路径 =====
// rewritePayload 依次过 id 清洗与模型名改写。两者改的是不同字段，顺序无关；
// 区别在于清洗无条件执行，改写器只在渠道开了 `response_model_name` 时存在。
func (w *responseModelWriter) rewritePayload(payload []byte) []byte {
	updated := ScrubUpstreamIds(payload)
	if w.rewriter != nil {
		updated = w.rewriter.Rewrite(updated)
	}
	return updated
}

// ===== CUSTOM END =====

func (w *responseModelWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(data) == 0 {
		return 0, nil
	}
	if w.mode == "" {
		contentType := w.Header().Get("Content-Type")
		encoding := w.Header().Get("Content-Encoding")
		switch {
		case w.Status() < 200 || w.Status() >= 300 || (encoding != "" && encoding != "identity"):
			// CUSTOM: 非 2xx 与压缩过的响应原样透传，**不改写**：解压再压回去的成本与
			// 风险都不值得，而且错误体里本来也没有协议 id。代价是上游若给 200 的响应
			// 加了 Content-Encoding，那一份不会被清洗——中继不应主动向上游请求压缩。
			w.mode = "passthrough"
		case strings.HasPrefix(contentType, "text/event-stream"):
			w.mode = "sse"
		case strings.Contains(contentType, "json") || (contentType == "" && bytes.HasPrefix(bytes.TrimSpace(data), []byte("{"))):
			w.mode = "json"
		default:
			w.mode = "passthrough"
		}
	}
	if w.mode == "passthrough" {
		return w.ResponseWriter.Write(data)
	}
	// The rewritten body may have a different length, including when headers
	// were supplied by an upstream provider. net/http selects the framing.
	w.Header().Del("Content-Length")
	if w.mode == "json" {
		body := data
		if len(w.pending) > 0 || !gjson.ValidBytes(data) {
			w.pending = append(w.pending, data...)
			body = w.pending
			if !gjson.ValidBytes(body) {
				return len(data), nil
			}
		}
		_, err := w.ResponseWriter.Write(w.rewritePayload(body)) // CUSTOM: 兼做 id 清洗
		w.pending = nil
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
	w.pending = append(w.pending, data...)
	for {
		end, delimiter := bytes.Index(w.pending, []byte("\n\n")), 2
		if crlf := bytes.Index(w.pending, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
			end, delimiter = crlf, 4
		}
		if end < 0 {
			break
		}
		event := w.pending[:end+delimiter]
		if _, err := w.ResponseWriter.Write(w.rewriteEvent(event)); err != nil {
			return 0, err
		}
		w.pending = w.pending[end+delimiter:]
	}
	if len(w.pending) == 0 {
		w.pending = nil
	}
	return len(data), nil
}

func (w *responseModelWriter) rewriteEvent(event []byte) []byte {
	lines := bytes.Split(event, []byte("\n"))
	var payload []byte
	dataLines := 0
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if dataLines > 0 {
			payload = append(payload, '\n')
		}
		value := bytes.TrimSuffix(line[5:], []byte("\r"))
		value = bytes.TrimPrefix(value, []byte(" "))
		payload = append(payload, value...)
		dataLines++
	}
	if dataLines == 0 {
		return event
	}
	updated := w.rewritePayload(payload) // CUSTOM: 兼做 id 清洗
	if bytes.Equal(updated, payload) {
		return event
	}
	var output bytes.Buffer
	written := false
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if written {
				continue
			}
			ending := "\n"
			if bytes.HasSuffix(line, []byte("\r")) {
				ending = "\r\n"
			}
			for _, part := range bytes.Split(updated, []byte("\n")) {
				output.WriteString("data: ")
				output.Write(part)
				output.WriteString(ending)
			}
			written = true
			continue
		}
		output.Write(line)
		if i < len(lines)-1 {
			output.WriteByte('\n')
		}
	}
	return output.Bytes()
}

func (w *responseModelWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *responseModelWriter) WriteHeaderNow() {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeaderNow()
}

func (w *responseModelWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Header().Del("Content-Length")
	w.ResponseWriter.Flush()
}

func (w *responseModelWriter) finish() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	// Preserve incomplete/malformed upstream output instead of dropping bytes.
	_, err := w.ResponseWriter.Write(w.pending)
	w.pending = nil
	return err
}

var _ http.Flusher = (*responseModelWriter)(nil)
