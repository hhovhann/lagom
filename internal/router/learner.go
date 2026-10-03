// Package learner decides which model a task class should START on.
//
// Every task is executed as a cascade: start on model i, verify, and on failure
// escalate to model i+1 and so on. For a start index i the expected cost of the
// task is
//
//	E_i = c_i + (1 - p_i) * E_{i+1}          E_last = c_last + (1 - p_last) * penalty
//
// where c_i is the mean cost of one attempt on model i (dollars plus a price on
// latency) and p_i is the probability that an attempt on model i passes the
// verifier. The learner keeps a Beta posterior over every p_i (per task class)
// and a running mean of every c_i, draws one sample of each p_i (Thompson
// sampling), and starts the task at argmin_i E_i. Exploration therefore falls
// out of posterior uncertainty and fades as evidence accumulates; no epsilon.
//
// Because every attempt of a cascade is an observation for the model that ran
// it, escalations also teach the learner about the more expensive models.
package router

import (
	"math"
	"math/rand/v2"
	"sort"
	"sync"
)

// Model is what the learner needs to know about a chain entry.
type LearnerModel struct {
	ID         string
	InPerMTok  float64
	OutPerMTok float64
	PriorP     float64
	PriorLatMS float64
	Private    bool
}

type LearnerConfig struct {
	FailPenaltyUSD   float64
	LatencyUSDPerSec float64
	PriorWeight      float64 // pseudo-observations behind PriorP
}

const (
	priorIn  = 250.0 // prompt tokens assumed before any data
	priorOut = 80.0
	costK    = 2.0 // pseudo-observations behind the price-based cost prior
)

type arm struct {
	succ, fail float64
	n          int
	tokN       int     // attempts that reported token usage
	outTok     float64 // completion tokens over those attempts
	costSum    float64 // USD
	latSum     float64 // seconds
	starts     int
}

type taskTokens struct {
	n       int
	in, out float64
}

type Learner struct {
	mu     sync.Mutex
	chain  []LearnerModel
	cfg    LearnerConfig
	arms   map[string][]arm // task -> one arm per chain entry
	tokens map[string]*taskTokens
	idx    map[string]int
}

func NewLearner(chain []LearnerModel, cfg LearnerConfig) *Learner {
	l := &Learner{chain: chain, cfg: cfg}
	l.Reset()
	for i, m := range chain {
		l.idx[m.ID] = i
	}
	return l
}

func (l *Learner) Reset() {
	l.mu.Lock()
	l.arms = map[string][]arm{}
	l.tokens = map[string]*taskTokens{}
	l.idx = map[string]int{}
	for i, m := range l.chain {
		l.idx[m.ID] = i
	}
	l.mu.Unlock()
}

func (l *Learner) armsFor(task string) []arm {
	a, ok := l.arms[task]
	if !ok {
		a = make([]arm, len(l.chain))
		l.arms[task] = a
	}
	return a
}

func (l *Learner) priorCost(m LearnerModel) float64 {
	return (priorIn*m.InPerMTok + priorOut*m.OutPerMTok) / 1e6
}

func (l *Learner) priorLat(m LearnerModel) float64 { return m.PriorLatMS / 1000 }

func (l *Learner) meanCost(m LearnerModel, a arm) float64 {
	pc := l.priorCost(m)
	pl := l.priorLat(m)
	c := (pc*costK + a.costSum) / (costK + float64(a.n))
	lat := (pl*costK + a.latSum) / (costK + float64(a.n))
	return c + l.cfg.LatencyUSDPerSec*lat
}

// Choose returns the chain index the task should start on.
func (l *Learner) Choose(task string) int {
	l.mu.Lock()
	src := l.armsFor(task)
	arms := make([]arm, len(src))
	copy(arms, src)
	l.mu.Unlock()

	n := len(l.chain)
	p := make([]float64, n)
	c := make([]float64, n)
	for i, m := range l.chain {
		w := l.cfg.PriorWeight
		a0 := m.PriorP*w + arms[i].succ
		b0 := (1-m.PriorP)*w + arms[i].fail
		p[i] = betaSample(a0, b0)
		c[i] = l.meanCost(m, arms[i])
	}
	e := make([]float64, n)
	e[n-1] = c[n-1] + (1-p[n-1])*l.cfg.FailPenaltyUSD
	for i := n - 2; i >= 0; i-- {
		e[i] = c[i] + (1-p[i])*e[i+1]
	}
	best := 0
	for i := 1; i < n; i++ {
		if e[i] < e[best] {
			best = i
		}
	}
	return best
}

