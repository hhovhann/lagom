package gateway

import (
	"strings"
	"testing"

	"lagom/internal/config"
)

func TestAnthropicMessagesFormatRoutesThroughTheSamePath(t *testing.T) {
	t.Setenv("LAGOM_TEST_API", "secret")
	gw, _ := testServerWith(t, func(c *config.Config) { c.AuthTokenEnv = "LAGOM_TEST_API" })
	call := func(path, key, body string) (int, string) {
		resp, raw := postTo(t, gw, path, body, map[string]string{"x-api-key": key, "X-Lagom-Verify": "contains:anything", "X-Lagom-Task": "classify"})
		return resp.StatusCode, string(raw)
	}
	ok := `{"model":"claude-opus-4-5","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"Classify this ticket"}]}]}`
	code, raw := call("/v1/messages", "secret", ok)
	if code != 200 || !strings.Contains(raw, `"type":"message"`) || !strings.Contains(raw, `"input_tokens"`) || !strings.Contains(raw, `"lagom"`) {
		t.Fatalf("want an Anthropic-shaped message, got %d: %s", code, raw)
	}
	if code, raw := call("/v1/messages", "wrong", ok); code != 401 || !strings.Contains(raw, `"authentication_error"`) {
		t.Errorf("a bad x-api-key must be 401 in Anthropic error shape, got %d: %s", code, raw)
	}
	if code, raw := call("/v1/messages", "secret", strings.Replace(ok, `"max_tokens":64`, `"max_tokens":64,"stream":true`, 1)); code != 400 || !strings.Contains(raw, "streaming") {
		t.Errorf("streaming is not supported yet and must say so, got %d: %s", code, raw)
	}
	img := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`
	if code, _ := call("/v1/messages", "secret", img); code != 400 {
		t.Errorf("non-text content must be rejected, got %d", code)
	}
}
