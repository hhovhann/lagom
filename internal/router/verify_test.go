package router

import (
	"strings"
	"testing"
)

func TestChecks(t *testing.T) {
	cases := []struct {
		spec, out string
		want      bool
	}{
		{"equals:billing", " Billing. ", true},
		{"equals:billing", "bug", false},
		{"equals:Paris", "`paris`", true},
		{"contains:refund", "We will REFUND you", true},
		{"oneof:a|b|c", "B", true},
		{"oneof:a|b|c", "d", false},
		{"regex:^INV-\\d{4}$", "INV-1234", true},
		{"regex:^INV-\\d{4}$", "INV-12", false},
		{"number:42", "The answer is 42.", true},
		{"number:1284.5", "Total: 1,284.50", true},
		{"number:42", "41", false},
		{"number:42", "no digits", false},
		{"json:vendor,total", "```json\n{\"vendor\":\"A\",\"total\":3}\n```", true},
		{"json:vendor,total", `{"vendor":"A"}`, false},
		{"json:vendor", "not json", false},
		{`jsoneq:{"vendor":"Acme","total":1284.5}`, `Sure! {"vendor":"acme","total":"1,284.50","x":1}`, true},
		{`jsoneq:{"vendor":"Acme","total":1284.5}`, `{"vendor":"Acme","total":1284.6}`, false},
	}
	for _, c := range cases {
		sp, err := Parse(c.spec)
		if err != nil || sp == nil {
			t.Fatalf("Parse(%q): %v", c.spec, err)
		}
		if got := sp.Check(c.out); got != c.want {
			t.Errorf("%s on %q = %v, want %v", c.spec, c.out, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"nokind", "bogus:x", "regex:(", "number:abc", "jsoneq:{"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
	if sp, err := Parse(""); sp != nil || err != nil {
		t.Errorf("empty spec should be nil, nil")
	}
	sp, _ := Parse("judge:be concise")
	if !sp.IsJudge() || sp.Rubric != "be concise" {
		t.Errorf("judge spec not parsed: %+v", sp)
	}
}

func TestParseRejectsOversizedSpec(t *testing.T) {
	if _, err := Parse("regex:" + strings.Repeat("a", maxSpecLen)); err == nil {
		t.Error("an oversized verifier spec must be rejected")
	}
	if _, err := Parse("regex:^a+$"); err != nil {
		t.Errorf("a normal spec must still parse: %v", err)
	}
}

func TestRulesGates(t *testing.T) {
	sp, err := Parse("rules:maxwords=8;must=Atlas;ban=guarantee;ban=refund")
	if err != nil {
		t.Fatal(err)
	}
	for out, want := range map[string]bool{
		"Atlas customer cannot log in after reset.":                             true,
		"The customer cannot log in after the password reset email.":            false, // required term missing
		"Atlas customer cannot log in, we guarantee a fix":                      false, // banned claim
		"Atlas customer asks for a REFUND":                                      false, // ban is case-insensitive
		"Atlas customer cannot log in after resetting the password again today": false, // over the word limit
	} {
		if got := sp.Check(out); got != want {
			t.Errorf("rules(%q) = %v, want %v", out, got, want)
		}
	}
	for _, bad := range []string{"rules:", "rules:maxwords=zero", "rules:colour=red", "rules:must"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
