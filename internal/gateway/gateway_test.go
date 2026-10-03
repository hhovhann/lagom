package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lagom/internal/config"
	"lagom/internal/sim"
)

func testServer(t *testing.T) (*httptest.Server, *Server) { return testServerWith(t, nil) }

func testServerWith(t *testing.T, mod func(*config.Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := baseConfig(t)
	if mod != nil {
		mod(cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(func() { gw.Close(); s.Close() })
	return gw, s
}

// baseConfig returns a working three-model config wired to a fresh mock upstream.
func baseConfig(t *testing.T) *config.Config {
	t.Helper()
	up := httptest.NewServer((&sim.MockServer{LatencyScale: 0}).Handler())
	t.Cleanup(up.Close)
	return &config.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]config.ProviderCfg{
			"mock": {Type: "openai", BaseURL: up.URL + "/v1", ForwardMeta: true, MaxTokensField: "max_tokens", DefaultMaxToken: 256},
		},
		Models: []config.ModelCfg{
			{ID: "mock-local", Provider: "mock", Upstream: "mock-local", InPerMTok: 0.05, OutPerMTok: 0.1, PriorP: 0.7, PriorLatencyMS: 100},
			{ID: "mock-small", Provider: "mock", Upstream: "mock-small", InPerMTok: 0.15, OutPerMTok: 0.6, PriorP: 0.7, PriorLatencyMS: 100},
			{ID: "mock-large", Provider: "mock", Upstream: "mock-large", InPerMTok: 5, OutPerMTok: 25, PriorP: 0.7, PriorLatencyMS: 100},
		},
		Chain:         []string{"mock-local", "mock-small", "mock-large"},
		BaselineModel: "mock-large",
		Router:        config.RouterCfg{MaxAttempts: 3, FailPenaltyUSD: 0.01, AttemptTimeoutMS: 5000, PriorWeight: 2},
		Cache:         config.CacheCfg{Enabled: true, MaxEntries: 1000, TTLSec: 60},
		JudgeModel:    "mock-small",
		Guard:         "flag",
		Quality:       config.QualityCfg{Bar: 0.95, MinObservations: 20},
		MaxTokensCap:  8192,
	}
}

type lagomBlock struct {
	Model     string  `json:"model"`
	Attempts  int     `json:"attempts"`
	CacheHit  bool    `json:"cache_hit"`
	Verdict   string  `json:"verdict"`
	CostUSD   float64 `json:"cost_usd"`
	SavedUSD  float64 `json:"saved_usd"`
	Baseline  float64 `json:"baseline_usd"`
	Escalated bool    `json:"escalated"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Lagom lagomBlock `json:"lagom"`
}

func post(t *testing.T, gw *httptest.Server, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	return postTo(t, gw, "/v1/chat/completions", body, hdr)
}

func postTo(t *testing.T, gw *httptest.Server, path, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const body = `{"model":"auto","messages":[{"role":"user","content":"Classify this ticket"}]}`

func TestEscalatesUntilVerified(t *testing.T) {
	gw, _ := testServer(t)
	// the simulator's cheap models are right 90-94% of the time on "classify";
	// find a prompt where mock-local is wrong so we see a real escalation
	var found bool
	for i := 0; i < 200 && !found; i++ {
		b := strings.Replace(body, "Classify this ticket", "Classify ticket number "+string(rune('A'+i%26))+strings.Repeat("x", i), 1)
		resp, raw := post(t, gw, b, map[string]string{
			"X-Lagom-Task": "classify", "X-Lagom-Verify": "equals:billing",
			"X-Lagom-Mock-Task": "classify", "X-Lagom-Mock-Gold": "billing", "X-Lagom-Mock-Wrong": "bug",
		})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		var r chatResp
		json.Unmarshal(raw, &r)
		if r.Choices[0].Message.Content != "billing" || r.Lagom.Verdict != "pass" {
			t.Fatalf("expected a verified 'billing', got %q verdict %s", r.Choices[0].Message.Content, r.Lagom.Verdict)
		}
		if r.Lagom.Escalated {
			found = true
			if r.Lagom.Attempts < 2 || r.Lagom.SavedUSD >= r.Lagom.Baseline {
				t.Fatalf("bad escalation accounting: %+v", r.Lagom)
			}
		}
	}
	if !found {
		t.Fatal("never saw an escalation; verifier-driven cascade is not working")
	}
}

func TestCacheHitIsFreeAndRechecked(t *testing.T) {
	gw, _ := testServer(t)
	h := map[string]string{"X-Lagom-Verify": "equals:Paris", "X-Lagom-Mock-Task": "faq", "X-Lagom-Mock-Gold": "Paris", "X-Lagom-Mock-Wrong": "Lyon"}
	b := `{"model":"auto","messages":[{"role":"user","content":"Capital of France?"}]}`
	_, raw1 := post(t, gw, b, h)
	var r1 chatResp
	json.Unmarshal(raw1, &r1)
	if r1.Lagom.CacheHit || r1.Lagom.Verdict != "pass" {
		t.Fatalf("first call: %+v", r1.Lagom)
	}
	resp, raw2 := post(t, gw, b, h)
	var r2 chatResp
	json.Unmarshal(raw2, &r2)
	if !r2.Lagom.CacheHit || r2.Lagom.CostUSD != 0 || r2.Lagom.SavedUSD <= 0 || resp.Header.Get("X-Lagom-Cache") != "hit" {
		t.Fatalf("second call should be a free cache hit: %+v", r2.Lagom)
	}
	// a different verifier must not reuse the cached answer's verification
	h2 := map[string]string{"X-Lagom-Verify": "equals:Berlin", "X-Lagom-Mock-Task": "faq", "X-Lagom-Mock-Gold": "Paris"}
	_, raw3 := post(t, gw, b, h2)
	var r3 chatResp
	json.Unmarshal(raw3, &r3)
	if r3.Lagom.CacheHit {
		t.Fatal("cache served an answer verified under a different spec")
	}
	// opting out
	_, raw4 := post(t, gw, b, map[string]string{"X-Lagom-Verify": "equals:Paris", "X-Lagom-No-Cache": "1", "X-Lagom-Mock-Gold": "Paris"})
	var r4 chatResp
	json.Unmarshal(raw4, &r4)
	if r4.Lagom.CacheHit {
		t.Fatal("X-Lagom-No-Cache ignored")
	}
}

func TestPassthroughAndErrors(t *testing.T) {
	gw, _ := testServer(t)
	resp, raw := post(t, gw, strings.Replace(body, "auto", "mock-large", 1), map[string]string{"X-Lagom-Mock-Gold": "ok"})
	var r chatResp
	json.Unmarshal(raw, &r)
	if resp.StatusCode != 200 || r.Lagom.Model != "mock-large" || r.Lagom.Attempts != 1 || r.Lagom.SavedUSD != 0 {
		t.Fatalf("passthrough: %d %+v", resp.StatusCode, r.Lagom)
	}
	if resp, _ := post(t, gw, strings.Replace(body, "auto", "nope", 1), nil); resp.StatusCode != 400 {
		t.Fatalf("unknown model should be 400, got %d", resp.StatusCode)
	}
	if resp, _ := post(t, gw, `{bad json`, nil); resp.StatusCode != 400 {
		t.Fatalf("bad json should be 400, got %d", resp.StatusCode)
	}
	if resp, _ := post(t, gw, body, map[string]string{"X-Lagom-Verify": "bogus:x"}); resp.StatusCode != 400 {
		t.Fatalf("bad verifier should be 400, got %d", resp.StatusCode)
	}
}

func TestStreamingPassthroughIsValidSSE(t *testing.T) {
	gw, _ := testServer(t)
	b := `{"model":"mock-large","stream":true,"messages":[{"role":"user","content":"Say: it works \"quoted\"\nnewline"}]}`
	resp, raw := post(t, gw, b, map[string]string{"X-Lagom-Mock-Gold": `it works "quoted" now`})
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	var sawUsage, sawDone bool
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimPrefix(line, "data: ")
		if p == "[DONE]" {
			sawDone = true
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct{ PromptTokens int } `json:"usage"`
			Lagom *lagomBlock                 `json:"lagom"`
		}
		if err := json.Unmarshal([]byte(p), &ch); err != nil {
			t.Fatalf("invalid chunk JSON %q: %v", p, err)
		}
		for _, c := range ch.Choices {
			text.WriteString(c.Delta.Content)
		}
		if ch.Usage != nil && ch.Lagom != nil {
			sawUsage = true
		}
	}
	if text.String() != `it works "quoted" now` || !sawUsage || !sawDone {
		t.Fatalf("stream text=%q usage=%v done=%v", text.String(), sawUsage, sawDone)
	}
}

func TestStreamWithVerifierIsBufferedThenReplayed(t *testing.T) {
	gw, _ := testServer(t)
	b := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"Capital of Japan?"}]}`
	_, raw := post(t, gw, b, map[string]string{"X-Lagom-Verify": "equals:Tokyo", "X-Lagom-Mock-Gold": "Tokyo", "X-Lagom-Mock-Wrong": "Kyoto"})
	if !bytes.Contains(raw, []byte(`"content":"Tokyo"`)) || !bytes.Contains(raw, []byte("[DONE]")) {
		t.Fatalf("verified stream missing the verified answer:\n%s", raw)
	}
}

func TestConcurrentRequestsAndStats(t *testing.T) {
	gw, s := testServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := strings.Replace(body, "Classify this ticket", "ticket "+strings.Repeat("z", i), 1)
			post(t, gw, b, map[string]string{"X-Lagom-Run": "conc", "X-Lagom-Verify": "equals:billing", "X-Lagom-Mock-Gold": "billing", "X-Lagom-Mock-Task": "classify", "X-Lagom-Task": "classify"})
		}(i)
	}
	wg.Wait()
	s.led.Flush()
	snap := s.led.Snapshot()
	if got := snap.Runs["conc"].Tasks; got != 64 {
		t.Fatalf("ledger recorded %d tasks, want 64 (dropped %d)", got, snap.Dropped)
	}
	if snap.Runs["conc"].SuccessRate < 0.9 {
		t.Fatalf("success rate %.2f", snap.Runs["conc"].SuccessRate)
	}
}

