package assess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case "good":
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":12,"completion_tokens":2},"lagom":{"cost_usd":0.00004}}`))
		case "badkey":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
		case "empty":
			w.Write([]byte(`{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":12,"completion_tokens":64},"lagom":{"cost_usd":0.001}}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`model not found`))
		}
	}))
	defer srv.Close()

	res := CheckModels(context.Background(), srv.Client(), srv.URL, "", []string{"good", "badkey", "nomodel", "empty"})
	if !res[0].OK || res[0].InTok != 12 || res[0].OutTok != 2 || res[0].CostUSD != 0.00004 || res[0].Detail != "ok" {
		t.Errorf("good model: %+v", res[0])
	}
	if res[1].OK || !strings.Contains(res[1].Detail, "401") {
		t.Errorf("bad key should fail with the status: %+v", res[1])
	}
	if res[2].OK || !strings.Contains(res[2].Detail, "404") {
		t.Errorf("unknown model should fail with the status: %+v", res[2])
	}
	if !res[3].OK || !strings.Contains(res[3].Detail, "empty reply") {
		t.Errorf("an empty reply should be flagged: %+v", res[3])
	}
}

func TestCheckModelsUnreachable(t *testing.T) {
	res := CheckModels(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "", []string{"x"})
	if res[0].OK || !strings.Contains(res[0].Detail, "cannot reach") {
		t.Errorf("got %+v", res[0])
	}
}
