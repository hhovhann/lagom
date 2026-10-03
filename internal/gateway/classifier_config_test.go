package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"lagom/internal/config"
	"lagom/internal/providers"
	"lagom/internal/sim"
)

// The shipped mock config's classifier must place the synthetic workload correctly,
// and when it is unsure it must say so rather than guess.
func TestShippedMockClassifierPlacesTheWorkload(t *testing.T) {
	up := httptest.NewServer((&sim.MockServer{}).Handler())
	defer up.Close()
	cfg, err := config.Load("../../config/mock.json")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["mock"]
	p.BaseURL = up.URL + "/v1"
	cfg.Providers["mock"] = p
	cfg.LedgerPath = ""
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	right, unsure, wrong, total := 0, 0, 0, 0
	for _, task := range sim.Generate(300, 3) {
		total++
		got := s.taskOf(context.Background(), "", []providers.Message{{Role: "user", Content: providers.Content(task.Prompt)}}, true)
		switch got {
		case task.Type:
			right++
		case unclassified:
			unsure++
		default:
			wrong++
			if wrong <= 3 {
				t.Logf("wrong: %s placed as %s: %.70q", task.Type, got, task.Prompt)
			}
		}
	}
	t.Logf("right %d, unsure %d, wrong %d of %d", right, unsure, wrong, total)
	if wrong*100 > total*3 || right*100 < total*85 {
		t.Errorf("classifier quality too low: right %d, unsure %d, wrong %d of %d", right, unsure, wrong, total)
	}
}
