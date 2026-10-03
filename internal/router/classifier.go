package router

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// Embedder turns text into a vector (see providers.Embedder; the same shape).
type Embedder interface {
	Embed(ctx context.Context, model, text string) ([]float64, error)
}

// Classifier places a prompt into a task type by comparing its embedding with a few
// reference examples per type. It needs no labels or metadata on the request. When
// it is not confident (best similarity below minScore, or the best type beats the
// runner-up by less than minMargin) it returns "", and the caller holds the prompt on
// the most capable model instead of routing on a guess.
type Classifier struct {
	emb       Embedder
	model     string
	refs      map[string][][]float64
	minScore  float64
	minMargin float64
}

// NewClassifier embeds the reference examples once, at startup.
func NewClassifier(ctx context.Context, emb Embedder, model string, examples map[string][]string, minScore, minMargin float64) (*Classifier, error) {
	if len(examples) < 2 {
		return nil, fmt.Errorf("classifier needs examples for at least two task types")
	}
	c := &Classifier{emb: emb, model: model, refs: map[string][][]float64{}, minScore: minScore, minMargin: minMargin}
	for task, texts := range examples {
		for _, t := range texts {
			v, err := emb.Embed(ctx, model, t)
			if err != nil {
				return nil, fmt.Errorf("embed reference for %q: %w", task, err)
			}
			c.refs[task] = append(c.refs[task], v)
		}
		if len(c.refs[task]) == 0 {
			return nil, fmt.Errorf("task type %q has no examples", task)
		}
	}
	return c, nil
}

// Result is a classification with the evidence behind it.
type Result struct {
	Task   string  // "" when not confident
	Score  float64 // similarity to the best reference, -1..1
	Margin float64 // best score minus the best score of any other type
}

// Classify embeds text and returns the task type, or Task == "" when unsure.
func (c *Classifier) Classify(ctx context.Context, text string) (Result, error) {
	v, err := c.emb.Embed(ctx, c.model, text)
	if err != nil {
		return Result{}, err
	}
	type scored struct {
		task string
		s    float64
	}
	var all []scored
	for task, refs := range c.refs {
		best := -1.0
		for _, r := range refs {
			if s := cosine(v, r); s > best {
				best = s
			}
		}
		all = append(all, scored{task, best})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	res := Result{Score: all[0].s, Margin: all[0].s - all[1].s}
	if res.Score >= c.minScore && res.Margin >= c.minMargin {
		res.Task = all[0].task
	}
	return res, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
