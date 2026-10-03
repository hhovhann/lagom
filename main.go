// Command lagom is the whole product in one binary:
//
//	lagom demo    simulator + gateway + live demo page (no keys, no network)
//	lagom serve   the gateway, with the config you choose
//	lagom mock    the LLM simulator on its own
//	lagom check   one tiny request per model, before any real spend
//	lagom assess  find the cheapest model proven to clear your quality bar, per workload
//	lagom traffic send checked sample traffic through a running gateway (fills /report)
//	lagom eval    run the 4-way comparison against a running gateway
//	lagom bench   measure gateway overhead against a running gateway
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"lagom/internal/assess"
	"lagom/internal/config"
	"lagom/internal/gateway"
	"lagom/internal/sim"
)

const usage = `usage: lagom <command> [flags]

  demo    start the simulator and the gateway, open the live demo   (no API keys needed)
  serve   start the gateway:  lagom serve -config config/local.json
  mock    start only the LLM simulator
  check   one tiny request per model: does the key, the model id and the price work?
  assess  per workload, find the cheapest model proven to clear your quality bar
  traffic send checked sample traffic through a gateway; see the result at /report
  eval    compare "always biggest model" vs Lagom on a running gateway
  bench   measure the gateway's own overhead

Keys go in a .env file (see .env.example). Run "lagom <command> -h" for flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		serve(args)
	case "demo":
		demo(args)
	case "mock":
		mock(args)
	case "assess":
		assess.Command(args)
	case "check":
		assess.CheckCommand(args)
	case "traffic":
		sim.Traffic(args)
	case "eval":
		sim.Eval(args)
	case "bench":
		sim.Bench(args)
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, usage)
		os.Exit(2)
	}
}

func mock(args []string) {
	fs := flag.NewFlagSet("mock", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9100", "listen address")
	scale := fs.Float64("latency-scale", 1, "multiply simulated latencies (0 = no delay)")
	fs.Parse(args)
	log.Printf("mock LLM (SIMULATOR) listening on %s, latency scale %.2f", *addr, *scale)
	log.Fatal(http.ListenAndServe(*addr, (&sim.MockServer{LatencyScale: *scale}).Handler()))
}

// demo runs the simulator in-process next to the gateway and opens the live page.
func demo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	cfg := fs.String("config", "config/mock.json", "gateway config (must point at the simulator on :9100)")
	scale := fs.Float64("latency-scale", 1, "multiply simulated latencies (0 = no delay)")
	fs.Parse(args)
	// A clean ledger for this demo only: never touch other configs' ledgers (they hold learned state).
	if c, err := config.Load(*cfg); err == nil && c.LedgerPath != "" {
		os.Remove(c.LedgerPath)
	}
	go func() {
		log.Fatal(http.ListenAndServe("127.0.0.1:9100", (&sim.MockServer{LatencyScale: *scale}).Handler()))
	}()
	go func() {
		time.Sleep(700 * time.Millisecond)
		fmt.Println("\n  Live demo   http://localhost:8080/\n  Dashboard   http://localhost:8080/dashboard\n  Ctrl-C to stop.")
		openBrowser("http://localhost:8080/")
	}()
	serve([]string{"-config", *cfg})
}

func openBrowser(url string) {
	name := map[string]string{"darwin": "open", "linux": "xdg-open", "windows": "explorer"}[runtime.GOOS]
	if name != "" {
		exec.Command(name, url).Start()
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config/mock.json", "path to config JSON")
	envPath := fs.String("env", ".env", "optional file with API keys (KEY=VALUE per line)")
	fs.Parse(args)

	if err := config.LoadDotEnv(*envPath); err != nil {
		log.Fatalf("env: %v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	s, err := gateway.New(cfg)
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // whole request incl. body; does not cut streaming responses
		MaxHeaderBytes:    64 << 10,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("lagom gateway on %s (config %s, chain %v, guard %s)", cfg.Listen, *cfgPath, cfg.Chain, cfg.Guard)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	s.Close()
}
