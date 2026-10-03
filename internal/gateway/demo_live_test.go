package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lagom/internal/config"
)

func demoServer(t *testing.T, upstream string, price float64) *httptest.Server {
	t.Helper()
	gw, _ := testServerWith(t, func(c *config.Config) {
		c.Demo = config.DemoCfg{SessionCapUSD: 0.5, Providers: map[string]config.DemoProviderCfg{
			"fake": {Label: "Fake", Type: "openai", BaseURL: upstream, KeyRequired: true, Tiers: []config.DemoTier{
				{Label: "small", ID: "s", Upstream: "s", InPerMTok: price, OutPerMTok: price},
				{Label: "premium", ID: "p", Upstream: "p", InPerMTok: price, OutPerMTok: price},
			}},
		}}
	})
	return gw
}

func TestLiveRunNeverEchoesTheKeyEvenWhenTheProviderDoes(t *testing.T) {
	const key = "sk-test-SECRET-123456"
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid key " + r.Header.Get("Authorization"))) // a provider echoing the credential
	}))
	defer bad.Close()
	gw := demoServer(t, bad.URL, 1)
	resp, raw := postTo(t, gw, "/v1/demo/live", `{"provider":"fake","key":"`+key+`","per_task":3}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "SECRET-123456") {
		t.Fatalf("the visitor's key leaked into the response: %s", raw)
	}
	if !strings.Contains(string(raw), "[key]") {
		t.Errorf("the provider's error should be shown with the key scrubbed: %s", raw)
	}
}

func TestLiveRunStopsAtTheSpendCap(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":100000,"completion_tokens":100000}}`))
	}))
	defer up.Close()
	gw := demoServer(t, up.URL, 10) // $2 per call against a $0.50 cap
	_, raw := postTo(t, gw, "/v1/demo/live", `{"provider":"fake","key":"k","per_task":10}`, nil)
	if !strings.Contains(string(raw), `"capped":true`) {
		t.Fatalf("a run that exceeds the cap must say so: %s", raw)
	}
	var calls int
	for _, part := range strings.Split(string(raw), `"n":`)[1:] {
		calls += int(part[0] - '0') // single-digit counts: 80 possible calls, only a few may run
	}
	if calls > 12 {
		t.Errorf("the cap did not stop the run: ~%d calls were made", calls)
	}
}

func TestLiveRunRejectsUnknownProviderAndMissingKey(t *testing.T) {
	gw := demoServer(t, "http://127.0.0.1:1", 1)
	for body, want := range map[string]int{
		`{"provider":"nope","key":"k"}`:                          400,
		`{"provider":"fake"}`:                                    400,
		`{"provider":"fake","key":"k","base_url":"http://evil"}`: 200, // base_url in a request is ignored, never followed
	} {
		if resp, _ := postTo(t, gw, "/v1/demo/live", body, nil); resp.StatusCode != want {
			t.Errorf("%s -> %d, want %d", body, resp.StatusCode, want)
		}
	}
}

func TestDemoClassifyAndConfigAndSample(t *testing.T) {
	gw := demoServer(t, "http://127.0.0.1:1", 1)
	_, raw := postTo(t, gw, "/v1/demo/classify", `{"prompt":"Classify this ticket: refund please"}`, nil)
	if !strings.Contains(string(raw), `"model":"mock-large"`) || !strings.Contains(string(raw), `"proven":false`) {
		t.Errorf("nothing is proven yet, so the premium model is shown: %s", raw)
	}
	for _, path := range []string{"/v1/demo/config", "/v1/demo/sample"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %v %v", path, resp, err)
		}
		resp.Body.Close()
	}
}
