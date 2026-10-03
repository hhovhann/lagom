package ledger

import (
	"testing"
	"time"
)

func ev(day int, task, model string, base, cost float64, verdict string) Event {
	return Event{TS: time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC), Kind: "task", Mode: "optimize", Status: 200, Task: task, Model: model,
		BaselineUSD: base, CostUSD: cost, Verdict: verdict, Attempts: []Attempt{{Model: model, InTok: 60, OutTok: 40}}}
}

func TestObservationScalesToAMonthAndScoresTheEfficiency(t *testing.T) {
	o := NewObservation("big")
	for i := 0; i < 8; i++ { // 8 rerouted prompts over 2 days, each cost 1 of a 4 baseline
		o.Add(ev(1+i%2, "classify", "small", 4, 1, "pass"))
	}
	o.Add(ev(2, "reason", "big", 4, 4, "pass"))                                                              // 1 prompt kept on the premium model
	o.Add(Event{Kind: "task", Mode: "passthrough", Status: 200, Model: "big", BaselineUSD: 99, CostUSD: 99}) // assessment traffic is ignored
	v := o.View(ViewOpts{Currency: "EUR", Rate: 1, FeeRate: 0.2, Tiers: map[string]string{"small": "small", "big": "premium"}, Names: map[string]string{"classify": "Ticket triage"}})

	if v.Prompts != 9 || v.Rerouted != 8 || v.ObservedDays != 2 {
		t.Fatalf("counts: %+v", v)
	}
	if v.BaselineSpend != 36 || v.CostSpend != 12 || v.Saved != 24 {
		t.Errorf("observed spend: %+v", v)
	}
	if v.BaselineMonth != 540 || v.SavedMonth != 360 { // x15 for 30 days / 2 observed
		t.Errorf("month scaling: baseline %v saved %v", v.BaselineMonth, v.SavedMonth)
	}
	if v.Fee != 72 || v.Keep != 288 {
		t.Errorf("fee %v keep %v", v.Fee, v.Keep)
	}
	if v.QualityPass != 1 || v.Score.Cost != 100 || v.Score.Quality != 100 || v.Score.Label != "Efficient" {
		t.Errorf("score: %+v pass %v", v.Score, v.QualityPass)
	}
	if v.Workloads[0].Name != "Ticket triage" || v.Workloads[0].RoutedTo != "small" || v.Workloads[0].Saved != 24 {
		t.Errorf("workloads: %+v", v.Workloads)
	}
	if len(v.Models) != 2 || v.Models[0].Tier != "small" {
		t.Errorf("models: %+v", v.Models)
	}
	o.Reset()
	if o.View(ViewOpts{}).Prompts != 0 {
		t.Error("Reset must clear the observation")
	}
}

func TestEmptyObservationIsSafe(t *testing.T) {
	v := NewObservation("big").View(ViewOpts{})
	if v.Prompts != 0 || v.Score.Total != 0 || v.QualityPass != -1 {
		t.Errorf("%+v", v)
	}
}
