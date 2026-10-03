// Package gateway is the HTTP front end: OpenAI-compatible chat completions,
// the routing cascade, and the admin/reporting endpoints.
package gateway

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"lagom/internal/router"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lagom/internal/config"
	"lagom/internal/ledger"
	"lagom/internal/providers"
)

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed report.html
var reportHTML []byte

type model struct {
	cfg  config.ModelCfg
	prov providers.Provider
	br   breaker
}

// breaker is a tiny circuit breaker: after 3 consecutive failures the model is
// skipped for 10 seconds (unless it is the last resort).
type breaker struct {
	fails     atomic.Int32
	openUntil atomic.Int64
}

func (b *breaker) allow() bool { return time.Now().UnixNano() >= b.openUntil.Load() }

func (b *breaker) record(ok bool) {
	if ok {
		b.fails.Store(0)
		return
	}
	if b.fails.Add(1) >= 3 {
		b.openUntil.Store(time.Now().Add(10 * time.Second).UnixNano())
		b.fails.Store(0)
	}
}

type Server struct {
	cfg         *config.Config
	models      map[string]*model
	chain       []*model
	baseline    *model
	judge       *model
	lrn         *router.Learner
	cache       *router.Cache
	led         *ledger.Ledger
	pending     *pendingStore
	guard       *Guard
	classifier  *router.Classifier
	obs         *ledger.Observation
	liveSem     chan struct{} // bounds concurrent live demo runs
	runs, tasks *labelSet
	authToken   string
	adminToken  string
	simulated   bool // any provider is the mock simulator

	seq                        atomic.Uint64
	inflight                   atomic.Int64
	reqTotal                   atomic.Int64
	hits                       atomic.Int64
	errs                       atomic.Int64
	guardFlagged, guardBlocked atomic.Int64
	unclassifiedN              atomic.Int64
	spent                      atomic.Int64 // nano-dollars spent on upstream calls since start
}

