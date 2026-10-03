// Package assess answers one question per workload: what is the cheapest model that is
// PROVEN to clear the required quality bar? It runs labelled examples through every
// model tier, scores each answer with the example's own check, and recommends the
// cheapest model whose 95% lower bound on its pass rate reaches the bar. Models are
// never chosen on price alone, and "keep the premium model" is a normal outcome.
//
// The core is pure: it talks to models only through the Runner interface, so the
// command line (through the gateway) and the live demo (directly to a provider)
// share it.
package assess

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"lagom/internal/providers"
	"lagom/internal/router"
)

// Example is one labelled unit of work: the prompt and the check that decides whether an answer is right.
type Example struct {
	Task   string `json:"task"`   // workload name, e.g. "classify"
	Prompt string `json:"prompt"` // what is sent to the model
	Verify string `json:"verify"` // success check, e.g. "equals:billing" (see the README for the kinds)

	// Files are PDFs or images (paths relative to the dataset file) sent with the prompt.
	Files       []string               `json:"files,omitempty"`
	Attachments []providers.Attachment `json:"-"` // Files, loaded and base64-encoded by LoadJSONL
}

// Outcome is what one model call produced.
type Outcome struct {
	Pass      bool
	CostUSD   float64
	LatencyMS float64
}

// Runner executes one example on one model.
type Runner interface {
	Run(ctx context.Context, model string, ex Example) (Outcome, error)
}

// ErrBudget tells Evaluate to stop calling models (a spend cap was reached). Examples
// that were not run are left out of the counts rather than counted as failures.
var ErrBudget = errors.New("spend cap reached")

// ModelResult is the measured quality, cost and speed of one model on one workload.
type ModelResult struct {
	Model       string  `json:"model"`
	Task        string  `json:"task"`
	N           int     `json:"n"`      // examples that were run
	Pass        int     `json:"pass"`   // examples that passed
	Errors      int     `json:"errors"` // calls that failed (counted as not passing)
	Rate        float64 `json:"rate"`   // Pass / N
	Lower       float64 `json:"lower"`  // 95% Wilson lower bound on the pass rate
	MeanCostUSD float64 `json:"mean_cost_usd"`
	MeanLatMS   float64 `json:"mean_lat_ms"`
	FirstError  string  `json:"first_error,omitempty"` // why the first failed call failed (shown in the terminal, not in the report)
}

type acc struct {
	n, pass, errs int
	cost, lat     float64
	firstErr      string
}

// Evaluate runs every example on every model with a bounded worker pool.
func Evaluate(ctx context.Context, r Runner, models []string, examples []Example, workers int) []ModelResult {
	if workers < 1 {
		workers = 1
	}
	type job struct {
		model string
		ex    Example
	}
	jobs := make(chan job)
	var mu sync.Mutex
	got := map[[2]string]*acc{}
	var budgetHit bool

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				mu.Lock()
				stop := budgetHit
				mu.Unlock()
				if stop {
					continue
				}
				out, err := r.Run(ctx, j.model, j.ex)
				if err != nil && !errors.Is(err, ErrBudget) && ctx.Err() == nil {
					time.Sleep(300 * time.Millisecond)
					out, err = r.Run(ctx, j.model, j.ex) // one retry: a transient provider error is not a wrong answer
				}
				mu.Lock()
				if errors.Is(err, ErrBudget) {
					budgetHit = true
					mu.Unlock()
					continue
				}
				k := [2]string{j.model, j.ex.Task}
				a := got[k]
				if a == nil {
					a = &acc{}
					got[k] = a
				}
				if err != nil {
					a.errs++
					if a.firstErr == "" {
						a.firstErr = err.Error()
					}
				} else {
					a.n++
					if out.Pass {
						a.pass++
					}
					a.cost += out.CostUSD
					a.lat += out.LatencyMS
				}
				mu.Unlock()
			}
		}()
	}
loop:
	for _, m := range models {
		for _, ex := range examples {
			select {
			case jobs <- job{m, ex}:
			case <-ctx.Done():
				break loop
			}
		}
	}
	close(jobs)
	wg.Wait()

	var res []ModelResult
	for k, a := range got {
		mr := ModelResult{Model: k[0], Task: k[1], N: a.n, Pass: a.pass, Errors: a.errs, FirstError: a.firstErr}
		if a.n > 0 {
			mr.Rate = float64(a.pass) / float64(a.n)
			mr.Lower = router.WilsonLower(float64(a.pass), a.n)
		}
		if a.n > 0 {
			mr.MeanCostUSD, mr.MeanLatMS = a.cost/float64(a.n), a.lat/float64(a.n)
		}
		res = append(res, mr)
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Task != res[j].Task {
			return res[i].Task < res[j].Task
		}
		return res[i].Model < res[j].Model
	})
	return res
}
