package gateway

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lagom/internal/config"
)

// A PDF sent to the gateway must reach the provider intact, be scored by the verifier on the
// answer text, and not be confused with a different PDF by the cache.
func TestPDFReachesProviderAndIsScored(t *testing.T) {
	var seen []string // the file_data of each upstream request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					File struct {
						Data string `json:"file_data"`
					} `json:"file"`
				} `json:"content"`
			} `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &in); err != nil || len(in.Messages) == 0 || len(in.Messages[0].Content) == 0 {
			t.Errorf("upstream did not get content parts: %v %s", err, b[:min(len(b), 200)])
			http.Error(w, "bad", 400)
			return
		}
		seen = append(seen, in.Messages[0].Content[0].File.Data)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"noi\": 731270}"}}],"usage":{"prompt_tokens":1200,"completion_tokens":8}}`))
	}))
	defer up.Close()

	gw, _ := testServerWith(t, func(c *config.Config) {
		c.Providers = map[string]config.ProviderCfg{"p": {Type: "openai", BaseURL: up.URL, MaxTokensField: "max_tokens"}}
		c.Models = []config.ModelCfg{{ID: "m", Provider: "p", Upstream: "m", InPerMTok: 1, OutPerMTok: 1, PriceVerified: true}}
		c.Chain, c.BaselineModel, c.JudgeModel = []string{"m"}, "m", ""
	})

	// ~9.4 MB of base64: above the old 8 MB body limit, as a real multi-page scan can be.
	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("0123456789abcdef", 480000)))
	req := func(data string) string {
		return `{"model":"m","messages":[{"role":"user","content":[` +
			`{"type":"file","file":{"filename":"t12.pdf","file_data":"data:application/pdf;base64,` + data + `"}},` +
			`{"type":"text","text":"Extract the NOI."}]}]}`
	}
	hdr := map[string]string{"X-Lagom-Verify": `fields:min=1:{"noi":731270}`, "X-Lagom-Task": "t12"}

	resp, b := post(t, gw, req(big), hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b[:min(len(b), 300)])
	}
	var out chatResp
	json.Unmarshal(b, &out)
	if out.Lagom.Verdict != "pass" {
		t.Errorf("the answer matches the gold, verdict = %q", out.Lagom.Verdict)
	}
	if len(seen) != 1 || seen[0] != "data:application/pdf;base64,"+big {
		t.Fatalf("the provider must receive the PDF unchanged (got %d requests)", len(seen))
	}

	other := base64.StdEncoding.EncodeToString([]byte("a different pdf"))
	post(t, gw, req(other), hdr)
	if len(seen) != 2 {
		t.Errorf("a different PDF with the same prompt must not be served from the cache; upstream calls = %d", len(seen))
	}
}

func TestBadFileIsRejectedNotDropped(t *testing.T) {
	gw, _ := testServer(t)
	resp, _ := post(t, gw, `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"text","text":"hi"}]}]}`, nil)
	if resp.StatusCode != 400 {
		t.Errorf("a remote image link must be a 400, got %d", resp.StatusCode)
	}
}
