package assess

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// scripted passes a fixed fraction of examples per model, deterministically.
type scripted struct {
	passEvery map[string]int // model -> pass 1 of every N calls
	cost      map[string]float64
	calls     map[string]*atomic.Int64
}

func (s *scripted) Run(_ context.Context, model string, ex Example) (Outcome, error) {
	n := s.calls[model].Add(1)
	return Outcome{Pass: int(n)%s.passEvery[model] != 0, CostUSD: s.cost[model], LatencyMS: 10}, nil
}

func newScripted() *scripted {
	return &scripted{
		passEvery: map[string]int{"small": 1000, "mid": 1000, "big": 1000},
		cost:      map[string]float64{"small": 0.001, "mid": 0.004, "big": 0.02},
		calls:     map[string]*atomic.Int64{"small": {}, "mid": {}, "big": {}},
	}
}

func examples(n int) []Example {
	out := make([]Example, n)
	for i := range out {
		out[i] = Example{Task: "classify", Prompt: "p", Verify: "equals:x"}
	}
	return out
}

func TestRecommendsCheapestModelThatIsProvenNotJustCheapest(t *testing.T) {
	r := newScripted()
	r.passEvery["small"] = 3 // ~67% pass: fails any sensible bar
	res := Evaluate(context.Background(), r, []string{"small", "mid", "big"}, examples(300), 4)
	recs := Recommend(res, []string{"small", "mid", "big"}, "big", nil, 0.95)
	if len(recs) != 1 || recs[0].Verdict != UseSmaller || recs[0].Model != "mid" {
		t.Fatalf("mid is the cheapest model that clears the bar, got %+v", recs)
	}
	if got := recs[0].SavingPct; got < 79 || got > 81 { // (0.02-0.004)/0.02
		t.Errorf("saving = %.1f%%, want 80%%", got)
	}
}

func TestThinEvidenceNeverQualifiesAModel(t *testing.T) {
	r := newScripted() // every model passes everything
	res := Evaluate(context.Background(), r, []string{"small", "mid", "big"}, examples(10), 2)
	recs := Recommend(res, []string{"small", "mid", "big"}, "big", nil, 0.95)
	if recs[0].Verdict != NeedMoreData {
		t.Fatalf("10 perfect examples cannot prove a 95%% bar; want %q, got %+v", NeedMoreData, recs[0])
	}
}

func TestKeepPremiumWhenNothingCheaperQualifies(t *testing.T) {
	r := newScripted()
	r.passEvery["small"], r.passEvery["mid"] = 2, 2
	res := Evaluate(context.Background(), r, []string{"small", "mid", "big"}, examples(200), 4)
	if rec := Recommend(res, []string{"small", "mid", "big"}, "big", nil, 0.95)[0]; rec.Verdict != KeepPremium || rec.Model != "big" || rec.SavingPct != 0 {
		t.Fatalf("got %+v", rec)
	}
}

func TestPerWorkloadBarOverridesDefault(t *testing.T) {
	r := newScripted()
	r.passEvery["small"] = 20 // 95% pass
	res := Evaluate(context.Background(), r, []string{"small", "mid", "big"}, examples(400), 4)
	order := []string{"small", "mid", "big"}
	if rec := Recommend(res, order, "big", map[string]float64{"classify": 0.80}, 0.99)[0]; rec.Model != "small" {
		t.Errorf("with an 80%% bar the small model qualifies, got %+v", rec)
	}
	if rec := Recommend(res, order, "big", nil, 0.99)[0]; rec.Model == "small" {
		t.Errorf("with a 99%% bar the small model must not qualify, got %+v", rec)
	}
}

type capped struct{ n atomic.Int64 }

func (c *capped) Run(context.Context, string, Example) (Outcome, error) {
	if c.n.Add(1) > 5 {
		return Outcome{}, ErrBudget
	}
	return Outcome{Pass: true, CostUSD: 0.01}, nil
}

func TestBudgetStopsCallsAndSkippedExamplesAreNotCountedAsFailures(t *testing.T) {
	res := Evaluate(context.Background(), &capped{}, []string{"a"}, examples(50), 1)
	if len(res) != 1 || res[0].N != 5 || res[0].Pass != 5 {
		t.Fatalf("only the 5 calls that ran may be counted, got %+v", res)
	}
}

func TestLoadJSONLValidates(t *testing.T) {
	ok := `{"task":"classify","prompt":"a","verify":"equals:b"}` + "\n\n" + `{"task":"extract","prompt":"c","verify":"number:3"}`
	if ex, err := LoadJSONL(strings.NewReader(ok)); err != nil || len(ex) != 2 {
		t.Fatalf("got %v, %v", ex, err)
	}
	for _, bad := range []string{`{"task":"x","prompt":"y"}`, `not json`} {
		if _, err := LoadJSONL(strings.NewReader(bad)); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestMarkdownTable(t *testing.T) {
	md := Markdown([]Rec{{Task: "classify", Examples: 100, Bar: 0.95, Baseline: "big", Model: "mid", Rate: 0.99, Lower: 0.95, SavingPct: 80, Verdict: UseSmaller},
		{Task: "reason", Examples: 100, Bar: 0.95, Baseline: "big", Model: "big", Verdict: KeepPremium}})
	for _, want := range []string{"| classify | 100 | big | 95% | mid | 99.0% (95.0%) | 80% | use smaller model |", "| reason | 100 | big | 95% | - | - | - | keep premium model |"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing row %q in:\n%s", want, md)
		}
	}
}

type flaky struct{ calls atomic.Int64 }

// every example's first call fails; the retry succeeds, and one example always fails
func (f *flaky) Run(_ context.Context, _ string, ex Example) (Outcome, error) {
	if ex.Prompt == "always" {
		return Outcome{}, errors.New("provider down")
	}
	if f.calls.Add(1)%2 == 1 {
		return Outcome{}, errors.New("transient")
	}
	return Outcome{Pass: true, CostUSD: 0.01}, nil
}

func TestProviderErrorsAreRetriedAndReportedApartFromQuality(t *testing.T) {
	ex := []Example{{Task: "t", Prompt: "a", Verify: "equals:x"}, {Task: "t", Prompt: "always", Verify: "equals:x"}}
	res := Evaluate(context.Background(), &flaky{}, []string{"m"}, ex, 1)
	if len(res) != 1 || res[0].N != 1 || res[0].Pass != 1 || res[0].Errors != 1 || res[0].Rate != 1 {
		t.Fatalf("a transient error is retried, a hard failure is counted as an error and not as a wrong answer: %+v", res)
	}
}

type failingRunner struct{}

func (failingRunner) Run(ctx context.Context, model string, ex Example) (Outcome, error) {
	return Outcome{}, errors.New("gateway returned HTTP 502: the provider does not accept PDF files")
}

func TestFailedCallsAreExplainedAndNotCountedAsWrong(t *testing.T) {
	res := Evaluate(context.Background(), failingRunner{}, []string{"m"}, []Example{{Task: "t", Prompt: "p", Verify: "equals:x"}}, 1)
	if len(res) != 1 || res[0].N != 0 || res[0].Errors != 1 || !strings.Contains(res[0].FirstError, "does not accept PDF") {
		t.Fatalf("%+v", res)
	}
	if out := Failures(res); !strings.Contains(out, "m on t: 1 calls failed") || !strings.Contains(out, "does not accept PDF") {
		t.Errorf("terminal text: %q", out)
	}
	if out := failureCounts(res); !strings.Contains(out, "1 calls failed") || strings.Contains(out, "PDF") {
		t.Errorf("the report carries counts only: %q", out)
	}
}
