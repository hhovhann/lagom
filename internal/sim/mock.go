// Package mockllm is an OpenAI-compatible LLM SIMULATOR.
//
// It exists so the gateway can be benchmarked and demonstrated without API keys
// or network noise. It is not a model: it is told the right answer through
// X-Lagom-Mock-* headers (forwarded only to providers configured with
// forward_meta) and decides, deterministically per (model, prompt), whether the
// "model" gets it right, according to a per-task success probability.
//
// Everything produced with it is a simulation of the mechanism, not evidence of
// real-world savings. Real numbers come from the real-model config.
package sim

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"
)

type profile struct {
	ttft, perTok time.Duration
	p            map[string]float64
	verb         map[string]int // extra "thinking" output tokens by task
}

const defaultP = 0.80

var profiles = map[string]profile{
	// zero-latency, always-correct: used by the overhead benchmark
	"mock-instant": {p: map[string]float64{"*": 1}},
	"mock-local": {ttft: 60 * time.Millisecond, perTok: 14 * time.Millisecond,
		p:    map[string]float64{"classify": .90, "extract": .55, "reason": .12, "faq": .95, "grounded": .80, "summary": .35, "general": .60},
		verb: map[string]int{"reason": 40}},
	"mock-small": {ttft: 150 * time.Millisecond, perTok: 8 * time.Millisecond,
		p:    map[string]float64{"classify": .94, "extract": .72, "reason": .30, "faq": .97, "grounded": .88, "summary": .55, "general": .75},
		verb: map[string]int{"reason": 70}},
	"mock-medium": {ttft: 300 * time.Millisecond, perTok: 12 * time.Millisecond,
		p:    map[string]float64{"classify": .97, "extract": .94, "reason": .72, "faq": .98, "grounded": .96, "summary": .82, "general": .90},
		verb: map[string]int{"reason": 140}},
	"mock-large": {ttft: 600 * time.Millisecond, perTok: 20 * time.Millisecond,
		p:    map[string]float64{"classify": .99, "extract": .98, "reason": .95, "faq": .99, "grounded": .99, "summary": .96, "general": .97},
		verb: map[string]int{"reason": 260}},
}

type MockServer struct {
	// LatencyScale multiplies every simulated delay (0 disables sleeping).
	LatencyScale float64
}

func (s *MockServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /v1/embeddings", s.embed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func text(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func (s *MockServer) sleep(r *http.Request, d time.Duration) bool {
	d = time.Duration(float64(d) * s.LatencyScale)
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func (s *MockServer) chat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
		return
	}
	pr, ok := profiles[req.Model]
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"unknown mock model %q"}}`, req.Model), http.StatusNotFound)
		return
	}
	task := r.Header.Get("X-Lagom-Mock-Task")
	if task == "" {
		task = "general"
	}
	var prompt strings.Builder
	for _, m := range req.Messages {
		prompt.WriteString(text(m.Content))
		prompt.WriteByte('\n')
	}

	p, ok := pr.p[task]
	if !ok {
		if p, ok = pr.p["*"]; !ok {
			p = defaultP
		}
	}
	h := fnv.New64a()
	h.Write([]byte(req.Model))
	h.Write([]byte(prompt.String()))
	good := float64(h.Sum64()%10000)/10000 < p

	out := r.Header.Get("X-Lagom-Mock-Gold")
	if out == "" {
		out = "OK"
	}
	if !good {
		out = r.Header.Get("X-Lagom-Mock-Wrong")
		if out == "" {
			out = "I'm not sure."
		}
	}
	inTok := prompt.Len() / 4
	if inTok < 1 {
		inTok = 1
	}
	outTok := len(out)/4 + pr.verb[task]
	if outTok < 1 {
		outTok = 1
	}
	decode := time.Duration(outTok) * pr.perTok

	if !req.Stream {
		if !s.sleep(r, pr.ttft+decode) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "mock-1", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": out}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	if !s.sleep(r, pr.ttft) {
		return
	}
	pieces := strings.SplitAfter(out, " ")
	gap := decode / time.Duration(len(pieces))
	for _, piece := range pieces {
		if piece == "" {
			continue
		}
		b, _ := json.Marshal(map[string]any{
			"id": "mock-1", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": piece}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
		if !s.sleep(r, gap) {
			return
		}
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		b, _ := json.Marshal(map[string]any{
			"id": "mock-1", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{},
			"usage": map[string]int{"prompt_tokens": inTok, "completion_tokens": outTok, "total_tokens": inTok + outTok},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}

// embed returns a deterministic hashed bag-of-words vector: texts that share words are
// similar. It is a stand-in so the offline demo can classify prompts; real embeddings
// come from a real embedding model.
func (s *MockServer) embed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	v := make([]float64, 1024)
	for _, word := range strings.Fields(strings.ToLower(in.Input)) {
		h := fnv.New32a()
		h.Write([]byte(strings.Trim(word, ".,:;!?\"'()")))
		v[h.Sum32()%1024]++
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v}}})
}