func New(cfg *config.Config) (*Server, error) {
	s := &Server{cfg: cfg, models: map[string]*model{}, pending: newPending(100_000),
		guard: newGuard(cfg.Guard), liveSem: make(chan struct{}, 2), runs: newLabelSet(500), tasks: newLabelSet(500)}

	provs := map[string]providers.Provider{}
	for name, pc := range cfg.Providers {
		key := ""
		if pc.APIKeyEnv != "" {
			key = os.Getenv(pc.APIKeyEnv)
		}
		switch pc.Type {
		case "openai":
			provs[name] = providers.NewOpenAI(pc.BaseURL, key, pc.ForwardMeta, pc.MaxTokensField)
		case "anthropic":
			provs[name] = providers.NewAnthropic(pc.BaseURL, key)
		default:
			return nil, fmt.Errorf("provider %q: unknown type %q", name, pc.Type)
		}
		if pc.ForwardMeta {
			s.simulated = true
		}
		if pc.APIKeyEnv != "" && key == "" {
			log.Printf("warning: provider %q: env %s is empty; calls will fail until it is set", name, pc.APIKeyEnv)
		}
	}
	for _, mc := range cfg.Models {
		m := &model{cfg: mc, prov: provs[mc.Provider]}
		s.models[mc.ID] = m
		pc := cfg.Providers[mc.Provider]
		if !mc.PriceVerified && !pc.ForwardMeta && (mc.InPerMTok > 0 || mc.OutPerMTok > 0) {
			log.Printf("warning: model %q has unverified pricing; check the provider's price page before trusting savings", mc.ID)
		}
	}
	if cc := cfg.Classifier; cc.Model != "" {
		emb, ok := provs[cc.Provider].(providers.Embedder)
		if !ok {
			return nil, fmt.Errorf("classifier.provider %q cannot embed", cc.Provider)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cl, err := router.NewClassifier(ctx, emb, cc.Model, cc.Examples, cc.MinScore, cc.MinMargin)
		if err != nil {
			return nil, fmt.Errorf("classifier: %w", err)
		}
		s.classifier = cl
	}
	var lm []router.LearnerModel
	for _, id := range cfg.Chain {
		m := s.models[id]
		s.chain = append(s.chain, m)
		lm = append(lm, router.LearnerModel{
			ID: id, InPerMTok: m.cfg.InPerMTok, OutPerMTok: m.cfg.OutPerMTok,
			PriorP: m.cfg.PriorP, PriorLatMS: m.cfg.PriorLatencyMS, Private: m.cfg.Private,
		})
	}
	if cfg.BaselineModel != "" {
		s.baseline = s.models[cfg.BaselineModel]
	} else {
		s.baseline = s.chain[len(s.chain)-1]
	}
	s.obs = ledger.NewObservation(s.baseline.cfg.ID)
	if jm := cfg.JudgeModel; jm != "" {
		for _, id := range cfg.Chain {
			if id == jm {
				log.Printf("warning: judge_model %q is also in the chain: a model grading its own kind of answer is not an independent grader; use a different model", jm)
			}
		}
	}
	if cfg.JudgeModel != "" {
		s.judge = s.models[cfg.JudgeModel]
	}
	s.lrn = router.NewLearner(lm, router.LearnerConfig{
		FailPenaltyUSD:   cfg.Router.FailPenaltyUSD,
		LatencyUSDPerSec: cfg.Router.LatencyUSDPerSec,
		PriorWeight:      cfg.Router.PriorWeight,
	})
	if cfg.Cache.Enabled {
		s.cache = router.NewCache(cfg.Cache.MaxEntries, time.Duration(cfg.Cache.TTLSec)*time.Second)
	}
	if cfg.AuthTokenEnv != "" {
		s.authToken = os.Getenv(cfg.AuthTokenEnv)
	}
	if cfg.AdminTokenEnv != "" {
		s.adminToken = os.Getenv(cfg.AdminTokenEnv)
	}
	if s.authToken == "" && s.adminToken == "" && !cfg.AllowOpen && !isLoopback(cfg.Listen) {
		return nil, fmt.Errorf("listen %q is not loopback and no auth token is set: set auth_token_env (and admin_token_env), or allow_open to override", cfg.Listen)
	}
	if s.authToken == "" && s.adminToken == "" {
		log.Printf("warning: no auth tokens configured: /v1/stats, /v1/admin/reset and /metrics are open to anyone who can reach %s. Keep the listener on loopback or set auth_token_env / admin_token_env.", cfg.Listen)
	}
	// Learning survives restarts: rebuild the learner from the ledger before serving.
	if n, err := ledger.Replay(cfg.LedgerPath, s.observe); err != nil {
		return nil, fmt.Errorf("replay ledger: %w", err)
	} else if n > 0 {
		log.Printf("learner restored from %d ledger events (%s)", n, cfg.LedgerPath)
	}
	led, err := ledger.New(cfg.LedgerPath, s.observe)
	if err != nil {
		return nil, err
	}
	s.led = led
	return s, nil
}

func (s *Server) Close() { s.led.Close() }

// isLoopback reports whether a listen address is reachable only from this machine.
// An empty host (":8080") listens on every interface, so it is not loopback.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// labelSet bounds the cardinality of caller-supplied labels (run, task). Every
// distinct label becomes a map entry in the ledger or learner, so an unbounded
// set would let one client grow server memory without limit.
type labelSet struct {
	mu  sync.Mutex
	set map[string]bool
	max int
}

func newLabelSet(max int) *labelSet { return &labelSet{set: map[string]bool{}, max: max} }

// clean keeps [A-Za-z0-9._:-], truncates to 64 bytes, and maps labels beyond the
// cardinality cap to "other".
func (l *labelSet) clean(v, fallback string) string {
	b := make([]byte, 0, 64)
	for i := 0; i < len(v) && len(b) < 64; i++ {
		c := v[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-' {
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return fallback
	}
	k := string(b)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.set[k] {
		if len(l.set) >= l.max {
			return "other"
		}
		l.set[k] = true
	}
	return k
}

// addSpend records real money spent. It is updated on the request path (one
// atomic add) so the budget guard is exact even though the ledger is async.
func (s *Server) addSpend(usd float64) { s.spent.Add(int64(usd*1e9 + 0.5)) }

func (s *Server) spentUSD() float64 { return float64(s.spent.Load()) / 1e9 }

func (s *Server) overBudget() bool { return s.cfg.BudgetUSD > 0 && s.spentUSD() >= s.cfg.BudgetUSD }

// observe feeds the learner. It runs on the ledger's consumer goroutine, never
// on a request goroutine.
func (s *Server) observe(e ledger.Event) {
	s.obs.Add(e)
	if !e.Learn {
		return
	}
	for i, a := range e.Attempts {
		switch a.Verdict {
		case "pass", "fail", "error":
			s.lrn.Observe(e.Task, a.Model, a.Verdict == "pass", a.CostUSD, a.LatencyMS/1000, a.InTok, a.OutTok, i == 0 && e.Kind == "task")
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/outcomes", s.handleOutcome)
	mux.HandleFunc("GET /v1/models", s.api(s.handleModels))
	mux.HandleFunc("GET /v1/stats", s.admin(s.handleStats))
	mux.HandleFunc("GET /v1/report/candidates", s.admin(s.handleCandidates))
	mux.HandleFunc("GET /v1/report", s.admin(s.handleReport))
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) { servePage(w, reportHTML) })
	mux.HandleFunc("POST /v1/admin/reset", s.admin(s.handleReset))
	mux.HandleFunc("GET /metrics", s.admin(s.handleMetrics))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /v1/demo/tasks", s.api(s.handleDemoTasks))
	mux.HandleFunc("GET /v1/demo/config", s.api(s.handleDemoConfig))
	mux.HandleFunc("GET /v1/demo/sample", s.api(s.handleDemoSample))
	mux.HandleFunc("POST /v1/demo/live", s.api(s.handleDemoLive))
	mux.HandleFunc("POST /v1/demo/classify", s.api(s.handleDemoClassify))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { servePage(w, wizardHTML) })
	mux.HandleFunc("GET /race", func(w http.ResponseWriter, r *http.Request) { servePage(w, demoHTML) })
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) { servePage(w, dashboardHTML) })
	return mux
}

