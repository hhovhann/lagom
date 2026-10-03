// Package verify turns an outcome specification into a pass/fail check.
//
// A spec is "kind:argument" and arrives in the X-Lagom-Verify request header.
// These are the deterministic checks. The "judge:" kind needs a model call and
// is handled by the gateway; Parse reports it via IsJudge.
//
//	equals:<text>        trimmed, case-insensitive, trailing punctuation ignored
//	contains:<text>      case-insensitive substring
//	oneof:a|b|c          output is exactly one of the labels
//	regex:<re>           regular expression matches
//	number:<n>           the last number in the output equals n
//	json:k1,k2           output is a JSON object containing these keys
//	jsoneq:<json>        output is a JSON object that contains every field of <json>
//	fields:min=0.9;tol=0.005:<json>   at least 90% of the fields of <json> are right (partial credit)
//	rows:key=unit;min=0.9;tol=0.005:<json array>   rows matched by key column(s), cells compared
//	rules:maxwords=20;must=term;ban=term   hard gates: word limit, required and banned terms (repeatable)
//	judge:<rubric>       LLM judge (gateway)
package router

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

type Checker func(output string) bool

// Spec is a parsed verification specification.
type Spec struct {
	Raw    string
	Check  Checker // nil for judge specs
	Judge  bool
	Rubric string
}

func (s *Spec) IsJudge() bool { return s != nil && s.Judge }

// maxSpecLen bounds the verifier header. The spec is compiled on every request (a regex
// especially), so an unbounded one would let a caller burn CPU.
const maxSpecLen = 2048

// maxGoldSpecLen is the larger bound for fields and rows specs, which carry the whole expected answer.
const maxGoldSpecLen = 32 << 10

// Parse returns nil, nil for an empty spec (no verification requested).
func Parse(raw string) (*Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	kind, arg, ok := strings.Cut(raw, ":")
	if !ok {
		return nil, fmt.Errorf("verify spec %q: want kind:argument", raw)
	}
	limit := maxSpecLen
	if kind == "fields" || kind == "rows" {
		limit = maxGoldSpecLen
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("verify spec is %d bytes; the limit is %d", len(raw), limit)
	}
	sp := &Spec{Raw: raw}
	switch kind {
	case "equals":
		want := norm(arg)
		sp.Check = func(o string) bool { return norm(o) == want }
	case "contains":
		want := strings.ToLower(arg)
		sp.Check = func(o string) bool { return strings.Contains(strings.ToLower(o), want) }
	case "oneof":
		set := map[string]bool{}
		for _, l := range strings.Split(arg, "|") {
			set[norm(l)] = true
		}
		sp.Check = func(o string) bool { return set[norm(o)] }
	case "regex":
		re, err := regexp.Compile(arg)
		if err != nil {
			return nil, fmt.Errorf("verify regex: %w", err)
		}
		sp.Check = func(o string) bool { return re.MatchString(o) }
	case "number":
		want, err := strconv.ParseFloat(strings.TrimSpace(arg), 64)
		if err != nil {
			return nil, fmt.Errorf("verify number: %w", err)
		}
		sp.Check = func(o string) bool {
			got, ok := lastNumber(o)
			return ok && math.Abs(got-want) < 1e-6
		}
	case "json":
		keys := strings.Split(arg, ",")
		sp.Check = func(o string) bool {
			m, ok := parseObject(o)
			if !ok {
				return false
			}
			for _, k := range keys {
				if _, has := m[strings.TrimSpace(k)]; !has {
					return false
				}
			}
			return true
		}
	case "jsoneq":
		var want map[string]any
		if err := json.Unmarshal([]byte(arg), &want); err != nil {
			return nil, fmt.Errorf("verify jsoneq: %w", err)
		}
		sp.Check = func(o string) bool {
			got, ok := parseObject(o)
			if !ok {
				return false
			}
			for k, w := range want {
				g, has := got[k]
				if !has || !looseEqual(g, w) {
					return false
				}
			}
			return true
		}
	case "fields":
		check, err := parseFields(arg)
		if err != nil {
			return nil, err
		}
		sp.Check = check
	case "rows":
		check, err := parseRows(arg)
		if err != nil {
			return nil, err
		}
		sp.Check = check
	case "rules":
		check, err := parseRules(arg)
		if err != nil {
			return nil, err
		}
		sp.Check = check
	case "judge":
		sp.Judge, sp.Rubric = true, arg
	default:
		return nil, fmt.Errorf("verify spec: unknown kind %q", kind)
	}
	return sp, nil
}

func norm(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"' \n\t")
	s = strings.TrimRight(s, ".!")
	return strings.ToLower(strings.TrimSpace(s))
}

var numRe = regexp.MustCompile(`-?\d[\d,]*\.?\d*`)

func lastNumber(s string) (float64, bool) {
	all := numRe.FindAllString(s, -1)
	for i := len(all) - 1; i >= 0; i-- {
		t := strings.TrimRight(strings.ReplaceAll(all[i], ",", ""), ".")
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// parseObject extracts a JSON object from model output, tolerating code fences
// and surrounding prose.
func parseObject(s string) (map[string]any, bool) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s[i:j+1]), &m); err != nil {
		return nil, false
	}
	return m, true
}

func looseEqual(got, want any) bool {
	switch w := want.(type) {
	case float64:
		switch g := got.(type) {
		case float64:
			return math.Abs(g-w) < 1e-6
		case string:
			f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(g), ",", ""), 64)
			return err == nil && math.Abs(f-w) < 1e-6
		}
		return false
	case string:
		g, ok := got.(string)
		return ok && norm(g) == norm(w)
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// parseRules builds the hard-gate check for open-ended answers: "maxwords=N" caps the
// length, "must=term" requires a term, "ban=term" forbids one (terms are case-insensitive;
// must and ban may repeat). All gates must hold.
func parseRules(arg string) (func(string) bool, error) {
	maxWords := 0
	var must, ban []string
	for _, part := range strings.Split(arg, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			return nil, fmt.Errorf("verify rules: %q is not key=value", part)
		}
		switch strings.TrimSpace(k) {
		case "maxwords":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("verify rules: maxwords must be a positive integer")
			}
			maxWords = n
		case "must":
			must = append(must, strings.ToLower(v))
		case "ban":
			ban = append(ban, strings.ToLower(v))
		default:
			return nil, fmt.Errorf("verify rules: unknown rule %q (use maxwords, must, ban)", k)
		}
	}
	if maxWords == 0 && len(must) == 0 && len(ban) == 0 {
		return nil, fmt.Errorf("verify rules: give at least one rule")
	}
	return func(o string) bool {
		if maxWords > 0 && len(strings.Fields(o)) > maxWords {
			return false
		}
		low := strings.ToLower(o)
		for _, m := range must {
			if !strings.Contains(low, m) {
				return false
			}
		}
		for _, b := range ban {
			if strings.Contains(low, b) {
				return false
			}
		}
		return true
	}, nil
}
