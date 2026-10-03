package gateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"lagom/internal/assess"
	"lagom/internal/providers"
	"lagom/internal/sim"
)

//go:embed wizard.html
var wizardHTML []byte

// sampleJSON is a recorded run (same shape as a live run) so the wizard works with no key.
//
//go:embed sample.json
var sampleJSON []byte

type tierView struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	In    float64 `json:"in_per_mtok"`
	Out   float64 `json:"out_per_mtok"`
}

// handleDemoConfig lists what a visitor can run live and the spend cap.
func (s *Server) handleDemoConfig(w http.ResponseWriter, r *http.Request) {
	type prov struct {
		ID          string     `json:"id"`
		Label       string     `json:"label"`
		KeyRequired bool       `json:"key_required"`
		Tiers       []tierView `json:"tiers"`
	}
	provs := []prov{}
	for id, p := range s.cfg.Demo.Providers {
		pv := prov{ID: id, Label: p.Label, KeyRequired: p.KeyRequired}
		for _, t := range p.Tiers {
			pv.Tiers = append(pv.Tiers, tierView{t.ID, t.Label, t.InPerMTok, t.OutPerMTok})
		}
		provs = append(provs, pv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_cap_usd": s.cfg.Demo.SessionCapUSD, "providers": provs, "classifier": s.classifier != nil})
}

func (s *Server) handleDemoSample(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(sampleJSON)
}

// handleDemoLive runs the workload pack on every tier of one provider, paid by the
// visitor's key and hard-capped. The key is used for these calls only: it is never stored,
// logged or echoed, the provider's base URL comes from the config (never from the request),
// and error text is scrubbed of the key before it is returned.
func (s *Server) handleDemoLive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
		PerTask  int    `json:"per_task"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	pc, ok := s.cfg.Demo.Providers[in.Provider]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown provider")
		return
	}
	key := in.Key
	if !pc.KeyRequired {
		key = os.Getenv(pc.APIKeyEnv)
	}
	if pc.KeyRequired && (key == "" || len(key) > 512) {
		writeErr(w, http.StatusBadRequest, "this provider needs your API key (it is used for this run only)")
		return
	}
	select {
	case s.liveSem <- struct{}{}:
		defer func() { <-s.liveSem }()
	default:
		writeErr(w, http.StatusTooManyRequests, "another live run is in progress; try again shortly")
		return
	}
	if in.PerTask < 3 || in.PerTask > 25 {
		in.PerTask = 10
	}

	var prov providers.Provider
	if pc.Type == "anthropic" {
		prov = providers.NewAnthropic(pc.BaseURL, key)
	} else {
		prov = providers.NewOpenAI(pc.BaseURL, key, false, pc.MaxTokensField)
	}
	specs := map[string]assess.ModelSpec{}
	var order []string
	var tiers []tierView
	for _, t := range pc.Tiers {
		specs[t.ID] = assess.ModelSpec{Upstream: t.Upstream, InPerMTok: t.InPerMTok, OutPerMTok: t.OutPerMTok, Effort: t.Effort, MaxTokens: t.MaxTokens, PromptSuffix: t.PromptSuffix}
		order = append(order, t.ID)
		tiers = append(tiers, tierView{t.ID, t.Label, t.InPerMTok, t.OutPerMTok})
	}
	var examples []assess.Example
	for _, t := range sim.Generate(in.PerTask*4, 7) {
		examples = append(examples, assess.Example{Task: t.Type, Prompt: t.Prompt, Verify: t.Verify})
	}

	runner := &assess.ProviderRunner{Prov: prov, Models: specs, Cap: s.cfg.Demo.SessionCapUSD}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	results := assess.Evaluate(ctx, runner, order, examples, 4)
	for i := range results {
		results[i].FirstError = "" // provider text can quote the visitor's key; only the scrubbed first_error below is returned
	}

	firstErr := runner.FirstError()
	if key != "" {
		firstErr = strings.ReplaceAll(firstErr, key, "[key]")
	}
	if len(firstErr) > 160 {
		firstErr = firstErr[:160]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results, "models": tiers, "spent": runner.Spent(), "cap": s.cfg.Demo.SessionCapUSD,
		"capped": runner.Spent() >= s.cfg.Demo.SessionCapUSD, "first_error": firstErr,
	})
}

// handleDemoClassify shows the "day to day" step: how one prompt is placed and which
// model would answer it when nothing can check the answer.
func (s *Server) handleDemoClassify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "send {\"prompt\": \"...\"}")
		return
	}
	task := s.taskOf(r.Context(), "", []providers.Message{{Role: "user", Content: providers.Content(in.Prompt)}}, true)
	idx, proven := len(s.chain)-1, false
	if task != unclassified {
		idx, proven = s.lrn.Qualified(task, s.cfg.Quality.BarFor(task), s.cfg.Quality.MinObservations)
	}
	method := "keywords"
	if s.classifier != nil {
		method = "embeddings"
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "method": method, "model": s.chain[idx].cfg.ID, "proven": proven, "bar": s.cfg.Quality.BarFor(task)})
}