// Qualified returns the cheapest chain index that is PROVEN to clear the quality bar
// for this task type: at least minObs observations and a 95% lower bound on its
// pass rate of bar or better. ok is false when no model is proven yet; the caller
// then uses the most capable model rather than guessing.
func (l *Learner) Qualified(task string, bar float64, minObs int) (idx int, ok bool) {
	if minObs < 1 {
		minObs = 1 // a model with no evidence is never proven
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	arms := l.armsFor(task)
	for i, a := range arms {
		if a.n >= minObs && WilsonLower(a.succ, a.n) >= bar {
			return i, true
		}
	}
	return len(l.chain) - 1, false
}

// MeanOut returns the mean completion tokens a model has produced for a task type and how
// many attempts that mean rests on. The gateway uses it to price what the baseline model
// would have written, instead of assuming it writes as much as the cheaper model that answered.
func (l *Learner) MeanOut(task, model string) (mean float64, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.idx[model]
	if !ok {
		return 0, 0
	}
	a := l.armsFor(task)[i]
	if a.tokN == 0 {
		return 0, 0
	}
	return a.outTok / float64(a.tokN), a.tokN
}

// Observe records one attempt. success is the verifier outcome for that attempt.
func (l *Learner) Observe(task, model string, success bool, costUSD, latSec float64, inTok, outTok int, isStart bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.idx[model]
	if !ok {
		return
	}
	a := l.armsFor(task)
	if success {
		a[i].succ++
	} else {
		a[i].fail++
	}
	a[i].n++
	if inTok+outTok > 0 { // provider errors report no usage; they must not drag the mean down
		a[i].tokN++
		a[i].outTok += float64(outTok)
	}
	a[i].costSum += costUSD
	a[i].latSum += latSec
	if isStart {
		a[i].starts++
	}
	t := l.tokens[task]
	if t == nil {
		t = &taskTokens{}
		l.tokens[task] = t
	}
	t.n++
	t.in += float64(inTok)
	t.out += float64(outTok)
}

// ---- reporting ----

type ArmSnap struct {
	Model    string  `json:"model"`
	Private  bool    `json:"private"`
	N        int     `json:"n"`
	Starts   int     `json:"starts"`
	P        float64 `json:"p"`         // posterior mean success rate
	Lower    float64 `json:"lower"`     // 95% Wilson lower bound on observed success
	MeanCost float64 `json:"mean_cost"` // USD per attempt
	MeanLat  float64 `json:"mean_lat_ms"`
}

type TaskSnap struct {
	Task   string    `json:"task"`
	Volume int       `json:"volume"`
	AvgIn  float64   `json:"avg_in_tokens"`
	AvgOut float64   `json:"avg_out_tokens"`
	Arms   []ArmSnap `json:"arms"`
}

func (l *Learner) Snapshot() []TaskSnap {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []TaskSnap
	for task, arms := range l.arms {
		ts := TaskSnap{Task: task}
		if t := l.tokens[task]; t != nil && t.n > 0 {
			ts.AvgIn, ts.AvgOut = t.in/float64(t.n), t.out/float64(t.n)
		}
		for i, a := range arms {
			m := l.chain[i]
			s := ArmSnap{Model: m.ID, Private: m.Private, N: a.n, Starts: a.starts}
			ts.Volume += a.starts
			w := l.cfg.PriorWeight
			s.P = (m.PriorP*w + a.succ) / (w + a.succ + a.fail)
			s.Lower = WilsonLower(a.succ, a.n)
			s.MeanCost = (l.priorCost(m)*costK + a.costSum) / (costK + float64(a.n))
			s.MeanLat = (l.priorLat(m)*costK + a.latSum) / (costK + float64(a.n)) * 1000
			ts.Arms = append(ts.Arms, s)
		}
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}

func WilsonLower(succ float64, n int) float64 {
	if n == 0 {
		return 0
	}
	const z = 1.96
	nf := float64(n)
	p := succ / nf
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	m := z * math.Sqrt((p*(1-p)+z*z/(4*nf))/nf)
	return (c - m) / d
}

// ---- sampling ----

func betaSample(a, b float64) float64 {
	x := gammaSample(a)
	y := gammaSample(b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaSample draws Gamma(shape, 1) with Marsaglia-Tsang.
func gammaSample(shape float64) float64 {
	if shape < 1 {
		u := rand.Float64()
		return gammaSample(shape+1) * math.Pow(u, 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rand.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rand.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
