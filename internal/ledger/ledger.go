// Package ledger records every task execution and aggregates the numbers the
// product is judged on: cost per successful task, success rate, latency,
// cache hit rate and counterfactual savings.
//
// The hot path only does a non-blocking channel send. A single consumer
// goroutine owns all aggregation, the JSONL file and the learner updates, so no
// request ever waits on bookkeeping. If the buffer is full the event is dropped
// and counted (never block a customer's request for telemetry).
package ledger

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Attempt struct {
	Model     string  `json:"model"`
	Verdict   string  `json:"verdict"` // "pass", "fail", "n/a" (no verifier) or "error"
	Err       string  `json:"err,omitempty"`
	InTok     int     `json:"in_tok"`
	OutTok    int     `json:"out_tok"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMS float64 `json:"latency_ms"`
}

type Event struct {
	TS          time.Time `json:"ts"`
	Kind        string    `json:"kind"` // "task" or "outcome"
	ID          string    `json:"id"`
	Run         string    `json:"run"`
	Task        string    `json:"task"`
	Mode        string    `json:"mode"` // "optimize" or "passthrough"
	Model       string    `json:"model"`
	Attempts    []Attempt `json:"attempts"`
	CacheHit    bool      `json:"cache_hit"`
	Verdict     string    `json:"verdict"` // "pass", "fail", "n/a"
	CostUSD     float64   `json:"cost_usd"`
	JudgeUSD    float64   `json:"judge_usd,omitempty"`
	BaselineUSD float64   `json:"baseline_usd"`
	SavedUSD    float64   `json:"saved_usd"`
	LatencyMS   float64   `json:"latency_ms"`
	OverheadUS  int64     `json:"overhead_us"`
	Streamed    bool      `json:"streamed"`
	Learn       bool      `json:"learn"`
	Status      int       `json:"status"`
}

type item struct {
	ev  Event
	ctl chan struct{}
}

type seqPt struct {
	cost  float64
	ok    int8 // 1 pass, 0 fail, -1 unknown
	exec  bool // a model actually ran (not a cache hit)
	escal bool
	task  string
	start string // first model tried
}

type runAgg struct {
	tasks, withVerdict, pass   int
	cost, baseline, saved      float64
	cacheHits, escal, errTasks int
	attempts                   int
	lat, over                  []float64
	mix                        map[string]map[string]int
	seq                        []seqPt
	first, last                time.Time
}

type Ledger struct {
	ch      chan item
	dropped atomic.Int64
	mu      sync.Mutex
	runs    map[string]*runAgg
	recent  []Event
	path    string
	file    *os.File
	w       *bufio.Writer
	observe func(Event)
	done    chan struct{}
}

const maxSamples = 1_000_000

// New starts the consumer. path may be empty (no file). observe is called for
// every event on the consumer goroutine (used to feed the learner).
func New(path string, observe func(Event)) (*Ledger, error) {
	l := &Ledger{
		ch:      make(chan item, 1<<16),
		runs:    map[string]*runAgg{},
		observe: observe,
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		l.path, l.file, l.w = path, f, bufio.NewWriterSize(f, 1<<16)
	}
	go l.loop()
	return l, nil
}

// Emit never blocks.
func (l *Ledger) Emit(e Event) {
	select {
	case l.ch <- item{ev: e}:
	default:
		l.dropped.Add(1)
	}
}

func (l *Ledger) Dropped() int64 { return l.dropped.Load() }

// Flush waits until every event emitted before the call has been processed.
func (l *Ledger) Flush() {
	c := make(chan struct{})
	l.ch <- item{ctl: c}
	<-c
}

// Reset clears the aggregates and starts a fresh log: the old file is kept next to
// it as <path>.reset, so a restart cannot bring the cleared history back.
func (l *Ledger) Reset() {
	l.Flush()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = map[string]*runAgg{}
	l.recent = nil
	if l.file == nil {
		return
	}
	l.w.Flush()
	l.file.Close()
	os.Rename(l.path, l.path+".reset")
	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		l.file, l.w = f, bufio.NewWriterSize(f, 1<<16)
	}
}

func (l *Ledger) Close() {
	l.Flush()
	if l.w != nil {
		l.w.Flush()
		l.file.Close()
	}
}

func (l *Ledger) loop() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case it := <-l.ch:
			if it.ctl != nil {
				if l.w != nil {
					l.w.Flush()
				}
				close(it.ctl)
				continue
			}
			l.apply(it.ev)
		case <-tick.C:
			if l.w != nil {
				l.mu.Lock()
				l.w.Flush()
				l.mu.Unlock()
			}
		}
	}
}

func (l *Ledger) apply(e Event) {
	if l.observe != nil {
		l.observe(e)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		if b, err := json.Marshal(&e); err == nil {
			l.w.Write(b)
			l.w.WriteByte('\n')
		}
	}
	if e.Kind != "task" {
		return
	}
	r := l.runs[e.Run]
	if r == nil {
		r = &runAgg{mix: map[string]map[string]int{}}
		l.runs[e.Run] = r
	}
	if r.first.IsZero() {
		r.first = e.TS
	}
	r.last = e.TS
	r.tasks++
	r.cost += e.CostUSD
	r.baseline += e.BaselineUSD
	r.saved += e.SavedUSD
	r.attempts += len(e.Attempts)
	if e.CacheHit {
		r.cacheHits++
	}
	if len(e.Attempts) > 1 {
		r.escal++
	}
	if e.Status >= 400 {
		r.errTasks++
	}
	pt := seqPt{cost: e.CostUSD, ok: -1, exec: !e.CacheHit && len(e.Attempts) > 0, escal: len(e.Attempts) > 1, task: e.Task}
	if len(e.Attempts) > 0 {
		pt.start = e.Attempts[0].Model
	}
	switch e.Verdict {
	case "pass":
		r.withVerdict++
		r.pass++
		pt.ok = 1
	case "fail":
		r.withVerdict++
		pt.ok = 0
	}
	if len(r.seq) < maxSamples {
		r.seq = append(r.seq, pt)
		r.lat = append(r.lat, e.LatencyMS)
		r.over = append(r.over, float64(e.OverheadUS))
	}
	if e.Model != "" {
		m := r.mix[e.Task]
		if m == nil {
			m = map[string]int{}
			r.mix[e.Task] = m
		}
		m[e.Model]++
	}
	l.recent = append(l.recent, e)
	if len(l.recent) > 40 {
		l.recent = l.recent[len(l.recent)-40:]
	}
}

// ---- reporting ----

// SeriesPt summarises one window of consecutive tasks. ExecCostPerTask and
// EscalationRate look only at tasks where a model actually ran, so cache hits
// cannot mask (or fake) learning.
type SeriesPt struct {
	I               int     `json:"i"`
	CostPerSucc     float64 `json:"cost_per_success"`
	SuccessRate     float64 `json:"success_rate"`
	ExecCostPerTask float64 `json:"exec_cost_per_task"`
	EscalationRate  float64 `json:"escalation_rate"`
}

// StartMix shows which model each task class started on, early vs late in the run.
type StartMix struct {
	Early map[string]int `json:"early"`
	Late  map[string]int `json:"late"`
}

type RunStats struct {
	Name           string                    `json:"name"`
	Tasks          int                       `json:"tasks"`
	WithVerdict    int                       `json:"with_verdict"`
	Successes      int                       `json:"successes"`
	SuccessRate    float64                   `json:"success_rate"`
	CostUSD        float64                   `json:"cost_usd"`
	CostPerSuccess float64                   `json:"cost_per_success_usd"`
	BaselineUSD    float64                   `json:"baseline_usd"`
	SavedUSD       float64                   `json:"saved_usd"`
	SavedPct       float64                   `json:"saved_pct"`
	CacheHits      int                       `json:"cache_hits"`
	CacheHitRate   float64                   `json:"cache_hit_rate"`
	EscalationRate float64                   `json:"escalation_rate"`
	Errors         int                       `json:"errors"`
	LatP50         float64                   `json:"lat_p50_ms"`
	LatP95         float64                   `json:"lat_p95_ms"`
	LatP99         float64                   `json:"lat_p99_ms"`
	OverP50        float64                   `json:"overhead_p50_us"`
	OverP99        float64                   `json:"overhead_p99_us"`
	WallSec        float64                   `json:"wall_sec"`
	Mix            map[string]map[string]int `json:"mix"`
	Series         []SeriesPt                `json:"series"`
	StartMix       map[string]StartMix       `json:"start_mix"`
}

type Snapshot struct {
	Runs    map[string]RunStats `json:"runs"`
	Recent  []Event             `json:"recent"`
	Dropped int64               `json:"dropped_events"`
}

func (l *Ledger) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := Snapshot{Runs: map[string]RunStats{}, Dropped: l.dropped.Load()}
	out.Recent = append(out.Recent, l.recent...)
	for name, r := range l.runs {
		rs := RunStats{
			Name: name, Tasks: r.tasks, WithVerdict: r.withVerdict, Successes: r.pass,
			CostUSD: r.cost, BaselineUSD: r.baseline, SavedUSD: r.saved,
			CacheHits: r.cacheHits, Errors: r.errTasks,
			Mix: map[string]map[string]int{},
		}
		for t, m := range r.mix {
			rs.Mix[t] = map[string]int{}
			for k, v := range m {
				rs.Mix[t][k] = v
			}
		}
		if r.withVerdict > 0 {
			rs.SuccessRate = float64(r.pass) / float64(r.withVerdict)
		}
		if r.pass > 0 {
			rs.CostPerSuccess = r.cost / float64(r.pass)
		}
		if r.baseline > 0 {
			rs.SavedPct = r.saved / r.baseline * 100
		}
		if r.tasks > 0 {
			rs.CacheHitRate = float64(r.cacheHits) / float64(r.tasks)
		}
		if exec := r.tasks - r.cacheHits; exec > 0 {
			rs.EscalationRate = float64(r.escal) / float64(exec)
		}
		rs.LatP50, rs.LatP95, rs.LatP99 = pct(r.lat, 50), pct(r.lat, 95), pct(r.lat, 99)
		rs.OverP50, rs.OverP99 = pct(r.over, 50), pct(r.over, 99)
		rs.WallSec = r.last.Sub(r.first).Seconds()
		rs.Series = series(r.seq)
		rs.StartMix = startMix(r.seq)
		out.Runs[name] = rs
	}
	return out
}

func series(seq []seqPt) []SeriesPt {
	if len(seq) == 0 {
		return nil
	}
	w := len(seq) / 24
	if w < 25 {
		w = 25
	}
	var out []SeriesPt
	for s := 0; s < len(seq); s += w {
		e := s + w
		if e > len(seq) {
			e = len(seq)
		}
		var cost, execCost float64
		var pass, verd, exec, esc int
		for _, p := range seq[s:e] {
			cost += p.cost
			if p.ok >= 0 {
				verd++
			}
			if p.ok == 1 {
				pass++
			}
			if p.exec {
				exec++
				execCost += p.cost
				if p.escal {
					esc++
				}
			}
		}
		pt := SeriesPt{I: e}
		if pass > 0 {
			pt.CostPerSucc = cost / float64(pass)
		}
		if verd > 0 {
			pt.SuccessRate = float64(pass) / float64(verd)
		}
		if exec > 0 {
			pt.ExecCostPerTask = execCost / float64(exec)
			pt.EscalationRate = float64(esc) / float64(exec)
		}
		out = append(out, pt)
	}
	return out
}

// startMix compares the first and last third of each task class's executed
// tasks: which model did the router choose to start on?
func startMix(seq []seqPt) map[string]StartMix {
	byTask := map[string][]string{}
	for _, p := range seq {
		if p.exec && p.start != "" {
			byTask[p.task] = append(byTask[p.task], p.start)
		}
	}
	out := map[string]StartMix{}
	for task, starts := range byTask {
		third := len(starts) / 3
		if third < 3 {
			continue
		}
		sm := StartMix{Early: map[string]int{}, Late: map[string]int{}}
		for _, m := range starts[:third] {
			sm.Early[m]++
		}
		for _, m := range starts[len(starts)-third:] {
			sm.Late[m]++
		}
		out[task] = sm
	}
	return out
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := make([]float64, len(xs))
	copy(c, xs)
	sort.Float64s(c)
	i := int(float64(len(c)-1) * p / 100)
	return c[i]
}

// Replay feeds every event stored in the JSONL log at path to fn, in order, and
// returns how many it read. This is how learning survives a restart: the log is the
// source of truth and the learner is rebuilt from it. A missing file is not an
// error; lines that do not parse (a torn last write, say) are skipped.
func Replay(path string, fn func(Event)) (int, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) || path == "" {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		fn(e)
		n++
	}
	return n, sc.Err()
}
