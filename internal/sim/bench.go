// Command bench measures what the gateway adds to a request.
//
// It sends the same load twice: directly to the upstream (the mock LLM with
// zero simulated latency) and through the gateway, then reports the latency
// distribution of each and the difference. Because the upstream does no work,
// the delta is the gateway's own cost: parsing, routing, accounting, streaming
// proxying and the extra network hop.
//
// Caveat to state when quoting numbers: client, gateway and upstream share one
// machine's CPUs here, so absolute figures are pessimistic for the gateway under
// high concurrency and optimistic for network cost (loopback).
package sim

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func Bench(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	gw := fs.String("gateway", "http://127.0.0.1:8080", "gateway base URL")
	direct := fs.String("direct", "http://127.0.0.1:9100", "upstream base URL (no /v1)")
	conc := fs.Int("c", 256, "concurrent clients")
	n := fs.Int("n", 50000, "requests per target")
	stream := fs.Bool("stream", false, "use streaming")
	model := fs.String("model", "mock-instant", "model id (must exist on both)")
	out := fs.String("out", "", "append a markdown summary to this file")
	fs.Parse(args)

	body := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"ping"}]}`, *model, *stream)

	hc := &http.Client{Transport: &http.Transport{
		MaxIdleConns: *conc * 2, MaxIdleConnsPerHost: *conc * 2, DisableCompression: true,
	}}
	fire := func(url string, count int, hdr map[string]string) []time.Duration {
		lat := make([]time.Duration, 0, count)
		var mu sync.Mutex
		var wg sync.WaitGroup
		jobs := make(chan struct{}, *conc)
		for i := 0; i < *conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]time.Duration, 0, count / *conc + 1)
				for range jobs {
					req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
					req.Header.Set("Content-Type", "application/json")
					for k, v := range hdr {
						req.Header.Set(k, v)
					}
					t := time.Now()
					resp, err := hc.Do(req)
					if err != nil {
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode == 200 {
						local = append(local, time.Since(t))
					}
				}
				mu.Lock()
				lat = append(lat, local...)
				mu.Unlock()
			}()
		}
		for i := 0; i < count; i++ {
			jobs <- struct{}{}
		}
		close(jobs)
		wg.Wait()
		return lat
	}

	dURL := strings.TrimRight(*direct, "/") + "/v1/chat/completions"
	gURL := strings.TrimRight(*gw, "/") + "/v1/chat/completions"
	gHdr := map[string]string{"X-Lagom-Run": "bench"}

	fire(dURL, 2000, nil) // warm up connections and JIT-ish caches
	fire(gURL, 2000, gHdr)

	t0 := time.Now()
	dl := fire(dURL, *n, nil)
	dDur := time.Since(t0)
	t0 = time.Now()
	gl := fire(gURL, *n, gHdr)
	gDur := time.Since(t0)

	if len(dl) == 0 || len(gl) == 0 {
		fmt.Fprintln(os.Stderr, "no successful requests; are the gateway and mockllm running (and is the model configured)?")
		os.Exit(1)
	}
	row := func(name string, l []time.Duration, d time.Duration) []float64 {
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		p := func(q float64) float64 { return float64(l[int(float64(len(l)-1)*q)].Microseconds()) }
		v := []float64{float64(len(l)) / d.Seconds(), p(.50), p(.95), p(.99), p(.999)}
		fmt.Printf("%-18s %9.0f req/s   p50 %6.0f µs   p95 %6.0f µs   p99 %6.0f µs   p99.9 %6.0f µs\n", name, v[0], v[1], v[2], v[3], v[4])
		return v
	}
	mode := "non-streaming"
	if *stream {
		mode = "streaming (SSE)"
	}
	fmt.Printf("\n%s, concurrency %d, %d requests per target\n", mode, *conc, *n)
	dv := row("direct to upstream", dl, dDur)
	gv := row("via gateway", gl, gDur)
	fmt.Printf("%-18s %19s   p50 %+6.0f µs   p95 %+6.0f µs   p99 %+6.0f µs   p99.9 %+6.0f µs\n",
		"gateway adds", "", gv[1]-dv[1], gv[2]-dv[2], gv[3]-dv[3], gv[4]-dv[4])

	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			defer f.Close()
			fmt.Fprintf(f, "| %s | %d | %d | %.0f | %.0f | %+.0f | %+.0f | %+.0f |\n",
				mode, *conc, *n, gv[0], dv[0], gv[1]-dv[1], gv[3]-dv[3], gv[4]-dv[4])
		}
	}
}
