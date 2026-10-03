package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"lagom/internal/providers"
	"lagom/internal/router"
)

// GatewayRunner runs examples through a Lagom gateway, naming each model directly
// so the gateway records real cost and verdicts. With Learn set, the gateway also feeds
// the results to its learner, which is how an assessment "seeds" routing.
type GatewayRunner struct {
	Base  string // e.g. http://127.0.0.1:8080
	Token string // gateway API token, if it uses one
	Learn bool
	HC    *http.Client
}

func (g *GatewayRunner) Run(ctx context.Context, model string, ex Example) (Outcome, error) {
	body, err := json.Marshal(map[string]any{"model": model, "messages": []providers.Message{{Role: "user", Content: providers.Content(ex.Prompt), Attachments: ex.Attachments}}})
	if err != nil {
		return Outcome{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Outcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lagom-Verify", ex.Verify)
	req.Header.Set("X-Lagom-Task", ex.Task)
	req.Header.Set("X-Lagom-Run", "assess")
	req.Header.Set("X-Lagom-No-Cache", "1")
	if g.Learn {
		req.Header.Set("X-Lagom-Learn", "1")
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	hc := g.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	t0 := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return Outcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return Outcome{}, fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, oneLine(string(b), 400))
	}
	var out struct {
		Lagom struct {
			Verdict string  `json:"verdict"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"lagom"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return Outcome{}, err
	}
	return Outcome{Pass: out.Lagom.Verdict == "pass", CostUSD: out.Lagom.CostUSD, LatencyMS: float64(time.Since(t0)) / 1e6}, nil
}

// ModelSpec is what a ProviderRunner needs to call and price one model.
type ModelSpec struct {
	Upstream     string
	InPerMTok    float64
	OutPerMTok   float64
	Effort       string
	MaxTokens    int
	PromptSuffix string // appended to every prompt (e.g. " /no_think" for a non-thinking tier)
}

// ProviderRunner calls a provider directly (used by the live demo, where the caller's
// own key pays for the calls). It stops with ErrBudget once Cap dollars are spent. The
// checks are the deterministic kinds; a judge check needs a gateway and is rejected.
type ProviderRunner struct {
	Prov   providers.Provider
	Models map[string]ModelSpec
	Cap    float64

	mu       sync.Mutex
	spent    float64
	firstErr string
}

// FirstError is the first provider error seen, for telling a visitor why a run failed
// (a rejected key, say). The caller must scrub it before showing it.
func (p *ProviderRunner) FirstError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstErr
}

// Spent reports the money spent so far.
func (p *ProviderRunner) Spent() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spent
}

func (p *ProviderRunner) Run(ctx context.Context, model string, ex Example) (Outcome, error) {
	spec, ok := p.Models[model]
	if !ok {
		return Outcome{}, fmt.Errorf("unknown model %q", model)
	}
	check, err := router.Parse(ex.Verify)
	if err != nil || check == nil || check.IsJudge() {
		return Outcome{}, fmt.Errorf("example needs a deterministic check")
	}
	p.mu.Lock()
	over := p.Cap > 0 && p.spent >= p.Cap
	p.mu.Unlock()
	if over {
		return Outcome{}, ErrBudget
	}
	mt := spec.MaxTokens
	if mt == 0 {
		mt = 512
	}
	t0 := time.Now()
	res, err := p.Prov.Chat(ctx, &providers.Call{Messages: []providers.Message{{Role: "user", Content: providers.Content(ex.Prompt + spec.PromptSuffix)}}, MaxTokens: mt, Effort: spec.Effort}, spec.Upstream, nil)
	if err != nil {
		p.mu.Lock()
		if p.firstErr == "" {
			p.firstErr = err.Error()
		}
		p.mu.Unlock()
		return Outcome{}, err
	}
	cost := (float64(res.Usage.PromptTokens)*spec.InPerMTok + float64(res.Usage.CompletionTokens)*spec.OutPerMTok) / 1e6
	p.mu.Lock()
	p.spent += cost
	p.mu.Unlock()
	return Outcome{Pass: check.Check(res.Text), CostUSD: cost, LatencyMS: float64(time.Since(t0)) / 1e6}, nil
}
