package router

import (
	"context"
	"hash/fnv"
	"strings"
	"testing"
)

// bagEmbedder is a deterministic stand-in: a hashed bag of words, so texts that share words are similar.
type bagEmbedder struct{}

func (bagEmbedder) Embed(_ context.Context, _ string, text string) ([]float64, error) {
	v := make([]float64, 4096)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%4096]++
	}
	return v, nil
}

var refs = map[string][]string{
	"classify": {"label this support ticket as billing bug or feature", "classify the customer request into a category"},
	"extract":  {"extract the invoice number vendor and total as json", "pull the fields out of this invoice"},
}

func TestClassifierPlacesConfidentPromptsAndRefusesGuesses(t *testing.T) {
	c, err := NewClassifier(context.Background(), bagEmbedder{}, "m", refs, 0.3, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ text, want string }{
		{"please classify this customer request into a category", "classify"},
		{"extract the vendor and invoice total as json", "extract"},
		{"write a haiku about autumn moonlight", ""}, // nothing like any reference: unsure
	}
	for _, tc := range cases {
		got, err := c.Classify(context.Background(), tc.text)
		if err != nil || got.Task != tc.want {
			t.Errorf("Classify(%q) = %+v, %v; want %q", tc.text, got, err, tc.want)
		}
	}
}

func TestClassifierNeedsTwoTypesAndExamples(t *testing.T) {
	if _, err := NewClassifier(context.Background(), bagEmbedder{}, "m", map[string][]string{"a": {"x"}}, 0.3, 0.1); err == nil {
		t.Error("one task type cannot be told apart from anything")
	}
	if _, err := NewClassifier(context.Background(), bagEmbedder{}, "m", map[string][]string{"a": {"x"}, "b": {}}, 0.3, 0.1); err == nil {
		t.Error("a type without examples must be rejected")
	}
}
