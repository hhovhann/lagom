package ledger

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Observation turns the stream of task events into the customer-facing report: what
// the traffic costs today, what routing saves, how much was rerouted, and how that
// breaks down by day, workload and model. It is rebuilt from the ledger at startup, so
// the report survives restarts like the learner does.
//
// Only optimised requests count (model "auto"). Passthrough calls, such as assessment
// runs, are not customer traffic.
type Observation struct {
	mu       sync.Mutex
	baseline string
	days     map[string]*agg
	tasks    map[string]*taskAgg
	models   map[string]*agg
	total    agg
}

type agg struct {
	prompts, rerouted             int
	baselineUSD, costUSD          float64
	reroutedTokens, tokens        int
	reroutedChecked, reroutedPass int
}

type taskAgg struct {
	agg
	routedTo map[string]int // model -> rerouted prompts
}

// NewObservation counts a request as "rerouted" when a model other than baseline answered it.
func NewObservation(baseline string) *Observation {
	o := &Observation{baseline: baseline}
	o.Reset()
	return o
}

func (o *Observation) Reset() {
	o.mu.Lock()
	o.days, o.tasks, o.models, o.total = map[string]*agg{}, map[string]*taskAgg{}, map[string]*agg{}, agg{}
	o.mu.Unlock()
}

func (a *agg) add(e Event, rerouted bool, tokens int) {
	a.prompts++
	a.baselineUSD += e.BaselineUSD
	a.costUSD += e.CostUSD
	a.tokens += tokens
	if rerouted {
		a.rerouted++
		a.reroutedTokens += tokens
		if e.Verdict == "pass" || e.Verdict == "fail" {
			a.reroutedChecked++
			if e.Verdict == "pass" {
				a.reroutedPass++
			}
		}
	}
}

// Add records one event; events that are not optimised, successful task requests are ignored.
func (o *Observation) Add(e Event) {
	if e.Kind != "task" || e.Mode != "optimize" || e.Status != 200 || e.Model == "" {
		return
	}
	tokens := 0
	if n := len(e.Attempts); n > 0 {
		tokens = e.Attempts[n-1].InTok + e.Attempts[n-1].OutTok
	}
	rerouted := e.Model != o.baseline
	day := e.TS.UTC().Format("2006-01-02")

	o.mu.Lock()
	defer o.mu.Unlock()
	o.total.add(e, rerouted, tokens)
	if o.days[day] == nil {
		o.days[day] = &agg{}
	}
	o.days[day].add(e, rerouted, tokens)
	t := o.tasks[e.Task]
	if t == nil {
		t = &taskAgg{routedTo: map[string]int{}}
		o.tasks[e.Task] = t
	}
	t.add(e, rerouted, tokens)
	if rerouted {
		t.routedTo[e.Model]++
	}
	if o.models[e.Model] == nil {
		o.models[e.Model] = &agg{}
	}
	o.models[e.Model].add(e, rerouted, tokens)
}

// ViewOpts says how to present the numbers.
type ViewOpts struct {
	Currency string            // label, e.g. "EUR"
	Rate     float64           // multiplier from the pricing currency (USD) to Currency
	FeeRate  float64           // share of the saving charged, e.g. 0.20
	Tiers    map[string]string // model id -> small | mid | premium
	Names    map[string]string // task -> display name
}

type Day struct {
	Day      string  `json:"day"`
	Prompts  int     `json:"prompts"`
	Rerouted int     `json:"rerouted"`
	Saved    float64 `json:"saved"`
}

type Workload struct {
	Task        string  `json:"task"`
	Name        string  `json:"name"`
	Prompts     int     `json:"prompts"`
	ReroutedPct float64 `json:"rerouted_pct"`
	Today       string  `json:"today"`
	RoutedTo    string  `json:"routed_to"`
	Baseline    float64 `json:"baseline"`
	WithRouting float64 `json:"with_routing"`
	Saved       float64 `json:"saved"`
}

type ModelRow struct {
	Model    string  `json:"model"`
	Tier     string  `json:"tier"`
	Prompts  int     `json:"prompts"`
	SharePct float64 `json:"share_pct"`
	Cost     float64 `json:"cost"`
}

type Score struct {
	Total      int    `json:"total"`
	Label      string `json:"label"`
	Cost       int    `json:"cost"`
	Quality    int    `json:"quality"`
	Fit        int    `json:"fit"`
	Efficiency int    `json:"efficiency"`
}

// View is the report. Money fields are in Currency. "Month" figures scale what was
// observed to 30 days; they describe the observation window and are not an invoice.
type View struct {
	Currency       string     `json:"currency"`
	ObservedDays   int        `json:"observed_days"`
	WindowDays     int        `json:"window_days"`
	Baseline       string     `json:"baseline"`
	Prompts        int        `json:"prompts"`
	Rerouted       int        `json:"rerouted"`
	ReroutedPct    float64    `json:"rerouted_pct"`
	BaselineSpend  float64    `json:"baseline_spend"` // observed window
	CostSpend      float64    `json:"cost_spend"`
	Saved          float64    `json:"saved"`
	BaselineMonth  float64    `json:"baseline_month"`
	CostMonth      float64    `json:"cost_month"`
	SavedMonth     float64    `json:"saved_month"`
	SavedPct       float64    `json:"saved_pct"`
	TokensRerouted int        `json:"tokens_rerouted"`
	QualityPass    float64    `json:"quality_pass"` // pass rate of rerouted answers that had a check; -1 if none
	Score          Score      `json:"score"`
	FeeRate        float64    `json:"fee_rate"`
	Fee            float64    `json:"fee_month"`
	Keep           float64    `json:"keep_month"`
	Daily          []Day      `json:"daily"`
	Workloads      []Workload `json:"workloads"`
	Models         []ModelRow `json:"models"`
}

