// Package config loads the gateway configuration (JSON, stdlib only).
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Listen        string                 `json:"listen"`
	Providers     map[string]ProviderCfg `json:"providers"`
	Models        []ModelCfg             `json:"models"`
	Chain         []string               `json:"chain"` // cheapest -> most capable
	BaselineModel string                 `json:"baseline_model"`
	JudgeModel    string                 `json:"judge_model"`
	Router        RouterCfg              `json:"router"`
	Cache         CacheCfg               `json:"cache"`
	LedgerPath    string                 `json:"ledger_path"`
	AuthTokenEnv  string                 `json:"auth_token_env"`  // bearer token for /v1/chat, /v1/outcomes, /v1/models
	AdminTokenEnv string                 `json:"admin_token_env"` // bearer token for stats, reset, reports, metrics (falls back to the API token)
	BudgetUSD     float64                `json:"budget_usd"`      // hard cap on total spend since start; 0 = unlimited
	MaxTokensCap  int                    `json:"max_tokens_cap"`  // ceiling on a caller's max_tokens (default 8192): stops cost amplification
	Quality       QualityCfg             `json:"quality"`
	Classifier    ClassifierCfg          `json:"classifier"`
	Demo          DemoCfg                `json:"demo"`
	Report        ReportCfg              `json:"report"`
	Guard         string                 `json:"guard"`      // prompt-injection screen: "flag" (default), "block" or "off"
	AllowOpen     bool                   `json:"allow_open"` // permit a non-loopback listener with no auth token (not recommended)
}

// QualityCfg is the success bar a model must be proven to clear before it may answer
// a task type that cannot be checked per request.
type QualityCfg struct {
	Bar             float64            `json:"bar"`              // default required pass rate (0-1), e.g. 0.95
	Bars            map[string]float64 `json:"bars"`             // per task type overrides
	MinObservations int                `json:"min_observations"` // evidence needed before a model counts as proven
}

// BarFor returns the required pass rate for a task type.
func (q QualityCfg) BarFor(task string) float64 {
	if b, ok := q.Bars[task]; ok {
		return b
	}
	return q.Bar
}

// ClassifierCfg turns on embedding-based task classification for requests that do not
// name their task. A prompt it is unsure about is held on the most capable model.
type ClassifierCfg struct {
	Provider  string              `json:"provider"`   // an openai-type provider that serves /embeddings
	Model     string              `json:"model"`      // embedding model id; empty = classifier off
	MinScore  float64             `json:"min_score"`  // best similarity needed (default 0.5)
	MinMargin float64             `json:"min_margin"` // lead over the runner-up type (default 0.03)
	Examples  map[string][]string `json:"examples"`   // reference prompts per task type
}

// ReportCfg shapes the customer report at /report.
type ReportCfg struct {
	Currency      string            `json:"currency"`        // label shown with money, e.g. "EUR" (default "USD")
	USDToCurrency float64           `json:"usd_to_currency"` // fixed illustrative rate from the pricing currency (default 1)
	FeeRate       float64           `json:"fee_rate"`        // share of the monthly saving shown as the fee (default 0.20)
	Workloads     map[string]string `json:"workloads"`       // task type -> display name
}

// DemoCfg configures the live quality-gate demo: which providers a visitor can run a
// workload on and what it may spend. Base URLs come only from here, never from a request.
type DemoCfg struct {
	SessionCapUSD float64                    `json:"session_cap_usd"` // hard cap per live run (default 0.50)
	Providers     map[string]DemoProviderCfg `json:"providers"`
}

type DemoProviderCfg struct {
	Label          string     `json:"label"`
	Type           string     `json:"type"` // "openai" or "anthropic"
	BaseURL        string     `json:"base_url"`
	MaxTokensField string     `json:"max_tokens_field"`
	KeyRequired    bool       `json:"key_required"` // true: the visitor brings a key; false: the server's api_key_env is used (local models)
	APIKeyEnv      string     `json:"api_key_env"`
	Tiers          []DemoTier `json:"tiers"` // cheapest first; the last is the premium baseline
}

type DemoTier struct {
	Label        string  `json:"label"` // small | mid | premium
	ID           string  `json:"id"`
	Upstream     string  `json:"upstream"`
	InPerMTok    float64 `json:"in_per_mtok"`
	OutPerMTok   float64 `json:"out_per_mtok"`
	Effort       string  `json:"effort"`
	MaxTokens    int     `json:"max_tokens"`
	PromptSuffix string  `json:"prompt_suffix"` // appended to each prompt, e.g. " /no_think"
}

type ProviderCfg struct {
	Type            string `json:"type"` // "openai" (any OpenAI-compatible server) or "anthropic"
	BaseURL         string `json:"base_url"`
	APIKeyEnv       string `json:"api_key_env"`
	ForwardMeta     bool   `json:"forward_meta"`      // simulator only
	MaxTokensField  string `json:"max_tokens_field"`  // "max_tokens" (default) or "max_completion_tokens"
	DefaultMaxToken int    `json:"default_max_token"` // used when the request sets none
}

type ModelCfg struct {
	ID             string  `json:"id"`
	Provider       string  `json:"provider"`
	Upstream       string  `json:"upstream"` // model name sent to the provider
	InPerMTok      float64 `json:"in_per_mtok"`
	OutPerMTok     float64 `json:"out_per_mtok"`
	PriceVerified  bool    `json:"price_verified"`
	Private        bool    `json:"private"` // self-hosted / open model
	PriorP         float64 `json:"prior_p"` // prior belief of success, before any data
	PriorLatencyMS float64 `json:"prior_latency_ms"`
	Effort         string  `json:"effort"`
	MaxTokens      int     `json:"max_tokens"`
	PromptSuffix   string  `json:"prompt_suffix"` // appended to the last user message, e.g. " /no_think" to switch Qwen3 thinking off
}

