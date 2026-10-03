package assess

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lagom/internal/config"
	"lagom/internal/sim"
)

// Command implements `lagom assess`: run labelled examples through every model of a
// running gateway and print the workload analysis (the cheapest model proven per workload).
func Command(args []string) {
	fs := flag.NewFlagSet("assess", flag.ExitOnError)
	gw := fs.String("gateway", "http://127.0.0.1:8080", "gateway base URL")
	data := fs.String("data", "", "labelled examples, JSONL: {\"task\",\"prompt\",\"verify\"} per line")
	synthetic := fs.Int("synthetic", 0, "use N built-in synthetic examples instead of -data")
	seed := fs.Uint64("seed", 1, "seed for the synthetic examples")
	models := fs.String("models", "", "comma-separated model ids, cheapest first (default: the gateway's chain)")
	baseline := fs.String("baseline", "", "model used today (default: the last model)")
	bar := fs.Float64("bar", 0.95, "required pass rate, 0-1")
	bars := fs.String("bars", "", "per-workload bars, e.g. classify=0.98,extract=0.95")
	conc := fs.Int("c", 4, "concurrent calls")
	learn := fs.Bool("learn", false, "also teach the gateway's learner from these results (seeds routing)")
	out := fs.String("out", "", "write the report (markdown) to this file")
	jsonOut := fs.String("json", "", "write the raw results as JSON (the demo's recorded-sample format) to this file")
	fs.Parse(args)

	config.LoadDotEnv(".env")
	token := os.Getenv("LAGOM_API_KEY")

	var examples []Example
	switch {
	case *data != "":
		f, err := os.Open(*data)
		fatal(err)
		examples, err = LoadJSONLDir(f, filepath.Dir(*data))
		f.Close()
		fatal(err)
	case *synthetic > 0:
		for _, t := range sim.Generate(*synthetic, *seed) {
			examples = append(examples, Example{Task: t.Type, Prompt: t.Prompt, Verify: t.Verify})
		}
	default:
		fatal(fmt.Errorf("give -data FILE.jsonl or -synthetic N"))
	}

	order := splitList(*models)
	if len(order) == 0 {
		order = chainOf(*gw, token)
	}
	base := *baseline
	if base == "" {
		base = order[len(order)-1]
	}
	barMap, err := parseBars(*bars)
	fatal(err)

	fmt.Printf("assessing %d examples on %v (baseline %s, bar %.0f%%) ...\n", len(examples), order, base, *bar*100)
	r := &GatewayRunner{Base: *gw, Token: token, Learn: *learn, HC: &http.Client{Timeout: 10 * time.Minute}}
	results := Evaluate(context.Background(), r, order, examples, *conc)
	recs := Recommend(results, order, base, barMap, *bar)
	failures := Failures(results)
	if failures != "" {
		fmt.Print("\n" + failures)
	}

	report := "# Workload analysis\n\n" + Markdown(recs) + "\n" + failureCounts(results) +
		"A cheaper model qualifies only when the 95% lower bound on its pass rate reaches the required bar. " +
		"\"Need more examples\" means its raw rate reached the bar but the evidence is too thin to prove it.\n"
	fmt.Println("\n" + report)
	if *jsonOut != "" {
		models := modelViews(*gw, token, order)
		b, _ := json.MarshalIndent(map[string]any{"results": results, "models": models, "spent": 0, "cap": 0}, "", " ")
		fatal(os.WriteFile(*jsonOut, b, 0o644))
		fmt.Println("results written to", *jsonOut)
	}
	if *out != "" {
		fatal(os.WriteFile(*out, []byte(report), 0o644))
		fmt.Println("report written to", *out)
	}
}

// modelViews labels the tiers (cheapest = small, last = premium) and attaches the gateway's prices.
func modelViews(gw, token string, order []string) []map[string]any {
	req, _ := http.NewRequest(http.MethodGet, gw+"/v1/models", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	prices := map[string][2]float64{}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		defer resp.Body.Close()
		var m struct {
			Data []struct {
				ID  string  `json:"id"`
				In  float64 `json:"in_per_mtok"`
				Out float64 `json:"out_per_mtok"`
			} `json:"data"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m) == nil {
			for _, d := range m.Data {
				prices[d.ID] = [2]float64{d.In, d.Out}
			}
		}
	}
	var out []map[string]any
	for i, id := range order {
		label := "mid"
		switch i {
		case 0:
			label = "small"
		case len(order) - 1:
			label = "premium"
		}
		out = append(out, map[string]any{"id": id, "label": label, "in_per_mtok": prices[id][0], "out_per_mtok": prices[id][1]})
	}
	return out
}

func chainOf(gw, token string) []string {
	req, _ := http.NewRequest(http.MethodGet, gw+"/v1/models", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	fatal(json.Unmarshal(body, &m))
	var ids []string
	for _, d := range m.Data {
		if d.ID != "auto" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		fatal(fmt.Errorf("the gateway lists no models"))
	}
	return ids
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBars(s string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, p := range splitList(s) {
		k, v, ok := strings.Cut(p, "=")
		f, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil || f <= 0 || f > 1 {
			return nil, fmt.Errorf("bad -bars entry %q: want name=0.95", p)
		}
		out[k] = f
	}
	return out, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "assess:", err)
		os.Exit(1)
	}
}
