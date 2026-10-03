package assess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadJSONLDirLoadsFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "t12.pdf"), []byte("%PDF-1.4 fake"), 0o644)
	line := `{"task":"t12","prompt":"Extract.","verify":"fields:min=1:{\"noi\":1}","files":["docs/t12.pdf"]}`
	ex, err := LoadJSONLDir(strings.NewReader(line), dir)
	if err != nil || len(ex) != 1 || len(ex[0].Attachments) != 1 {
		t.Fatalf("%v %+v", err, ex)
	}
	a := ex[0].Attachments[0]
	if a.MediaType != "application/pdf" || a.Name != "t12.pdf" || a.Data == "" {
		t.Errorf("attachment: %+v", a)
	}
}

func TestLoadJSONLDirRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	for name, files := range map[string]string{
		"missing":   `["nope.pdf"]`,
		"wrong ext": `["a.txt"]`,
		"absolute":  `["/etc/passwd.pdf"]`,
		"climbing":  `["../secret.pdf"]`,
	} {
		line := `{"task":"t","prompt":"p","verify":"equals:x","files":` + files + `}`
		if _, err := LoadJSONLDir(strings.NewReader(line), dir); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestGatewayRunnerSendsFiles(t *testing.T) {
	var body struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"lagom":{"verdict":"pass","cost_usd":0.01}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rr.pdf"), []byte("%PDF-1.4 fake"), 0o644)
	ex, err := LoadJSONLDir(strings.NewReader(`{"task":"rr","prompt":"Extract.","verify":"equals:x","files":["rr.pdf"]}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&GatewayRunner{Base: srv.URL, HC: srv.Client()}).Run(context.Background(), "m", ex[0])
	if err != nil || !out.Pass {
		t.Fatalf("%v %+v", err, out)
	}
	parts := body.Messages[0].Content
	if len(parts) != 2 || parts[0]["type"] != "file" || parts[1]["text"] != "Extract." {
		t.Errorf("request parts: %v", parts)
	}
}
