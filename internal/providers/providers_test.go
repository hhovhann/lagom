package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests run the adapters against fake servers that speak each vendor's
// wire format. They prove request shape and parsing; they do not prove the live
// APIs accept the request (that needs real keys: see README, "Real models").

func call() *Call {
	return &Call{
		MaxTokens: 100,
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hello"},
		},
	}
}

func TestAnthropicNonStreaming(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"hi "},{"type":"text","text":"there"}],
			"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":4}}`))
	}))
	defer srv.Close()

	c := call()
	c.Effort = "low"
	temp := 0.2
	c.Temperature = &temp
	res, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), c, "claude-haiku-4-5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi there" || res.Usage.PromptTokens != 15 || res.Usage.CompletionTokens != 3 {
		t.Fatalf("parsed %+v", res)
	}
	if gotBody["system"] != "be brief" || gotBody["max_tokens"].(float64) != 100 {
		t.Errorf("system/max_tokens wrong: %v", gotBody)
	}
	if _, has := gotBody["temperature"]; has {
		t.Error("temperature must not be sent: newest models reject sampling params")
	}
	if oc, _ := gotBody["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("effort not forwarded: %v", gotBody["output_config"])
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("system role leaked into messages: %v", msgs)
	}
}

func TestAnthropicRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":0}}`))
	}))
	defer srv.Close()
	if _, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), call(), "m", nil); err == nil {
		t.Fatal("refusal should surface as an error so the cascade can escalate")
	}
}

func TestAnthropicStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
	}))
	defer srv.Close()
	var parts []string
	res, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), call(), "m", func(d string) { parts = append(parts, d) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(parts, "|") != "Hel|lo" || res.Text != "Hello" || res.Usage.PromptTokens != 9 || res.Usage.CompletionTokens != 2 {
		t.Fatalf("parts=%v res=%+v", parts, res)
	}
}

func TestAnthropicHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"rate_limit_error"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := NewAnthropic(srv.URL, "k").Chat(context.Background(), call(), "m", nil)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want a 429 error, got %v", err)
	}
}

func TestOpenAIStreamingWithUsage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("auth header missing")
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Par\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"is\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	var n int
	res, err := NewOpenAI(srv.URL+"/v1", "sk", false, "max_completion_tokens").Chat(context.Background(), call(), "gpt", func(string) { n++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Paris" || n != 2 || res.Usage.PromptTokens != 7 || res.Usage.CompletionTokens != 2 {
		t.Fatalf("res=%+v deltas=%d", res, n)
	}
	if _, has := gotBody["max_tokens"]; has || gotBody["max_completion_tokens"].(float64) != 100 {
		t.Errorf("token field wrong: %v", gotBody)
	}
	if so, _ := gotBody["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options.include_usage missing: %v", gotBody)
	}
}

func TestOpenAIMissingUsageIsEstimated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"an answer of some length"}}]}`))
	}))
	defer srv.Close()
	res, err := NewOpenAI(srv.URL, "", false, "max_tokens").Chat(context.Background(), call(), "m", nil)
	if err != nil || res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 {
		t.Fatalf("usage must never be zero (cost accounting): %+v %v", res, err)
	}
}

func TestOpenAIForwardsMetaOnlyWhenEnabled(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Lagom-Mock-Gold")
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	c := call()
	c.Meta = map[string]string{"Gold": "secret-answer"}
	NewOpenAI(srv.URL, "", false, "").Chat(context.Background(), c, "m", nil)
	if got != "" {
		t.Fatal("simulator hints leaked to a non-mock provider")
	}
	NewOpenAI(srv.URL, "", true, "").Chat(context.Background(), c, "m", nil)
	if got != "secret-answer" {
		t.Fatal("simulator hints not forwarded to the mock provider")
	}
}
