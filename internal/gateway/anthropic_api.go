package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"lagom/internal/providers"
)

// POST /v1/messages speaks Anthropic's Messages wire format, so an app that uses the
// Anthropic SDK can switch by changing its base URL. The request is translated to the
// internal chat request and handled by the same path as /v1/chat/completions, so it gets
// the same routing, verification, caching, guard and spend limit.
//
// Limits, stated plainly: text only (no images or tool use) and no streaming yet. A model
// name the gateway does not know is treated as "auto": the point of the endpoint is that
// the app keeps its existing model name while Lagom chooses which model answers.

type anthropicReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system"`
	Messages    []anthropicMsg  `json:"messages"`
	Temperature *float64        `json:"temperature"`
	Stream      bool            `json:"stream"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// textOf accepts a plain string or an array of text blocks; any other block is rejected.
func textOf(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type != "text" {
			return "", false
		}
		b.WriteString(bl.Text)
	}
	return b.String(), true
}

func anthropicErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
}

// capture is a minimal in-memory http.ResponseWriter.
type capture struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) Write(b []byte) (int, error) { return c.buf.Write(b) }
func (c *capture) WriteHeader(code int)        { c.code = code }

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var in anthropicReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
		anthropicErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	if in.Stream {
		anthropicErr(w, http.StatusBadRequest, "invalid_request_error", "streaming is not supported on /v1/messages yet; use /v1/chat/completions")
		return
	}
	var msgs []providers.Message
	if sys, ok := textOf(in.System); !ok {
		anthropicErr(w, http.StatusBadRequest, "invalid_request_error", "only text is supported in system")
		return
	} else if sys != "" {
		msgs = append(msgs, providers.Message{Role: "system", Content: providers.Content(sys)})
	}
	for _, m := range in.Messages {
		text, ok := textOf(m.Content)
		if !ok {
			anthropicErr(w, http.StatusBadRequest, "invalid_request_error", "only text content blocks are supported")
			return
		}
		msgs = append(msgs, providers.Message{Role: m.Role, Content: providers.Content(text)})
	}
	model := in.Model
	if _, known := s.models[model]; !known {
		model = "auto"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "max_tokens": in.MaxTokens, "temperature": in.Temperature})

	req := r.Clone(r.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	rec := &capture{h: http.Header{}, code: http.StatusOK}
	s.handleChat(rec, req)

	if rec.code != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		json.Unmarshal(rec.buf.Bytes(), &e)
		kind := "api_error"
		switch {
		case rec.code == http.StatusUnauthorized:
			kind = "authentication_error"
		case rec.code == http.StatusTooManyRequests:
			kind = "rate_limit_error"
		case rec.code >= 400 && rec.code < 500:
			kind = "invalid_request_error"
		}
		anthropicErr(w, rec.code, kind, e.Error.Message)
		return
	}
	var out struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Lagom json.RawMessage `json:"lagom"`
	}
	if err := json.Unmarshal(rec.buf.Bytes(), &out); err != nil || len(out.Choices) == 0 {
		anthropicErr(w, http.StatusBadGateway, "api_error", "unreadable upstream result")
		return
	}
	for k, v := range rec.h {
		w.Header()[k] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": out.ID, "type": "message", "role": "assistant", "model": out.Model,
		"content":     []map[string]string{{"type": "text", "text": out.Choices[0].Message.Content}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": out.Usage.Prompt, "output_tokens": out.Usage.Completion},
		"lagom": out.Lagom,
	})
}