type RouterCfg struct {
	MaxAttempts      int     `json:"max_attempts"`
	FailPenaltyUSD   float64 `json:"fail_penalty_usd"`    // cost assigned to a task that ends unverified
	LatencyUSDPerSec float64 `json:"latency_usd_per_sec"` // price of waiting
	AttemptTimeoutMS int     `json:"attempt_timeout_ms"`
	PriorWeight      float64 `json:"prior_weight"`
}

type CacheCfg struct {
	Enabled    bool `json:"enabled"`
	MaxEntries int  `json:"max_entries"`
	TTLSec     int  `json:"ttl_sec"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.defaults()
	return &c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080" // loopback by default: the admin endpoints are sensitive
	}
	if c.Guard == "" {
		c.Guard = "flag"
	}
	if c.Classifier.MinScore == 0 {
		c.Classifier.MinScore = 0.5
	}
	if c.Classifier.MinMargin == 0 {
		c.Classifier.MinMargin = 0.03
	}
	if c.Report.Currency == "" {
		c.Report.Currency = "USD"
	}
	if c.Report.USDToCurrency == 0 {
		c.Report.USDToCurrency = 1
	}
	if c.Report.FeeRate == 0 {
		c.Report.FeeRate = 0.20
	}
	if c.Report.Workloads == nil {
		c.Report.Workloads = map[string]string{"classify": "Support ticket triage", "extract": "Invoice extraction", "faq": "Customer FAQ",
			"reason": "Reasoning", "grounded": "Grounded answers (RAG)", "summary": "Customer summaries", "general": "General", "unclassified": "Unclassified (held on premium)"}
	}
	if c.Demo.SessionCapUSD == 0 {
		c.Demo.SessionCapUSD = 0.5
	}
	if c.Quality.Bar == 0 {
		c.Quality.Bar = 0.95
	}
	if c.Quality.MinObservations == 0 {
		c.Quality.MinObservations = 20
	}
	if c.MaxTokensCap == 0 {
		c.MaxTokensCap = 8192
	}
	if c.Router.MaxAttempts == 0 {
		c.Router.MaxAttempts = 3
	}
	if c.Router.FailPenaltyUSD == 0 {
		c.Router.FailPenaltyUSD = 0.05
	}
	if c.Router.AttemptTimeoutMS == 0 {
		c.Router.AttemptTimeoutMS = 60000
	}
	if c.Router.PriorWeight == 0 {
		c.Router.PriorWeight = 2
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 10000
	}
	if c.Cache.TTLSec == 0 {
		c.Cache.TTLSec = 3600
	}
	for i := range c.Models {
		if c.Models[i].Upstream == "" {
			c.Models[i].Upstream = c.Models[i].ID
		}
		if c.Models[i].PriorP == 0 {
			c.Models[i].PriorP = 0.7
		}
		if c.Models[i].PriorLatencyMS == 0 {
			c.Models[i].PriorLatencyMS = 800
		}
	}
	for k, p := range c.Providers {
		if p.MaxTokensField == "" {
			p.MaxTokensField = "max_tokens"
		}
		if p.DefaultMaxToken == 0 {
			p.DefaultMaxToken = 1024
		}
		c.Providers[k] = p
	}
}

func (c *Config) validate() error {
	switch c.Guard {
	case "off", "flag", "block":
	default:
		return fmt.Errorf("guard %q: use \"off\", \"flag\" or \"block\"", c.Guard)
	}
	if c.Classifier.Model != "" {
		if p, ok := c.Providers[c.Classifier.Provider]; !ok || p.Type != "openai" {
			return fmt.Errorf("classifier.provider %q must be an openai-type provider that serves /embeddings", c.Classifier.Provider)
		}
	}
	for name, p := range c.Demo.Providers {
		if (p.Type != "openai" && p.Type != "anthropic") || p.BaseURL == "" || len(p.Tiers) < 2 {
			return fmt.Errorf("demo.providers[%q]: needs type openai|anthropic, base_url and at least two tiers", name)
		}
	}
	for task, b := range c.Quality.Bars {
		if b <= 0 || b > 1 {
			return fmt.Errorf("quality.bars[%q] = %v: use a fraction between 0 and 1", task, b)
		}
	}
	if c.Quality.Bar <= 0 || c.Quality.Bar > 1 {
		return fmt.Errorf("quality.bar = %v: use a fraction between 0 and 1", c.Quality.Bar)
	}
	ids := map[string]bool{}
	for _, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("model %q references unknown provider %q", m.ID, m.Provider)
		}
		ids[m.ID] = true
	}
	if len(c.Chain) == 0 {
		return fmt.Errorf("chain is empty")
	}
	for _, id := range c.Chain {
		if !ids[id] {
			return fmt.Errorf("chain references unknown model %q", id)
		}
	}
	if c.BaselineModel != "" && !ids[c.BaselineModel] {
		return fmt.Errorf("baseline_model %q is not a configured model", c.BaselineModel)
	}
	if c.JudgeModel != "" && !ids[c.JudgeModel] {
		return fmt.Errorf("judge_model %q is not a configured model", c.JudgeModel)
	}
	return nil
}
