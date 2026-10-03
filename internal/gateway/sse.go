package gateway

import (
	"net/http"
	"strconv"
	"time"
)

// sseWriter writes OpenAI-style chat.completion.chunk events with minimal
// allocation: one reused buffer, hand-rolled JSON string escaping.
type sseWriter struct {
	w       http.ResponseWriter
	f       http.Flusher
	id      string
	created int64
	started bool
	buf     []byte
	hdr     func(h http.Header, model string)
}

func newSSE(w http.ResponseWriter, id string, hdr func(http.Header, string)) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f, id: id, created: time.Now().Unix(), hdr: hdr, buf: make([]byte, 0, 512)}
}

func (s *sseWriter) begin(model string) {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	if s.hdr != nil {
		s.hdr(h, model)
	}
	s.w.WriteHeader(http.StatusOK)
	s.chunkRaw(model, `{"role":"assistant"}`, "null", "")
}

func (s *sseWriter) delta(model, text string) {
	s.begin(model)
	b := s.buf[:0]
	b = append(b, `{"content":`...)
	b = appendJSONString(b, text)
	b = append(b, '}')
	s.buf = b
	s.chunkRaw(model, string(b), "null", "")
}

func (s *sseWriter) chunkRaw(model, delta, finish, extra string) {
	b := make([]byte, 0, 256+len(delta)+len(extra))
	b = append(b, "data: {\"id\":\""...)
	b = append(b, s.id...)
	b = append(b, "\",\"object\":\"chat.completion.chunk\",\"created\":"...)
	b = strconv.AppendInt(b, s.created, 10)
	b = append(b, ",\"model\":"...)
	b = appendJSONString(b, model)
	b = append(b, ",\"choices\":[{\"index\":0,\"delta\":"...)
	b = append(b, delta...)
	b = append(b, ",\"finish_reason\":"...)
	b = append(b, finish...)
	b = append(b, '}', ']')
	b = append(b, extra...)
	b = append(b, "}\n\n"...)
	s.w.Write(b)
	if s.f != nil {
		s.f.Flush()
	}
}

// finish ends the stream with a usage chunk (including the lagom block) and [DONE].
func (s *sseWriter) finish(model string, in, out int, tdJSON string) {
	s.begin(model)
	s.chunkRaw(model, "{}", `"stop"`, "")
	b := make([]byte, 0, 256+len(tdJSON))
	b = append(b, "data: {\"id\":\""...)
	b = append(b, s.id...)
	b = append(b, "\",\"object\":\"chat.completion.chunk\",\"created\":"...)
	b = strconv.AppendInt(b, s.created, 10)
	b = append(b, ",\"model\":"...)
	b = appendJSONString(b, model)
	b = append(b, ",\"choices\":[],\"usage\":{\"prompt_tokens\":"...)
	b = strconv.AppendInt(b, int64(in), 10)
	b = append(b, ",\"completion_tokens\":"...)
	b = strconv.AppendInt(b, int64(out), 10)
	b = append(b, ",\"total_tokens\":"...)
	b = strconv.AppendInt(b, int64(in+out), 10)
	b = append(b, "},\"lagom\":"...)
	b = append(b, tdJSON...)
	b = append(b, "}\n\ndata: [DONE]\n\n"...)
	s.w.Write(b)
	if s.f != nil {
		s.f.Flush()
	}
}

const hexd = "0123456789abcdef"

// appendJSONString appends s as a JSON string literal.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}
