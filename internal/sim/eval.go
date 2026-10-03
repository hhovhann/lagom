// Command eval runs the same workload four times through the gateway:
//
//	baseline   every task goes straight to the frontier model (what a customer does today)
//	static     cheapest-first cascade with verification, no learning (ablation)
//	routing    model "auto", cache OFF: learned start model + verified escalation
//	full       model "auto", cache on
//
// All runs use the same verifiers, so quality is measured the same way, and all
// are recorded by the same ledger, so cost and latency are comparable. Splitting
// "routing" from "full" keeps the cache from flattering the routing result.
package sim

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type seriesPt struct {
	I               int     `json:"i"`
	CostPerSucc     float64 `json:"cost_per_success"`
	SuccessRate     float64 `json:"success_rate"`
	ExecCostPerTask float64 `json:"exec_cost_per_task"`
	EscalationRate  float64 `json:"escalation_rate"`
}

type runStats struct {
	Name           string                    `json:"name"`
	Tasks          int                       `json:"tasks"`
	Successes      int                       `json:"successes"`
	SuccessRate    float64                   `json:"success_rate"`
	CostUSD        float64                   `json:"cost_usd"`
	CostPerSuccess float64                   `json:"cost_per_success_usd"`
	BaselineUSD    float64                   `json:"baseline_usd"`
	SavedUSD       float64                   `json:"saved_usd"`
	CacheHitRate   float64                   `json:"cache_hit_rate"`
	EscalationRate float64                   `json:"escalation_rate"`
	LatP50         float64                   `json:"lat_p50_ms"`
	LatP95         float64                   `json:"lat_p95_ms"`
	OverP50        float64                   `json:"overhead_p50_us"`
	OverP99        float64                   `json:"overhead_p99_us"`
	Mix            map[string]map[string]int `json:"mix"`
	Series         []seriesPt                `json:"series"`
	StartMix       map[string]struct {
		Early map[string]int `json:"early"`
		Late  map[string]int `json:"late"`
	} `json:"start_mix"`
}

type statsResp struct {
	Ledger struct {
		Runs map[string]runStats `json:"runs"`
	} `json:"ledger"`
	Baseline  string  `json:"baseline"`
	Simulated bool    `json:"simulated"`
	SpentUSD  float64 `json:"spent_usd"`
	BudgetUSD float64 `json:"budget_usd"`
}

type candidate struct {
	Task                 string  `json:"task"`
	Volume               int     `json:"volume"`
	Status               string  `json:"status"`
	Model                string  `json:"model"`
	Private              bool    `json:"private"`
	PassRate             float64 `json:"pass_rate"`
	PassLower95          float64 `json:"pass_lower_95"`
	SavingsPer1kTasksUSD float64 `json:"savings_per_1k_tasks_usd"`
}

type candidates struct {
	Candidates []candidate `json:"candidates"`
}

func Eval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	gw := fs.String("gateway", "http://127.0.0.1:8080", "gateway base URL")
	n := fs.Int("n", 300, "number of tasks")
	c := fs.Int("c", 8, "concurrency")
	seed := fs.Uint64("seed", 1, "workload seed")
	baseline := fs.String("baseline", "", "baseline model id (default: the gateway's baseline)")
	out := fs.String("out", "", "write a markdown report to this path")
	fs.Parse(args)

	var st statsResp
	mustGet(*gw+"/v1/stats", &st)
	base := *baseline
	if base == "" {
		base = st.Baseline
	}
	tasks := Generate(*n, *seed)
	mix := map[string]int{}
	seen := map[string]bool{}
	repeats := 0
	for _, t := range tasks {
		mix[t.Type]++
		if seen[t.Prompt] {
			repeats++
		}
		seen[t.Prompt] = true
	}
	fmt.Printf("workload: %d tasks %v seed %d, %d exact repeats, concurrency %d, baseline model %s\n",
		*n, mix, *seed, repeats, *c, base)
	if st.Simulated {
		fmt.Println("MODE: SIMULATION (mock LLM). Mechanism and overhead only; not evidence of real-world savings.")
	}

	mustPost(*gw + "/v1/admin/reset?scope=all")
	fmt.Print("1/4 baseline (always frontier)        ... ")
	fmt.Printf("done (%d http errors)\n", run(*gw, "baseline", base, tasks, *c, false, ""))

	mustPost(*gw + "/v1/admin/reset?scope=learner") // keep ledger, clear learner and cache
	fmt.Print("2/4 static cascade, cheapest first    ... ")
	fmt.Printf("done (%d http errors)\n", run(*gw, "static", "auto", tasks, *c, true, "static"))

	mustPost(*gw + "/v1/admin/reset?scope=learner")
	fmt.Print("3/4 learned routing (cache off)       ... ")
	fmt.Printf("done (%d http errors)\n", run(*gw, "routing", "auto", tasks, *c, true, ""))

	mustPost(*gw + "/v1/admin/reset?scope=learner")
	fmt.Print("4/4 learned routing + cache           ... ")
	fmt.Printf("done (%d http errors)\n", run(*gw, "full", "auto", tasks, *c, false, ""))
	time.Sleep(600 * time.Millisecond) // let the ledger drain

	mustGet(*gw+"/v1/stats", &st)
	var cand candidates
	mustGet(*gw+"/v1/report/candidates", &cand)

	fmt.Printf("\nSpend recorded by the gateway this session: $%.4f (budget cap %s)\n", st.SpentUSD, map[bool]string{true: "none", false: fmt.Sprintf("$%.2f", st.BudgetUSD)}[st.BudgetUSD == 0])
	rep := report(st, cand, base, *n, mix, repeats, *seed)
	fmt.Println()
	fmt.Println(rep)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(rep), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
		} else {
			fmt.Println("report written to", *out)
		}
	}
}

