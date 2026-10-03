package gateway

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"lagom/internal/config"
)

const checked = `{"model":"auto","messages":[{"role":"user","content":"Classify this ticket"}]}`

func TestLearningSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	gw, s := testServerWith(t, func(c *config.Config) { c.LedgerPath = path })
	for i := 0; i < 5; i++ {
		post(t, gw, strings.Replace(checked, "ticket", "ticket "+strings.Repeat("x", i), 1),
			map[string]string{"X-Lagom-Verify": "contains:anything", "X-Lagom-Task": "classify"})
	}
	s.led.Flush()
	if len(s.lrn.Snapshot()) == 0 {
		t.Fatal("the first server learned nothing")
	}
	s.Close()

	cfg := baseConfig(t)
	cfg.LedgerPath = path
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap := s2.lrn.Snapshot()
	if len(snap) == 0 || snap[0].Task != "classify" {
		t.Fatalf("a restarted gateway must rebuild what it learned, got %+v", snap)
	}
}

func TestUncheckableTrafficOnlyUsesProvenModels(t *testing.T) {
	gw, s := testServerWith(t, func(c *config.Config) {
		c.Quality = config.QualityCfg{Bar: 0.85, MinObservations: 10}
	})
	headers := map[string]string{"X-Lagom-Task": "classify"}

	_, raw := post(t, gw, checked, headers)
	if !strings.Contains(string(raw), `"model":"mock-large"`) {
		t.Fatalf("nothing is proven yet, so the premium model must answer: %s", raw)
	}

	for i := 0; i < 30; i++ { // evidence that the cheapest model passes this task type
		s.lrn.Observe("classify", "mock-local", true, 0.0001, 0.1, 50, 20, false)
	}
	_, raw = post(t, gw, strings.Replace(checked, "ticket", "ticket two", 1), headers)
	if !strings.Contains(string(raw), `"model":"mock-local"`) {
		t.Fatalf("a model proven to clear the bar should answer: %s", raw)
	}

	_, raw = post(t, gw, strings.Replace(checked, "ticket", "ticket three", 1), map[string]string{"X-Lagom-Task": unclassified})
	if !strings.Contains(string(raw), `"model":"mock-large"`) {
		t.Fatalf("an unclassified prompt must be held on the premium model: %s", raw)
	}
}

func TestClassifierRoutesByContentAndHoldsUnsurePromptsOnPremium(t *testing.T) {
	gw, _ := testServerWith(t, func(c *config.Config) {
		c.Classifier = config.ClassifierCfg{Provider: "mock", Model: "emb", MinScore: 0.3, MinMargin: 0.05, Examples: map[string][]string{
			"classify": {"label this support ticket as billing bug or feature", "classify the customer request into a category"},
			"extract":  {"extract the invoice number vendor and total as json", "pull the fields out of this invoice"},
		}}
	})
	ask := func(prompt string) (task, model string) {
		_, raw := post(t, gw, `{"model":"auto","messages":[{"role":"user","content":"`+prompt+`"}]}`, nil)
		var r struct {
			Lagom struct{ Task, Model string } `json:"lagom"`
		}
		jsonUnmarshal(raw, &r)
		return r.Lagom.Task, r.Lagom.Model
	}
	if task, _ := ask("please classify this customer request into a category"); task != "classify" {
		t.Errorf("a classify-like prompt was placed as %q", task)
	}
	if task, model := ask("write a haiku about autumn moonlight"); task != "unclassified" || model != "mock-large" {
		t.Errorf("an unsure prompt must be unclassified and held on the premium model, got %q on %q", task, model)
	}
}

func TestReportReflectsOptimisedTraffic(t *testing.T) {
	gw, s := testServer(t)
	for i := 0; i < 6; i++ {
		post(t, gw, strings.Replace(checked, "ticket", "ticket "+strings.Repeat("y", i), 1),
			map[string]string{"X-Lagom-Verify": "contains:zzz-never", "X-Lagom-Task": "classify"}) // cascade ends on the premium model
	}
	post(t, gw, `{"model":"mock-large","messages":[{"role":"user","content":"direct call"}]}`, nil) // passthrough: not customer traffic
	s.led.Flush()
	resp, err := http.Get(gw.URL + "/v1/report")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Prompts  int    `json:"prompts"`
		Baseline string `json:"baseline"`
		Rerouted int    `json:"rerouted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Prompts != 6 || v.Baseline != "mock-large" {
		t.Fatalf("only optimised requests are observed (6), got %+v", v)
	}
}

func TestBaselineCostIsCalibratedByMeasuredBaselineOutput(t *testing.T) {
	gw, s := testServer(t)
	ask := func() (baseline, cost float64) {
		_, raw := post(t, gw, strings.Replace(checked, "ticket", "ticket "+strings.Repeat("c", int(s.seq.Load())), 1),
			map[string]string{"X-Lagom-Verify": "contains:OK", "X-Lagom-Task": "classify"})
		var r struct {
			Lagom struct {
				Baseline float64 `json:"baseline_usd"`
				Cost     float64 `json:"cost_usd"`
				Model    string  `json:"model"`
			} `json:"lagom"`
		}
		jsonUnmarshal(raw, &r)
		if r.Lagom.Model == "mock-large" {
			t.Fatalf("the cheap model should have answered: %s", raw)
		}
		return r.Lagom.Baseline, r.Lagom.Cost
	}
	assumed, _ := ask() // no baseline measurements yet: the baseline is assumed to write what the cheap model wrote
	for i := 0; i < 15; i++ {
		s.lrn.Observe("classify", "mock-large", true, 0.02, 5, 100, 1000, false) // measured: the baseline writes ~1000 tokens here
	}
	measured, cost := ask()
	if measured < assumed*20 || measured <= cost {
		t.Errorf("a baseline measured at ~1000 tokens must price far above the assumption: assumed %v, measured %v, cost %v", assumed, measured, cost)
	}
}
