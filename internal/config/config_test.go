package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Every config shipped in config/ must load and validate.
func TestShippedConfigsLoad(t *testing.T) {
	files, err := filepath.Glob("../../config/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no configs found: %v", err)
	}
	for _, f := range files {
		c, err := Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if len(c.Chain) < 2 {
			t.Errorf("%s: chain needs at least two models to cascade", f)
		}
		// chain must be ordered cheapest -> most capable (by output price)
		byID := map[string]ModelCfg{}
		for _, m := range c.Models {
			byID[m.ID] = m
		}
		for i := 1; i < len(c.Chain); i++ {
			if byID[c.Chain[i]].OutPerMTok < byID[c.Chain[i-1]].OutPerMTok {
				t.Errorf("%s: chain not ordered by price at %s -> %s", f, c.Chain[i-1], c.Chain[i])
			}
		}
	}
}

func TestValidationRejectsBadConfig(t *testing.T) {
	c := &Config{Providers: map[string]ProviderCfg{"p": {Type: "openai"}}, Models: []ModelCfg{{ID: "a", Provider: "nope"}}}
	if c.validate() == nil {
		t.Fatal("unknown provider should be rejected")
	}
	c = &Config{Providers: map[string]ProviderCfg{"p": {}}, Models: []ModelCfg{{ID: "a", Provider: "p"}}, Chain: []string{"missing"}}
	if c.validate() == nil {
		t.Fatal("chain with unknown model should be rejected")
	}
}

func TestGuardModeValidated(t *testing.T) {
	c := &Config{Guard: "strict", Providers: map[string]ProviderCfg{}, Chain: []string{"x"}}
	if err := c.validate(); err == nil {
		t.Error("unknown guard mode must fail validation")
	}
	c = &Config{}
	c.defaults()
	if c.Guard != "flag" || c.MaxTokensCap != 8192 {
		t.Errorf("defaults: guard=%q cap=%d", c.Guard, c.MaxTokensCap)
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/.env"
	os.WriteFile(p, []byte("# comment\nLAGOM_A=one\nexport LAGOM_B=\"two words\"\nLAGOM_C='three'\n\nbad line\nLAGOM_KEEP=from-file\n"), 0o600)
	t.Setenv("LAGOM_KEEP", "from-env")
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"LAGOM_A": "one", "LAGOM_B": "two words", "LAGOM_C": "three", "LAGOM_KEEP": "from-env"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := LoadDotEnv(dir + "/missing.env"); err != nil {
		t.Errorf("a missing .env must not be an error: %v", err)
	}
}

func TestQualityBarDefaultsAndOverrides(t *testing.T) {
	c := &Config{}
	c.defaults()
	if c.Quality.Bar != 0.95 || c.Quality.MinObservations != 20 {
		t.Errorf("defaults: %+v", c.Quality)
	}
	c.Quality.Bars = map[string]float64{"extract": 0.98}
	if c.Quality.BarFor("extract") != 0.98 || c.Quality.BarFor("classify") != 0.95 {
		t.Error("per-task bar must override the default")
	}
	c.Quality.Bars["bad"] = 1.5
	if err := c.validate(); err == nil {
		t.Error("a bar above 1 must fail validation")
	}
}
