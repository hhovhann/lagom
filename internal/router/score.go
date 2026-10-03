package router

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Partial-credit checks for extraction work, where "right" is not all-or-nothing:
//
//	fields:min=0.9;tol=0.005:{"total_revenue":1250000,"noi":790000}
//	rows:key=suite;min=0.9;tol=0.005:[{"suite":"101","sqft":2400,"rent":58000},...]
//
// fields scores the share of expected fields the output got right. rows matches each
// expected row to an output row by the key column(s), then scores every other cell.
// A missing row loses all its cells; an extra row (one the key does not match) costs as
// many cells as an expected row has, so inventing rows is penalised, not ignored.
// The check passes when the score reaches min (default 1). tol is the relative tolerance
// for numbers (default 0.0005, i.e. 0.05%); strings compare as in jsoneq.

type scoreParams struct {
	min  float64
	tol  float64
	keys []string
}

func parseScoreParams(s string, needKey bool) (scoreParams, error) {
	p := scoreParams{min: 1, tol: 0.0005}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			return p, fmt.Errorf("%q is not key=value", part)
		}
		switch strings.TrimSpace(k) {
		case "min":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 || f > 1 {
				return p, fmt.Errorf("min must be a number in (0,1]")
			}
			p.min = f
		case "tol":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f >= 1 {
				return p, fmt.Errorf("tol must be a number in [0,1)")
			}
			p.tol = f
		case "key":
			if !needKey {
				return p, fmt.Errorf("key is only for rows")
			}
			for _, c := range strings.Split(v, ",") {
				if c = strings.TrimSpace(c); c != "" {
					p.keys = append(p.keys, c)
				}
			}
		default:
			return p, fmt.Errorf("unknown parameter %q", k)
		}
	}
	if needKey && len(p.keys) == 0 {
		return p, fmt.Errorf("rows needs key=<column>[,<column>]")
	}
	return p, nil
}

func splitParams(arg string) (params, body string, err error) {
	params, body, ok := strings.Cut(arg, ":")
	if !ok || strings.TrimSpace(body) == "" {
		return "", "", fmt.Errorf("want params:json after the kind")
	}
	return params, body, nil
}

func parseFields(arg string) (Checker, error) {
	ps, body, err := splitParams(arg)
	if err != nil {
		return nil, fmt.Errorf("verify fields: %w", err)
	}
	p, err := parseScoreParams(ps, false)
	if err != nil {
		return nil, fmt.Errorf("verify fields: %w", err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil || len(want) == 0 {
		return nil, fmt.Errorf("verify fields: the expected answer must be a non-empty JSON object")
	}
	return func(o string) bool {
		got, ok := parseObject(o)
		if !ok {
			return false
		}
		right, _ := scoreCells(got, want, p.tol)
		return float64(right) >= p.min*float64(len(want))-1e-9
	}, nil
}

func parseRows(arg string) (Checker, error) {
	ps, body, err := splitParams(arg)
	if err != nil {
		return nil, fmt.Errorf("verify rows: %w", err)
	}
	p, err := parseScoreParams(ps, true)
	if err != nil {
		return nil, fmt.Errorf("verify rows: %w", err)
	}
	var want []map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil || len(want) == 0 {
		return nil, fmt.Errorf("verify rows: the expected answer must be a non-empty JSON array of objects")
	}
	cols := 0
	for _, r := range want {
		for _, k := range p.keys {
			if _, ok := r[k]; !ok {
				return nil, fmt.Errorf("verify rows: an expected row has no key column %q", k)
			}
		}
		cols = max(cols, len(r))
	}
	total := 0
	for _, r := range want {
		total += len(r)
	}
	return func(o string) bool {
		got, ok := parseRowList(o)
		if !ok {
			return false
		}
		byKey := map[string]map[string]any{}
		for _, r := range got {
			if k, ok := rowKey(r, p.keys); ok {
				if _, dup := byKey[k]; !dup {
					byKey[k] = r
				}
			}
		}
		right, matched := 0, map[string]bool{}
		for _, w := range want {
			k, _ := rowKey(w, p.keys)
			g, ok := byKey[k]
			if !ok {
				continue
			}
			matched[k] = true
			r, _ := scoreCells(g, w, p.tol)
			right += r
		}
		extra := 0
		for _, r := range got {
			if k, ok := rowKey(r, p.keys); !ok || !matched[k] {
				extra++
			}
		}
		denom := float64(total + extra*cols)
		return float64(right) >= p.min*denom-1e-9
	}, nil
}

// scoreCells counts how many fields of want the row got right.
func scoreCells(got, want map[string]any, tol float64) (right, total int) {
	for k, w := range want {
		total++
		if g, ok := got[k]; ok && cellEqual(g, w, tol) {
			right++
		}
	}
	return right, total
}

func cellEqual(got, want any, tol float64) bool {
	w, ok := want.(float64)
	if !ok {
		return looseEqual(got, want)
	}
	var g float64
	switch v := got.(type) {
	case float64:
		g = v
	case string:
		f, ok := parseMoney(v)
		if !ok {
			return false
		}
		g = f
	default:
		return false
	}
	return math.Abs(g-w) <= tol*math.Max(math.Abs(w), 1)+1e-9
}

// parseMoney reads "$1,234.50", "(1,200)" (accounting negative) and "12%" style strings.
func parseMoney(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")")
	s = strings.Trim(s, "()$ ")
	s = strings.ReplaceAll(s, ",", "")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		f = -f
	}
	return f, true
}

func rowKey(r map[string]any, keys []string) (string, bool) {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := r[k]
		if !ok {
			return "", false
		}
		if f, isNum := v.(float64); isNum {
			parts = append(parts, strconv.FormatFloat(f, 'f', -1, 64))
		} else {
			parts = append(parts, norm(fmt.Sprint(v)))
		}
	}
	return strings.Join(parts, "\x1f"), true
}

// parseRowList finds the list of row objects in model output: a bare array, or an object
// holding one array of objects (any key name).
func parseRowList(s string) ([]map[string]any, bool) {
	if i, j := strings.Index(s, "["), strings.LastIndex(s, "]"); i >= 0 && j > i {
		var rows []map[string]any
		if json.Unmarshal([]byte(s[i:j+1]), &rows) == nil {
			return rows, true
		}
	}
	obj, ok := parseObject(s)
	if !ok {
		return nil, false
	}
	for _, v := range obj {
		list, isList := v.([]any)
		if !isList {
			continue
		}
		rows := make([]map[string]any, 0, len(list))
		for _, e := range list {
			m, isMap := e.(map[string]any)
			if !isMap {
				return nil, false
			}
			rows = append(rows, m)
		}
		return rows, true
	}
	return nil, false
}