// servePage sends an embedded HTML page with browser hardening headers. The pages
// use inline script and style, so the CSP allows those but nothing from other origins.
func servePage(w http.ResponseWriter, page []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.Write(page)
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return h[len("Bearer "):]
	}
	return r.Header.Get("x-api-key") // Anthropic SDKs send the key here
}

// tokenOK compares in constant time so the check does not leak the token.
func tokenOK(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// authorized guards the customer-facing API (chat, outcomes, models).
func (s *Server) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	return tokenOK(bearer(r), s.authToken)
}

// adminAuthorized guards stats, reset, reports and metrics. The admin token is
// separate so a leaked application key cannot read spend data or wipe state.
// If no admin token is set it falls back to the API token; if neither is set the
// endpoints are open (loopback demo mode).
func (s *Server) adminAuthorized(r *http.Request) bool {
	want := s.adminToken
	if want == "" {
		want = s.authToken
	}
	if want == "" {
		return true
	}
	return tokenOK(bearer(r), want)
}

func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuthorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "admin token required")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "lagom_error"}})
}

func (m *model) cost(in, out int) float64 {
	return (float64(in)*m.cfg.InPerMTok + float64(out)*m.cfg.OutPerMTok) / 1e6
}

// ---- admin / reporting ----

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{{"id": "auto", "object": "model", "owned_by": "lagom"}}
	for _, m := range s.chain {
		data = append(data, map[string]any{
			"id": m.cfg.ID, "object": "model", "owned_by": m.cfg.Provider,
			"in_per_mtok": m.cfg.InPerMTok, "out_per_mtok": m.cfg.OutPerMTok, "private": m.cfg.Private,
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.led.Snapshot()
	chain := make([]string, len(s.chain))
	for i, m := range s.chain {
		chain[i] = m.cfg.ID
	}
	writeJSON(w, 200, map[string]any{
		"ledger":     snap,
		"learner":    s.lrn.Snapshot(),
		"chain":      chain,
		"baseline":   s.baseline.cfg.ID,
		"cache_size": s.cacheLen(),
		"inflight":   s.inflight.Load(),
		"simulated":  s.simulated,
		"spent_usd":  s.spentUSD(),
		"budget_usd": s.cfg.BudgetUSD,
	})
}

func (s *Server) cacheLen() int {
	if s.cache == nil {
		return 0
	}
	return s.cache.Len()
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope") // "all" (default) or "learner"
	s.led.Flush()
	s.lrn.Reset()
	if scope != "learner" {
		s.obs.Reset()
	}
	if s.cache != nil {
		s.cache.Clear()
	}
	if scope != "learner" {
		s.led.Reset()
		s.spent.Store(0)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "scope": scope})
}

// Candidate describes what a task class should do about the frontier model.
//
// Status:
//
//	move        a cheaper model clears the quality bar on its own (95% lower bound
//	            on pass rate >= candMinLower): route there; keep the verifier as a guard
//	first-try   a cheaper model is right often enough that "try it, escalate to the
//	            frontier model on failure" is cheaper in expectation
//	keep        no cheaper option pays off on the evidence so far
type Candidate struct {
	Task                 string  `json:"task"`
	Volume               int     `json:"volume"`
	Status               string  `json:"status"`
	Model                string  `json:"model"`
	Private              bool    `json:"private"`
	PassRate             float64 `json:"pass_rate"`
	PassLower95          float64 `json:"pass_lower_95"`
	ExpectedCostUSD      float64 `json:"expected_cost_per_task_usd"` // try Model first, escalate to baseline on failure
	BaselinePerTaskUSD   float64 `json:"baseline_cost_per_task_usd"`
	SavingsPer1kTasksUSD float64 `json:"savings_per_1k_tasks_usd"`
	Note                 string  `json:"note"`
}

const (
	candMinVolume = 20
	candMinN      = 10
	candMinLower  = 0.90
)

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	out := []Candidate{}
	for _, t := range s.lrn.Snapshot() {
		if t.Volume < candMinVolume {
			continue
		}
		base := (t.AvgIn*s.baseline.cfg.InPerMTok + t.AvgOut*s.baseline.cfg.OutPerMTok) / 1e6
		c := Candidate{Task: t.Task, Volume: t.Volume, Status: "keep", Model: s.baseline.cfg.ID, BaselinePerTaskUSD: base}
		c.ExpectedCostUSD = base
		for _, a := range t.Arms {
			if a.Model == s.baseline.cfg.ID || a.N < candMinN {
				continue
			}
			// try this model; with probability (1-p) pay for it AND the baseline
			exp := a.MeanCost + (1-a.P)*base
			if exp < c.ExpectedCostUSD {
				c.Model, c.Private = a.Model, a.Private
				c.PassRate, c.PassLower95 = a.P, a.Lower
				c.ExpectedCostUSD = exp
				c.Status = "first-try"
				if a.Lower >= candMinLower {
					c.Status = "move"
				}
			}
		}
		c.SavingsPer1kTasksUSD = (base - c.ExpectedCostUSD) * 1000
		switch c.Status {
		case "move":
			c.Note = "quality bar met without escalation"
		case "first-try":
			c.Note = "cheaper in expectation if failures escalate to the frontier model"
		default:
			c.Note = "no cheaper option pays off yet"
		}
		if c.Private && c.Status != "keep" {
			c.Note += " (private/open model)"
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavingsPer1kTasksUSD > out[j].SavingsPer1kTasksUSD })
	writeJSON(w, 200, map[string]any{
		"quality_bar": map[string]any{"min_observations": candMinN, "min_pass_lower_95": candMinLower},
		"candidates":  out,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "lagom_requests_total %d\n", s.reqTotal.Load())
	fmt.Fprintf(w, "lagom_cache_hits_total %d\n", s.hits.Load())
	fmt.Fprintf(w, "lagom_errors_total %d\n", s.errs.Load())
	fmt.Fprintf(w, "lagom_inflight %d\n", s.inflight.Load())
	fmt.Fprintf(w, "lagom_guard_flagged_total %d\n", s.guardFlagged.Load())
	fmt.Fprintf(w, "lagom_guard_blocked_total %d\n", s.guardBlocked.Load())
	fmt.Fprintf(w, "lagom_unclassified_total %d\n", s.unclassifiedN.Load())
	fmt.Fprintf(w, "lagom_spent_usd %f\n", s.spentUSD())
	fmt.Fprintf(w, "lagom_ledger_dropped_events_total %d\n", s.led.Dropped())
}

// ---- outcomes for tasks that had no inline verifier ----

type pendingEntry struct {
	task, model string
	in, out     int
	cost, lat   float64
}

type pendingStore struct {
	mu    sync.Mutex
	m     map[string]pendingEntry
	order []string
	cap   int
}

func newPending(cap int) *pendingStore { return &pendingStore{m: map[string]pendingEntry{}, cap: cap} }

func (p *pendingStore) put(id string, e pendingEntry) {
	p.mu.Lock()
	p.m[id] = e
	p.order = append(p.order, id)
	if len(p.order) > p.cap {
		delete(p.m, p.order[0])
		p.order = p.order[1:]
	}
	p.mu.Unlock()
}

func (p *pendingStore) take(id string) (pendingEntry, bool) {
	p.mu.Lock()
	e, ok := p.m[id]
	delete(p.m, id)
	p.mu.Unlock()
	return e, ok
}

// POST /v1/outcomes {"request_id":"...","success":true}
// Lets a customer report the real-world outcome later (test passed, ticket
// resolved...). The learner updates exactly as it does for inline verifiers.
func (s *Server) handleOutcome(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeErr(w, 401, "unauthorized")
		return
	}
	var in struct {
		RequestID string `json:"request_id"`
		Success   bool   `json:"success"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	p, ok := s.pending.take(in.RequestID)
	if !ok {
		writeErr(w, 404, "unknown or expired request_id")
		return
	}
	verdict := "fail"
	if in.Success {
		verdict = "pass"
	}
	s.led.Emit(ledger.Event{
		TS: time.Now(), Kind: "outcome", ID: in.RequestID, Task: p.task, Learn: true,
		Attempts: []ledger.Attempt{{Model: p.model, Verdict: verdict, InTok: p.in, OutTok: p.out, CostUSD: p.cost, LatencyMS: p.lat * 1000}},
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func init() { log.SetFlags(log.LstdFlags | log.Lmicroseconds) }

// handleReport serves the observation report: what the traffic costs today, what routing
// saves, and the breakdowns behind it. Figures describe the observed window and are not an invoice.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	tiers := map[string]string{}
	for i, m := range s.chain {
		switch i {
		case 0:
			tiers[m.cfg.ID] = "small"
		case len(s.chain) - 1:
			tiers[m.cfg.ID] = "premium"
		default:
			tiers[m.cfg.ID] = "mid"
		}
	}
	tiers[s.baseline.cfg.ID] = "premium"
	rc := s.cfg.Report
	writeJSON(w, http.StatusOK, s.obs.View(ledger.ViewOpts{Currency: rc.Currency, Rate: rc.USDToCurrency, FeeRate: rc.FeeRate, Tiers: tiers, Names: rc.Workloads}))
}
