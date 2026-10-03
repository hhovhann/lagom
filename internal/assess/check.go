package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"lagom/internal/config"
)

// CheckResult is one model's smoke-test outcome.
type CheckResult struct {
	Model     string
	OK        bool
	Detail    string // the reply, or why it failed
	LatencyMS float64
	InTok     int
	OutTok    int
	CostUSD   float64
}

// CheckModels sends one tiny request to each model through the gateway and reports
// whether the key, the model id and the price all work. It spends a fraction of a cent.
func CheckModels(ctx context.Context, hc *http.Client, base, token string, models []string) []CheckResult {
	out := make([]CheckResult, 0, len(models))
	for _, m := range models {
		out = append(out, checkOne(ctx, hc, base, token, m))
	}
	return out
}

func checkOne(ctx context.Context, hc *http.Client, base, token, model string) CheckResult {
	r := CheckResult{Model: model}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with the single word: ok"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lagom-Task", "check")
	req.Header.Set("X-Lagom-Run", "check")
	req.Header.Set("X-Lagom-No-Cache", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	t0 := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		r.Detail = "cannot reach the gateway: " + err.Error()
		return r
	}
	defer resp.Body.Close()
	r.LatencyMS = float64(time.Since(t0)) / 1e6
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		r.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, oneLine(string(raw), 300))
		return r
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Lagom struct {
			CostUSD float64 `json:"cost_usd"`
		} `json:"lagom"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Choices) == 0 {
		r.Detail = "unreadable reply: " + oneLine(string(raw), 200)
		return r
	}
	r.OK = true
	r.Detail = oneLine(v.Choices[0].Message.Content, 40)
	r.InTok, r.OutTok, r.CostUSD = v.Usage.Prompt, v.Usage.Completion, v.Lagom.CostUSD
	if strings.TrimSpace(r.Detail) == "" {
		r.Detail = "(empty reply: raise max_tokens for this model if it is a thinking model)"
	}
	return r
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// CheckCommand implements "lagom check": a cheap smoke test before any real spend.
func CheckCommand(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	gw := fs.String("gateway", "http://127.0.0.1:8080", "gateway base URL")
	models := fs.String("models", "", "comma-separated model ids (default: every model in the gateway's chain)")
	fs.Parse(args)

	config.LoadDotEnv(".env")
	token := os.Getenv("LAGOM_API_KEY")
	order := splitList(*models)
	if len(order) == 0 {
		order = chainOf(*gw, token)
	}
	fmt.Printf("checking %d models with one tiny request each ...\n\n", len(order))
	res := CheckModels(context.Background(), &http.Client{Timeout: 3 * time.Minute}, *gw, token, order)
	failed := 0
	total := 0.0
	for _, r := range res {
		if !r.OK {
			failed++
			fmt.Printf("  FAIL  %-18s %s\n", r.Model, r.Detail)
			continue
		}
		total += r.CostUSD
		price := fmt.Sprintf("$%.6f", r.CostUSD)
		if r.CostUSD == 0 {
			price = "price unknown or free: check in_per_mtok/out_per_mtok in the config"
		}
		fmt.Printf("  ok    %-18s %5.0f ms  %d in / %d out tokens  %s  reply: %q\n", r.Model, r.LatencyMS, r.InTok, r.OutTok, price, r.Detail)
	}
	fmt.Printf("\n%d of %d models passed; this check cost about $%.5f\n", len(res)-failed, len(res), total)
	if failed > 0 {
		fmt.Println("Fix the FAIL lines before running assess: 401/403 = the key; 404 = the provider does not know the model id; 400 \"unknown model\" = the id is not in your config; other 400 = often a token-limit setting.")
		os.Exit(1)
	}
}