func run(gw, runName, model string, tasks []Task, conc int, noCache bool, policy string) int64 {
	hc := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: conc * 2}}
	var errs atomic.Int64
	ch := make(chan Task)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				body, _ := json.Marshal(map[string]any{
					"model":    model,
					"messages": []map[string]string{{"role": "user", "content": t.Prompt}},
				})
				req, _ := http.NewRequest("POST", gw+"/v1/chat/completions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Lagom-Run", runName)
				req.Header.Set("X-Lagom-Task", t.Type)
				req.Header.Set("X-Lagom-Verify", t.Verify)
				if noCache {
					req.Header.Set("X-Lagom-No-Cache", "1")
				}
				if policy != "" {
					req.Header.Set("X-Lagom-Policy", policy)
				}
				// simulator hints; real providers never see these
				req.Header.Set("X-Lagom-Mock-Task", t.Type)
				req.Header.Set("X-Lagom-Mock-Gold", t.Gold)
				req.Header.Set("X-Lagom-Mock-Wrong", t.Wrong)
				resp, err := hc.Do(req)
				if err != nil {
					errs.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					errs.Add(1)
				}
			}
		}()
	}
	for _, t := range tasks {
		ch <- t
	}
	close(ch)
	wg.Wait()
	return errs.Load()
}

func mustGet(url string, v any) {
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot reach gateway:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		fmt.Fprintln(os.Stderr, "decode", url, err)
		os.Exit(1)
	}
}

func mustPost(url string) {
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot reach gateway:", err)
		os.Exit(1)
	}
	resp.Body.Close()
}

func pctChange(base, v float64) string {
	if base == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", (v-base)/base*100)
}

func fmtMix(m map[string]int) string {
	if len(m) == 0 {
		return "–"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s×%d", strings.TrimPrefix(k, "mock-"), m[k])
	}
	return strings.Join(parts, " ")
}

