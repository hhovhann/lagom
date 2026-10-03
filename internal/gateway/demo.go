package gateway

import (
	_ "embed"
	"net/http"
	"strconv"

	"lagom/internal/sim"
)

//go:embed demo.html
var demoHTML []byte

// handleDemoTasks serves a deterministic task mix to the live demo page.
// GET /v1/demo/tasks?n=30&seed=7
func (s *Server) handleDemoTasks(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n < 1 {
		n = 1
	}
	if n > 200 {
		n = 200
	}
	seed, _ := strconv.ParseUint(r.URL.Query().Get("seed"), 10, 64)
	if seed == 0 {
		seed = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":     sim.Generate(n, seed),
		"baseline":  s.baseline.cfg.ID,
		"simulated": s.simulated,
		"chain":     s.chainIDs(),
	})
}

func (s *Server) chainIDs() []string {
	ids := make([]string, len(s.chain))
	for i, m := range s.chain {
		ids[i] = m.cfg.ID
	}
	return ids
}
