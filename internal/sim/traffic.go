package sim

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"lagom/internal/config"
)

// Traffic implements `lagom traffic`: send n checked, synthetic requests with model
// "auto" through a running gateway, as an app would. The task type is NOT sent, so the
// gateway's classifier places each prompt. The results appear in /report.
func Traffic(args []string) {
	fs := flag.NewFlagSet("traffic", flag.ExitOnError)
	gw := fs.String("gateway", "http://127.0.0.1:8080", "gateway base URL")
	n := fs.Int("n", 60, "number of requests")
	c := fs.Int("c", 3, "concurrent requests")
	seed := fs.Uint64("seed", 9, "workload seed")
	fs.Parse(args)
	config.LoadDotEnv(".env")
	token := os.Getenv("LAGOM_API_KEY")

	hc := &http.Client{Timeout: 5 * time.Minute}
	var mu sync.Mutex
	var sent, pass, errs int
	var saved, cost float64
	var wg sync.WaitGroup
	ch := make(chan Task)
	for i := 0; i < *c; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				body, _ := json.Marshal(map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": t.Prompt}}})
				req, _ := http.NewRequest(http.MethodPost, *gw+"/v1/chat/completions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Lagom-Verify", t.Verify)
				req.Header.Set("X-Lagom-Mock-Task", t.Type) // simulator hints; real providers never see these
				req.Header.Set("X-Lagom-Mock-Gold", t.Gold)
				req.Header.Set("X-Lagom-Mock-Wrong", t.Wrong)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := hc.Do(req)
				mu.Lock()
				sent++
				if err != nil {
					errs++
					mu.Unlock()
					continue
				}
				var out struct {
					Lagom struct {
						Verdict string  `json:"verdict"`
						Cost    float64 `json:"cost_usd"`
						Saved   float64 `json:"saved_usd"`
					} `json:"lagom"`
				}
				json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs++
				} else if out.Lagom.Verdict == "pass" {
					pass++
				}
				cost += out.Lagom.Cost
				saved += out.Lagom.Saved
				mu.Unlock()
			}
		}()
	}
	for _, t := range Generate(*n, *seed) {
		ch <- t
	}
	close(ch)
	wg.Wait()
	fmt.Printf("sent %d requests: %d passed their check, %d errors; cost $%.5f, estimated saving $%.5f\nsee %s/report\n", sent, pass, errs, cost, saved, *gw)
}