func report(st statsResp, cand candidates, base string, n int, mix map[string]int, repeats int, seed uint64) string {
	b, sc, r, f := st.Ledger.Runs["baseline"], st.Ledger.Runs["static"], st.Ledger.Runs["routing"], st.Ledger.Runs["full"]
	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }

	w("# Lagom results\n\n")
	if st.Simulated {
		w("> **SIMULATION.** The upstream models are the bundled mock LLM server. This shows the mechanism, the\n")
		w("> routing behaviour and the gateway overhead. Dollar figures use the configured placeholder prices and are\n")
		w("> not evidence of real-world savings. Re-run with `config/real.json` for real-model numbers.\n\n")
	} else {
		w("> **REAL MODELS.** Token counts, latencies and answers come from the live models behind the gateway. Dollar\n")
		w("> figures multiply real token counts by the prices in the config; prices for local/private models are\n")
		w("> amortised placeholders, so read cost as \"relative tokens\" there. Workload: synthetic, machine-checkable tasks.\n\n")
	}
	w("Workload: %d tasks %v (seed %d); %d (%.0f%%) are exact repeats of an earlier task. Baseline = every task on `%s`.\n",
		n, mix, seed, repeats, float64(repeats)/float64(n)*100, base)
	w("Every run is graded by the same deterministic verifiers.\n\n")

	w("| Metric | Baseline (always `%s`) | Static cascade | Learned routing | Learned + cache |\n|---|---:|---:|---:|---:|\n", base)
	w("| Success rate (verified) | %.1f%% | %.1f%% | %.1f%% | %.1f%% |\n", b.SuccessRate*100, sc.SuccessRate*100, r.SuccessRate*100, f.SuccessRate*100)
	w("| Total cost | $%.4f | $%.4f | $%.4f | $%.4f |\n", b.CostUSD, sc.CostUSD, r.CostUSD, f.CostUSD)
	w("| **Cost per successful task** | $%.5f | $%.5f (%s) | $%.5f (**%s**) | $%.5f (**%s**) |\n",
		b.CostPerSuccess, sc.CostPerSuccess, pctChange(b.CostPerSuccess, sc.CostPerSuccess),
		r.CostPerSuccess, pctChange(b.CostPerSuccess, r.CostPerSuccess),
		f.CostPerSuccess, pctChange(b.CostPerSuccess, f.CostPerSuccess))
	w("| Latency p50 | %.0f ms | %.0f ms | %.0f ms | %.0f ms |\n", b.LatP50, sc.LatP50, r.LatP50, f.LatP50)
	w("| Latency p95 | %.0f ms | %.0f ms | %.0f ms | %.0f ms |\n", b.LatP95, sc.LatP95, r.LatP95, f.LatP95)
	w("| Cache hit rate | – | – | – | %.1f%% |\n", f.CacheHitRate*100)
	w("| Executed tasks that escalated | – | %.1f%% | %.1f%% | %.1f%% |\n", sc.EscalationRate*100, r.EscalationRate*100, f.EscalationRate*100)
	w("| Gateway overhead excl. upstream, p50 / p99 | %.0f / %.0f µs | %.0f / %.0f µs | %.0f / %.0f µs | %.0f / %.0f µs |\n\n",
		b.OverP50, b.OverP99, sc.OverP50, sc.OverP99, r.OverP50, r.OverP99, f.OverP50, f.OverP99)

	w("**How to read this.** *Static cascade* is the ablation: always start on the cheapest model and escalate on failure, no learning.\n")
	w("*Learned routing* (cache off) vs *static cascade* isolates what learning adds; vs *baseline* it is the total routing effect. Same prompts, quality graded identically.\n")
	w("\"Learned + cache\" adds the exact-match cache, so it also depends on how repetitive the workload is (here %.0f%% repeats).\n", float64(repeats)/float64(n)*100)
	w("Cost per successful task counts every attempt, including failed cheap attempts and any judge calls.\n\n")
	w("Savings method: `saved = cost the final answer would have had on %s (same tokens) − actual cost`; the ledger sums this per request.\n", base)
	w("Ledger estimate for the cache run: $%.4f saved of $%.4f counterfactual. The measured table above is the like-for-like check.\n\n", f.SavedUSD, f.BaselineUSD)
	if b.CostPerSuccess > 0 {
		w("Projection per 100,000 tasks at this mix: baseline $%.0f, static cascade $%.0f, learned $%.0f, learned + cache $%.0f.\n\n",
			b.CostPerSuccess*1e5, sc.CostPerSuccess*1e5, r.CostPerSuccess*1e5, f.CostPerSuccess*1e5)
	}

	w("## Does it learn? (routing-only run, cache off)\n\n")
	w("Which model each task class **started** on, first third of the run vs last third:\n\n| Task | Early starts | Late starts |\n|---|---|---|\n")
	tasks := make([]string, 0, len(r.StartMix))
	for t := range r.StartMix {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)
	for _, t := range tasks {
		w("| %s | %s | %s |\n", t, fmtMix(r.StartMix[t].Early), fmtMix(r.StartMix[t].Late))
	}
	w("\nCost and escalations per executed task, by window of tasks:\n\n| Tasks seen | $/executed task | escalation rate | success |\n|---:|---:|---:|---:|\n")
	for _, p := range r.Series {
		w("| %d | %.5f | %.0f%% | %.0f%% |\n", p.I, p.ExecCostPerTask, p.EscalationRate*100, p.SuccessRate*100)
	}

	w("\n## Which workloads should move off the frontier model?\n\n")
	w("`move`: a cheaper model clears the bar alone (95%% lower bound on pass rate ≥ 90%%). `first-try`: cheaper in expectation if failures escalate to `%s`.\n\n", base)
	w("| Task | Volume | Status | Model | Pass rate (95%% lower) | Saving per 1k tasks |\n|---|---:|---|---|---:|---:|\n")
	for _, c := range cand.Candidates {
		if c.Status == "keep" {
			w("| %s | %d | keep | `%s` | – | – |\n", c.Task, c.Volume, c.Model)
			continue
		}
		priv := ""
		if c.Private {
			priv = " (private)"
		}
		w("| %s | %d | %s | `%s`%s | %.0f%% (%.0f%%) | $%.2f |\n", c.Task, c.Volume, c.Status, c.Model, priv, c.PassRate*100, c.PassLower95*100, c.SavingsPer1kTasksUSD)
	}
	return sb.String()
}
