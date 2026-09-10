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
func WrapResponseModelWriter(c *gin.Context, requestModel string, format types.RelayFormat) func() {
	setting, _ := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
	if !setting.ResponseModelName || requestModel == "" || format == types.RelayFormatOpenAIRealtime {
		return func() {}
	}
	original := c.Writer
	writer := &responseModelWriter{
		ResponseWriter: original,
		rewriter:       NewResponseModelRewriter(requestModel, format),
	}
	c.Writer = writer
	return func() {
		if err := writer.finish(); err != nil {
			logger.LogWarn(c, "failed to finish response model rewrite: "+err.Error())
		}
		c.Writer = original
	}
}

// responseModelWriter handles all HTTP adapter output, including direct c.JSON
// and c.Writer writes. SSE buffering ends at each event, never at stream EOF.
type responseModelWriter struct {
	gin.ResponseWriter
	rewriter *ResponseModelRewriter
	mu       sync.Mutex
	pending  []byte
	mode     string
}

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
		_, err := w.ResponseWriter.Write(w.rewriter.Rewrite(body))
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
	updated := w.rewriter.Rewrite(payload)
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
