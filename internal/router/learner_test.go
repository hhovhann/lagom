package router

import (
	"math/rand/v2"
	"testing"
)

// simulate runs the cascade logic against a world with known success
// probabilities, feeding every attempt back to the learner.
func simulate(l *Learner, task string, trueP []float64, rounds int) {
	for i := 0; i < rounds; i++ {
		start := l.Choose(task)
		for j := start; j < len(trueP); j++ {
			ok := rand.Float64() < trueP[j]
			m := l.chain[j]
			cost := (priorIn*m.InPerMTok + priorOut*m.OutPerMTok) / 1e6
			l.Observe(task, m.ID, ok, cost, 0.5, 250, 80, j == start)
			if ok {
				break
			}
		}
	}
}

func share(l *Learner, task string, idx, draws int) float64 {
	n := 0
	for i := 0; i < draws; i++ {
		if l.Choose(task) == idx {
			n++
		}
	}
	return float64(n) / float64(draws)
}

func chain() []LearnerModel {
	return []LearnerModel{
		{ID: "cheap", InPerMTok: 20, OutPerMTok: 20, PriorP: 0.7, PriorLatMS: 500},
		{ID: "mid", InPerMTok: 100, OutPerMTok: 200, PriorP: 0.7, PriorLatMS: 500},
		{ID: "big", InPerMTok: 1000, OutPerMTok: 2000, PriorP: 0.7, PriorLatMS: 500},
	}
}

func TestStartsCheapWhenCheapModelIsGood(t *testing.T) {
	l := NewLearner(chain(), LearnerConfig{FailPenaltyUSD: 0.5, PriorWeight: 2})
	simulate(l, "easy", []float64{0.95, 0.97, 0.99}, 400)
	if s := share(l, "easy", 0, 300); s < 0.85 {
		t.Fatalf("expected to start on cheap model for an easy task, got share %.2f", s)
	}
}

func TestSkipsCheapModelThatKeepsFailing(t *testing.T) {
	l := NewLearner(chain(), LearnerConfig{FailPenaltyUSD: 0.5, PriorWeight: 2})
	simulate(l, "hard", []float64{0.02, 0.97, 0.99}, 400)
	if s := share(l, "hard", 0, 300); s > 0.15 {
		t.Fatalf("expected to stop starting on a model that almost always fails, got share %.2f", s)
	}
}

func TestTaskClassesAreIndependent(t *testing.T) {
	l := NewLearner(chain(), LearnerConfig{FailPenaltyUSD: 0.5, PriorWeight: 2})
	simulate(l, "easy", []float64{0.95, 0.97, 0.99}, 300)
	simulate(l, "hard", []float64{0.02, 0.97, 0.99}, 300)
	if share(l, "easy", 0, 200) < 0.8 || share(l, "hard", 0, 200) > 0.2 {
		t.Fatal("learning leaked across task classes")
	}
}

func TestSnapshotAndReset(t *testing.T) {
	l := NewLearner(chain(), LearnerConfig{FailPenaltyUSD: 0.5, PriorWeight: 2})
	simulate(l, "easy", []float64{0.9, 0.9, 0.9}, 50)
	snap := l.Snapshot()
	if len(snap) != 1 || snap[0].Volume != 50 {
		t.Fatalf("snapshot volume = %+v", snap)
	}
	l.Reset()
	if len(l.Snapshot()) != 0 {
		t.Fatal("reset did not clear state")
	}
}

func TestBetaSampleInRange(t *testing.T) {
	var sum float64
	const n = 20000
	for i := 0; i < n; i++ {
		x := betaSample(8, 2)
		if x < 0 || x > 1 {
			t.Fatalf("beta sample out of range: %v", x)
		}
		sum += x
	}
	if mean := sum / n; mean < 0.77 || mean > 0.83 { // true mean 0.8
		t.Fatalf("Beta(8,2) mean = %.3f, want ~0.8", mean)
	}
}

func TestQualifiedNeedsEvidenceAndAClearedBar(t *testing.T) {
	l := NewLearner([]LearnerModel{{ID: "cheap", PriorP: 0.5}, {ID: "mid", PriorP: 0.5}, {ID: "top", PriorP: 0.5}}, LearnerConfig{PriorWeight: 2})
	if i, ok := l.Qualified("t", 0.9, 10); ok || i != 2 {
		t.Fatalf("no evidence: want premium and not proven, got %d, %v", i, ok)
	}
	for i := 0; i < 40; i++ {
		l.Observe("t", "cheap", i%2 == 0, 0, 0, 0, 0, false) // 50% pass: fails the bar
		l.Observe("t", "mid", true, 0, 0, 0, 0, false)       // 100% over 40: lower bound ~0.91
	}
	if i, ok := l.Qualified("t", 0.9, 10); !ok || i != 1 {
		t.Fatalf("mid is the cheapest proven model, got %d, %v", i, ok)
	}
	if _, ok := l.Qualified("t", 0.99, 10); ok {
		t.Error("40 observations cannot prove a 99% bar")
	}
}

func TestMeanOutIgnoresErrorsAndNeedsObservations(t *testing.T) {
	l := NewLearner([]LearnerModel{{ID: "a", PriorP: 0.5}, {ID: "b", PriorP: 0.5}}, LearnerConfig{PriorWeight: 2})
	if _, n := l.MeanOut("t", "b"); n != 0 {
		t.Fatal("no observations yet")
	}
	l.Observe("t", "b", true, 0, 0, 50, 400, false)
	l.Observe("t", "b", true, 0, 0, 50, 600, false)
	l.Observe("t", "b", false, 0, 0, 0, 0, false) // a provider error reports no usage
	if mean, n := l.MeanOut("t", "b"); n != 2 || mean != 500 {
		t.Errorf("mean %v over %d attempts, want 500 over 2", mean, n)
	}
}
