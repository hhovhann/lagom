package assess

import (
	"fmt"
	"sort"
	"strings"
)

// Verdicts a recommendation can carry.
const (
	UseSmaller   = "use smaller model"
	KeepPremium  = "keep premium model"
	NeedMoreData = "need more examples"
)

// Rec is the decision for one workload.
type Rec struct {
	Task         string  `json:"task"`
	Examples     int     `json:"examples"`
	Bar          float64 `json:"bar"`
	Baseline     string  `json:"baseline"`
	BaselineRate float64 `json:"baseline_rate"`
	BaselineCost float64 `json:"baseline_cost_usd"`
	Model        string  `json:"model"` // the recommended model (the baseline when nothing cheaper qualifies)
	Rate         float64 `json:"rate"`
	Lower        float64 `json:"lower"`
	Cost         float64 `json:"cost_usd"`
	SavingPct    float64 `json:"saving_pct"`
	Verdict      string  `json:"verdict"`
}

// Recommend picks, per workload, the cheapest model PROVEN to clear the bar. order
// lists the models cheapest to most capable; baseline is what the customer uses today
// (normally the last). bars maps a workload to its required pass rate; defBar covers the rest.
//
// A model qualifies when the 95% lower bound on its pass rate reaches the bar, so a
// handful of lucky examples cannot qualify it. When a cheaper model's raw rate reaches
// the bar but the evidence is too thin to prove it, the verdict is NeedMoreData.
func Recommend(results []ModelResult, order []string, baseline string, bars map[string]float64, defBar float64) []Rec {
	by := map[string]map[string]ModelResult{}
	for _, r := range results {
		if by[r.Task] == nil {
			by[r.Task] = map[string]ModelResult{}
		}
		by[r.Task][r.Model] = r
	}
	var recs []Rec
	for task, models := range by {
		bar := defBar
		if b, ok := bars[task]; ok {
			bar = b
		}
		base := models[baseline]
		rec := Rec{Task: task, Bar: bar, Baseline: baseline, BaselineRate: base.Rate, BaselineCost: base.MeanCostUSD,
			Examples: base.N, Model: baseline, Rate: base.Rate, Lower: base.Lower, Cost: base.MeanCostUSD, Verdict: KeepPremium}
		for _, id := range order {
			if id == baseline {
				break
			}
			r, ok := models[id]
			if !ok || r.N == 0 {
				continue
			}
			if r.Lower >= bar {
				rec.Model, rec.Rate, rec.Lower, rec.Cost, rec.Verdict = id, r.Rate, r.Lower, r.MeanCostUSD, UseSmaller
				break
			}
			if r.Rate >= bar && rec.Verdict == KeepPremium {
				rec.Verdict = NeedMoreData
			}
		}
		if rec.Verdict == UseSmaller && rec.BaselineCost > 0 {
			rec.SavingPct = (rec.BaselineCost - rec.Cost) / rec.BaselineCost * 100
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Task < recs[j].Task })
	return recs
}

// Markdown renders the workload analysis table.
func Markdown(recs []Rec) string {
	var b strings.Builder
	b.WriteString("| Workload | Examples | Current model | Required | Cheaper model | Accuracy (95% lower) | Saving | Recommendation |\n")
	b.WriteString("|---|---:|---|---:|---|---:|---:|---|\n")
	for _, r := range recs {
		model, acc, saving := "-", "-", "-"
		if r.Model != r.Baseline {
			model = r.Model
		}
		if r.Verdict == UseSmaller {
			acc = fmt.Sprintf("%.1f%% (%.1f%%)", r.Rate*100, r.Lower*100)
			saving = fmt.Sprintf("%.0f%%", r.SavingPct)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %.0f%% | %s | %s | %s | %s |\n", r.Task, r.Examples, r.Baseline, r.Bar*100, model, acc, saving, r.Verdict)
	}
	return b.String()
}

// Failures lists, for the terminal, every model and workload where calls failed (a rejected key,
// an unknown model id, a provider that does not accept PDFs) with the first error seen. Failed
// calls are not counted as wrong answers; they are not in the pass rate at all.
func Failures(results []ModelResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Errors == 0 {
			continue
		}
		fmt.Fprintf(&b, "WARNING: %s on %s: %d calls failed (not counted as wrong answers). First error: %s\n", r.Model, r.Task, r.Errors, r.FirstError)
	}
	return b.String()
}

// failureCounts is the report's version: counts only, since provider error text can quote a key fragment.
func failureCounts(results []ModelResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Errors > 0 {
			fmt.Fprintf(&b, "- %s on %s: %d calls failed and are not counted (see the terminal output for the error)\n", r.Model, r.Task, r.Errors)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "**Calls that failed (not scored):**\n\n" + b.String() + "\n"
}
