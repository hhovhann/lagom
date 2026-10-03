package router

import (
	"strings"
	"testing"
)

func TestFieldsCheck(t *testing.T) {
	spec := `fields:min=0.75;tol=0.01:{"revenue":1000,"expenses":400,"noi":600,"name":"Acme Plaza"}`
	cases := []struct {
		out  string
		want bool
	}{
		{`{"revenue":1000,"expenses":400,"noi":600,"name":"acme plaza"}`, true},
		{`{"revenue":"$1,000","expenses":400,"noi":600}`, true},                      // 3 of 4 = 75%
		{`Here you go: {"revenue":1005,"expenses":400,"noi":590,"name":"x"}`, false}, // revenue ok within 1%, expenses ok, noi 1.7% off, name wrong: 2 of 4
		{`{"revenue":1000,"expenses":999,"noi":1}`, false},
		{`no json`, false},
	}
	for _, c := range cases {
		sp, err := Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.Check(c.out); got != c.want {
			t.Errorf("out %q: got %v want %v", c.out, got, c.want)
		}
	}
}

func TestFieldsDefaultsToAllRight(t *testing.T) {
	sp, err := Parse(`fields:min=1:{"a":1,"b":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Check(`{"a":1}`) || !sp.Check(`{"a":1,"b":2,"c":3}`) {
		t.Error("min=1 must need every expected field and ignore extra ones")
	}
}

const goldRows = `[{"suite":"101","sqft":2400,"rent":58000},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":3000,"rent":72000},{"suite":"104","sqft":1200,"rent":0}]`

func TestRowsCheck(t *testing.T) {
	spec := `rows:key=suite;min=0.9;tol=0.001:` + goldRows
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"exact", goldRows, true},
		{"wrapped in object and prose", `Done. {"rows":` + goldRows + `}`, true},
		{"numbers as money strings, key as number", `[{"suite":101,"sqft":"2,400","rent":"$58,000"},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":3000,"rent":72000},{"suite":"104","sqft":1200,"rent":0}]`, true},
		{"a cell within tolerance still counts", `[{"suite":"101","sqft":2400,"rent":58000},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":3000,"rent":72001},{"suite":"104","sqft":1200,"rent":0}]`, true},
		{"two wrong cells of 12 is 83%", `[{"suite":"101","sqft":2400,"rent":5800},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":300,"rent":72000},{"suite":"104","sqft":1200,"rent":0}]`, false},
		{"missing row", `[{"suite":"101","sqft":2400,"rent":58000},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":3000,"rent":72000}]`, false},
		{"invented rows are penalised", `[{"suite":"101","sqft":2400,"rent":58000},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":3000,"rent":72000},{"suite":"104","sqft":1200,"rent":0},{"suite":"TOTAL","sqft":8400,"rent":173200}]`, false},
		{"not rows", `{"total":173200}`, false},
	}
	for _, c := range cases {
		sp, err := Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.Check(c.out); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRowsToleratesSmallErrorsBelowMin(t *testing.T) {
	sp, err := Parse(`rows:key=suite;min=0.8:` + goldRows)
	if err != nil {
		t.Fatal(err)
	}
	// two wrong cells of 12 = 83% >= 80%
	out := `[{"suite":"101","sqft":2400,"rent":5800},{"suite":"102","sqft":1800,"rent":43200},{"suite":"103","sqft":300,"rent":72000},{"suite":"104","sqft":1200,"rent":0}]`
	if !sp.Check(out) {
		t.Error("83% should pass a min of 0.8")
	}
}

func TestScoreSpecErrors(t *testing.T) {
	for _, bad := range []string{
		"fields:{}",                    // no params separator
		`fields:min=2:{"a":1}`,         // min out of range
		`fields:tol=x:{"a":1}`,         // tol not a number
		`fields:min=0.9:[]`,            // not an object
		`fields:key=a:{"a":1}`,         // key is rows-only
		`rows:min=0.9:[{"a":1}]`,       // no key
		`rows:key=b:[{"a":1}]`,         // key column absent from the gold
		`rows:key=a:{"a":1}`,           // not an array
		`rows:key=a;bogus=1:[{"a":1}]`, // unknown parameter
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestGoldSpecsAllowLargerAnswers(t *testing.T) {
	var b strings.Builder
	b.WriteString(`rows:key=id;min=0.9:[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"unit-` + strings.Repeat("x", 5) + `","v":1}`)
	}
	b.WriteString(`]`)
	if _, err := Parse(b.String()); err != nil {
		t.Errorf("a rows spec of %d bytes should parse: %v", b.Len(), err)
	}
	if _, err := Parse("regex:" + strings.Repeat("a", maxSpecLen)); err == nil {
		t.Error("other kinds keep the small limit")
	}
}