const windowDays = 30

// View computes the report. The component scores are simple, stated mappings (cost: saving
// against a 40% target; quality: pass rate of rerouted answers; fit: share of prompts
// rerouted; efficiency: share of tokens on lean models), weighted 35/25/25/15.
func (o *Observation) View(opt ViewOpts) View {
	o.mu.Lock()
	defer o.mu.Unlock()
	if opt.Rate == 0 {
		opt.Rate = 1
	}
	v := View{Currency: opt.Currency, WindowDays: windowDays, Baseline: o.baseline, FeeRate: opt.FeeRate, QualityPass: -1,
		Prompts: o.total.prompts, Rerouted: o.total.rerouted, TokensRerouted: o.total.reroutedTokens}
	money := func(usd float64) float64 { return usd * opt.Rate }

	var keys []string
	for d := range o.days {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		first, _ := time.Parse("2006-01-02", keys[0])
		last, _ := time.Parse("2006-01-02", keys[len(keys)-1])
		v.ObservedDays = int(last.Sub(first).Hours()/24) + 1
	}
	for _, d := range keys {
		a := o.days[d]
		v.Daily = append(v.Daily, Day{Day: d, Prompts: a.prompts, Rerouted: a.rerouted, Saved: money(a.baselineUSD - a.costUSD)})
	}

	t := o.total
	if t.prompts == 0 {
		return v
	}
	v.ReroutedPct = float64(t.rerouted) / float64(t.prompts)
	v.BaselineSpend, v.CostSpend = money(t.baselineUSD), money(t.costUSD)
	v.Saved = v.BaselineSpend - v.CostSpend
	scale := 1.0
	if v.ObservedDays > 0 {
		scale = float64(windowDays) / float64(v.ObservedDays)
	}
	v.BaselineMonth, v.CostMonth, v.SavedMonth = v.BaselineSpend*scale, v.CostSpend*scale, v.Saved*scale
	if v.BaselineSpend > 0 {
		v.SavedPct = v.Saved / v.BaselineSpend
	}
	if t.reroutedChecked > 0 {
		v.QualityPass = float64(t.reroutedPass) / float64(t.reroutedChecked)
	}
	if v.SavedMonth > 0 {
		v.Fee = v.SavedMonth * opt.FeeRate
		v.Keep = v.SavedMonth - v.Fee
	}

	clamp := func(x float64) int { return int(math.Round(math.Max(0, math.Min(100, x)))) }
	v.Score.Cost = clamp(v.SavedPct / 0.40 * 100)
	if v.QualityPass >= 0 {
		v.Score.Quality = clamp(v.QualityPass * 100)
	}
	v.Score.Fit = clamp(v.ReroutedPct * 100)
	if t.tokens > 0 {
		v.Score.Efficiency = clamp(float64(t.reroutedTokens) / float64(t.tokens) * 100)
	}
	v.Score.Total = int(math.Round(0.35*float64(v.Score.Cost) + 0.25*float64(v.Score.Quality) + 0.25*float64(v.Score.Fit) + 0.15*float64(v.Score.Efficiency)))
	switch {
	case v.Score.Total >= 85:
		v.Score.Label = "Efficient"
	case v.Score.Total >= 70:
		v.Score.Label = "Balanced"
	default:
		v.Score.Label = "Over-provisioned"
	}

	for task, a := range o.tasks {
		w := Workload{Task: task, Name: task, Prompts: a.prompts, Today: o.baseline,
			Baseline: money(a.baselineUSD), WithRouting: money(a.costUSD), Saved: money(a.baselineUSD - a.costUSD)}
		if n := opt.Names[task]; n != "" {
			w.Name = n
		}
		if a.prompts > 0 {
			w.ReroutedPct = float64(a.rerouted) / float64(a.prompts)
		}
		w.RoutedTo = o.baseline
		best := 0
		for m, n := range a.routedTo {
			if n > best || (n == best && m < w.RoutedTo) {
				best, w.RoutedTo = n, m
			}
		}
		v.Workloads = append(v.Workloads, w)
	}
	sort.Slice(v.Workloads, func(i, j int) bool { return v.Workloads[i].Saved > v.Workloads[j].Saved })
	for m, a := range o.models {
		v.Models = append(v.Models, ModelRow{Model: m, Tier: opt.Tiers[m], Prompts: a.prompts, SharePct: float64(a.prompts) / float64(t.prompts), Cost: money(a.costUSD)})
	}
	rank := map[string]int{"small": 0, "mid": 1, "premium": 2}
	sort.Slice(v.Models, func(i, j int) bool {
		a, b := rank[v.Models[i].Tier], rank[v.Models[j].Tier]
		if a != b {
			return a < b
		}
		return v.Models[i].Model < v.Models[j].Model
	})
	return v
}