func TestAppendJSONString(t *testing.T) {
	in := "a\"b\\c\nd\te\x01é"
	out := appendJSONString(nil, in)
	var back string
	if err := json.Unmarshal(out, &back); err != nil || back != in {
		t.Fatalf("round trip failed: %q -> %s -> %q (%v)", in, out, back, err)
	}
}

func TestBudgetCapStopsSpending(t *testing.T) {
	gw, s := testServer(t)
	s.cfg.BudgetUSD = 0.000001 // one cent-of-a-cent: the first call may pass, the next must be refused
	pass := strings.Replace(body, "auto", "mock-large", 1)
	resp, _ := post(t, gw, pass, map[string]string{"X-Lagom-Mock-Gold": "ok"})
	if resp.StatusCode != 200 {
		t.Fatalf("first call should succeed, got %d", resp.StatusCode)
	}
	if s.spentUSD() <= 0 {
		t.Fatal("spend was not recorded")
	}
	resp, raw := post(t, gw, pass, nil)
	if resp.StatusCode != 429 || !strings.Contains(string(raw), "budget exhausted") {
		t.Fatalf("second call should be refused with 429, got %d: %s", resp.StatusCode, raw)
	}
	// resetting everything clears the counter
	http.Post(gw.URL+"/v1/admin/reset?scope=all", "application/json", nil)
	if s.spentUSD() != 0 {
		t.Fatal("reset did not clear spend")
	}
}
