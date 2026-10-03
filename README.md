# Lagom

An LLM gateway that lowers **cost per successful task**. It sits between your app and your AI
models, tries the cheapest model likely to work, **checks the answer**, escalates only if the check
fails, caches verified answers, and records what every request cost against what it would have cost
on your usual model.

One Go binary, standard library only. OpenAI-compatible API (streaming included).

## The problem, and what this solves

Most teams send every AI task to the biggest, most expensive model "to be safe". Many of those tasks are easy
enough for a cheaper model, but nobody can prove which ones, and nobody knows what a task *really* costs once
retries and failures are counted. Bills grow with volume, and quality is assumed instead of measured.

Lagom is a thin layer between the app and the models that answers one question for every task:
**what is the cheapest way to get a correct result?** It does that by

1. trying the cheapest model likely to work (or no model at all, if the answer is cached),
2. **checking the answer** with the customer's own success test,
3. escalating to a stronger model only if the check fails (failed tries are still counted),
4. recording the true cost per successful task against what the customer's usual model would have cost,
5. learning from those results which kinds of task the cheap model handles reliably, and starting them there.

Its metric is **cost per successful task**, not tokens.

| Design goal | Where it stands |
|---|---|
| Very fast, lightweight execution layer for high-volume traffic | One Go binary, about 0.2 ms added per request at light load (8 clients, re-measured with every feature on), streaming without buffering |
| "Change the base URL" integration | OpenAI format (`/v1/chat/completions`) and Anthropic format (`/v1/messages`, text and non-streaming) |
| Quality first, cost second: the cheapest model *proven* to clear a required quality bar | `lagom assess` per workload; only proven models answer unchecked traffic (see [Measure quality first](#prove-quality-before-routing)) |
| Cheapest way to get the outcome: another model, a smaller model, caching, no call at all | Model cascade and verified exact cache are built; prompt and context optimisation are not |
| Know what kind of prompt it received, and refuse to guess | Embedding classifier; an unsure prompt is held on the premium model |
| Learn from production traffic (request → cost → latency → outcome → learn → optimise) | Built; learned state is rebuilt from the ledger on restart (see [How it learns](#how-it-learns)) |
| Measurable savings | Per-request ledger: cost, baseline cost, saving, models tried |
| A board-ready report on what the traffic costs and what routing saves | `/report`: monthly baseline and saving, an "Efficiency score", rerouted share, daily and cumulative charts, by workload and by model, fee and what you keep; print to PDF |
| Show it live | A five-step demo at `/`: provider and key, quality bar, volume, day-to-day routing, what happens next |
| Move repetitive work to private or open models | `/v1/report/candidates` lists which task types could move |

Full requirement-by-requirement status: `docs/ARCHITECTURE.md`.

## The tasks used in the demo and tests

Synthetic, small, and machine-checkable, so a wrong answer is detected automatically:

| Task | Example | Success check |
|---|---|---|
| classify | label a support ticket: billing, bug, feature_request, account_access | answer equals the right label |
| extract | pull vendor, invoice number, total, currency from an invoice as JSON | JSON contains all four correct values |
| faq | "What is the capital of Egypt?" | answer equals the right city |
| reason | a short arithmetic word problem | the number in the answer is correct |

About 18% of tasks in the 300-task test are exact repeats, to exercise the cache. A real customer replaces these with their
own tasks and sends their own check in the `X-Lagom-Verify` header (or reports the outcome later through `/v1/outcomes`).

## How success is measured

Lagom does not judge answers by understanding them. A **check** decides, and the check is the customer's definition of "good enough":

| Check (`X-Lagom-Verify`) | Passes when |
|---|---|
| `equals:Cairo` | the answer is exactly that (case and trailing punctuation ignored) |
| `oneof:billing\|bug\|...` | the answer is one of the allowed labels |
| `number:139` | the last number in the answer is 139 |
| `regex:` / `contains:` | the pattern or text matches |
| `json:k1,k2` / `jsoneq:{...}` | the answer is valid JSON with those keys, or those values |
| `fields:min=0.9;tol=0.005:{...}` | extraction with partial credit: passes when at least 90% of the expected fields are right (numbers within 0.5%) |
| `rows:key=suite;min=0.9;tol=0.005:[{...}]` | table extraction: rows matched by the key column(s), every cell scored; missing and invented rows both cost points |
| `judge:<rubric>` | a separate grader model answers PASS against your rubric (fails closed) |

**A request fails** when the check fails, the provider errors, it times out, or the model refuses. **A request that cannot be measured**
(no check, or a prompt the classifier cannot place) is never routed to a cheaper model on a guess: it goes to the premium model.
If the result is only known later (a test passed, a ticket was resolved), report it through `POST /v1/outcomes`.

For open-ended tasks the practical check is a mix: hard rules (word limit, required fields, banned claims) plus the independent grader.
Keep the grader a different model from the ones being tested, so no model marks its own homework.

## Prove quality before routing

Before any traffic moves, find out which model is good enough for each workload:

```bash
# examples.jsonl, one per line: {"task":"classify","prompt":"...","verify":"equals:billing"}
./lagom assess -data examples.jsonl -bar 0.95 -bars extract=0.98 -learn -out report.md
```

Every example runs on every model of the chain. A model **qualifies** only when the 95% lower bound on its measured pass rate reaches the
workload's bar, so a few lucky answers cannot qualify it. The report is one row per workload: current model, required quality, the cheaper
model's accuracy, the saving, and a recommendation: *use smaller model*, *keep premium model*, or *need more examples*.
With `-learn` the results also teach the gateway, so routing starts from proven models. The bar is configured in `quality` (default 95%).

**Documents (PDF and images).** A line may add `"files": ["docs/t12-001.pdf"]` (paths relative to the dataset file; PDF, PNG, JPEG, GIF, WebP; 20 MB each) and the
file is sent with the prompt. Score the answer with `fields:` or `rows:` (partial credit on extracted numbers). The gateway also accepts
OpenAI-style `file` / `image_url` content parts holding base64 `data:` URIs on `/v1/chat/completions`, and turns them into Anthropic `document`/`image` blocks or OpenAI
`file`/`image_url` parts. **Tested against fake servers only: the first live run with a real key is the real test** (Gemini's OpenAI-compatible endpoint in particular may not accept PDFs;
if it does not, `lagom check` and the first `assess` error will say so). Links and file ids are rejected, not ignored; the injection guard and the classifier see the prompt text, not the document.

## The report

`/report` turns the ledger into the page a CFO or CTO reads. Only optimised requests (`model: "auto"`) count; assessment runs do not. It rebuilds from the
ledger on every start, so it survives restarts. Figures are scaled from the observed days to a 30-day month and are an observation, not an invoice.

| Section | How it is computed |
|---|---|
| Baseline / saving / % saved | what the answers would have cost on the baseline model, minus what they actually cost, per observed day, scaled to 30 days |
| Rerouted | requests answered by a model other than the baseline |
| Avoidable premium tokens | tokens of rerouted answers: work that ran on a richer model than it needed |
| Efficiency score (0-100) | Cost 35% (saving against a 40% target) + Quality 25% (pass rate of rerouted answers that had a check) + Fit 25% (share rerouted) + Efficiency 15% (share of tokens on lean models) |
| Fee / you keep | `report.fee_rate` (default 20%) of the monthly saving. Set by the operator; the pilot bills nobody |

The score formulas are simple stated mappings, not a standard: tune them with the people who read the report. Currency and fee are in the `report` config section.

## How it learns

Every attempt is an observation: *this model, on this kind of task, passed or failed, cost this much, took this long*.
Per task type and model the gateway keeps a pass-rate estimate (Beta posterior) and picks the starting model by sampling it
(Thompson sampling), so it keeps exploring but favours what has worked. With a check supplied, a wrong pick costs a failed cheap attempt, not a wrong
answer. Without a check the gateway cannot tell, so give it one.

**Do you need a database or a cache?**

| | Today (pilot) | For production |
|---|---|---|
| Answer cache | in memory, exact match, re-verified on every hit | shared cache (Redis) once there is more than one gateway |
| Ledger (every request and cost) | in memory aggregates + append-only JSONL file | a database (Postgres to start, a columnar store at large volume) |
| Learned state | in memory, **rebuilt from the ledger on every start** | the same, or stored in the database |

For a single-node pilot no database is needed: the append-only ledger is the source of truth and is replayed at startup, so a restart
does not forget what was learned. To learn from millions of executions, move the ledger to a database and train off the request path.
The plan is in `docs/ARCHITECTURE.md`.

> **Status: pilot v1.** Tested on a simulator and on real local models (see [Results](#results)).
> The Anthropic and OpenAI-compatible adapters have been run against fake servers and local servers, **not yet
> against the live cloud APIs**. Savings shown by the simulator demonstrate the mechanism, not what you will save.

## Quick start

Needs Go 1.22+.

```bash
make demo      # simulator + gateway + live page at http://localhost:8080  (no keys, no network)
make test      # vet + all tests with the race detector
```

| Run mode | Command | Needs |
|---|---|---|
| **Demo** (simulated models) | `make demo` | nothing |
| **Assess your workloads** | `./lagom assess -data examples.jsonl` | a running gateway (any mode above) |
| **Fill the report with sample traffic** | `./lagom traffic -n 60` then open `/report` | a running gateway |
| **Local models** (real inference) | `make local` | LM Studio or Ollama serving on `:1234`, edit `config/local.json` |
| **Cloud models** (real money) | `make real` | `ANTHROPIC_API_KEY` in `.env`, read [Real API keys](#real-api-keys) first |

## Use it

Point any OpenAI client at the gateway and set `model` to `auto`:

```bash
curl localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -H 'X-Lagom-Verify: oneof:positive|negative' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Sentiment, one word: great service!"}]}'
```

The reply is a normal chat completion plus a `lagom` block: `cost_usd`, `baseline_usd`, `saved_usd`,
`path` (e.g. `["small✗","medium✓"]`), `cache_hit`, `guard`.

| Header | Meaning |
|---|---|
| `X-Lagom-Verify` | The success check: `equals:` · `contains:` · `oneof:a\|b` · `regex:` · `number:` · `json:k1,k2` · `jsoneq:` · `fields:` · `rows:` · `judge:<rubric>`. Without it the gateway cannot know whether a cheap answer was good enough. |
| `X-Lagom-Task` | Task class (classify, extract, …). Guessed from the prompt if absent. |
| `X-Lagom-Baseline-Model` | Model to compare savings against (default: `baseline_model` in the config). |
| `X-Lagom-No-Cache` | Skip the cache. |
| `X-Lagom-Learn` | With a named model: also teach the learner from this request (used by `assess -learn`). |

`model: "<id>"` instead of `auto` sends the request straight to that model (still verified and recorded).

Anthropic clients work the same way: `POST /v1/messages` accepts Anthropic's request format (text, non-streaming) and returns Anthropic's
response format. A model name the gateway does not know is treated as `auto`, so an app can keep its existing model name.

Pages and endpoints: `/` five-step quality-gate demo · `/report` the observation report (`/v1/report` as JSON, admin token) · `/race` the side-by-side race · `/dashboard` metrics · `/v1/report/candidates` workloads that could move
to a cheaper or private model · `/v1/stats` · `/metrics` (Prometheus text) · `/v1/outcomes` (report a result later).

## Configuration

Three ready configs in `config/`; each is one small JSON file.

| Key | What it does |
|---|---|
| `providers` | Where models live: `type` `openai` (OpenAI, Gemini, Ollama, LM Studio, vLLM) or `anthropic`; `api_key_env` names the env var holding the key |
| `models` | `id`, `provider`, `upstream` (name sent to the provider), prices per million tokens, `max_tokens`, optional `prompt_suffix` |
| `chain` | Model ids from cheapest to most capable |
| `baseline_model` | What "before Lagom" means, for the savings figure |
| `budget_usd` | Hard cap on spend since start. Past it: HTTP 429. `0` = unlimited |
| `guard` | Prompt-injection screen: `flag` (default) · `block` · `off` |
| `max_tokens_cap` | Ceiling on a caller's `max_tokens` (default 8192) |
| `auth_token_env`, `admin_token_env` | Env vars holding the API and admin bearer tokens |
| `quality` | `bar` (default 0.95), `bars` per task type, `min_observations` (default 20) |
| `classifier` | embedding `provider` and `model`, `min_score`, `min_margin`, `examples` per task type; empty = keyword guess |
| `demo` | providers the live demo may run (`key_required`, tiers with prices) and `session_cap_usd` (default 0.50) |
| `cache` | `enabled`, `max_entries`, `ttl_sec` |

## Real API keys

1. `cp .env.example .env` and fill the keys you use (`.env` is git-ignored and loaded automatically).
   A Claude Pro / ChatGPT Plus subscription is **not** an API key; create one in the provider's console and
   add prepaid credit.
2. Use a **dedicated key with a provider-side spend limit**. The gateway's `budget_usd` (default `$3` in
   `config/real.json`) is a second line of defence, computed from tokens × configured prices; the provider's
   billing page is the source of truth.
3. Stage it, and read the cost from `/v1/stats` each time. First `./lagom check` (one tiny request per model: it tells you
   if a key, a model id or a price is wrong, for a fraction of a cent):
   ```bash
   make real                                            # terminal 1
   ./lagom check                                        # terminal 2: key, model id and price OK?
   ./lagom eval -n 30 -c 2 -out eval-report.md   # terminal 2: smoke test
   ./lagom eval -n 100 -c 4 -out eval-report.md  # only if the first looked right
   ```
   Estimate (300 tasks, Haiku → Sonnet → Opus): about $0.3 if models answer directly, about $6 if
   Sonnet/Opus use heavy reasoning (thinking tokens are billed as output). Measure with `-n 30` first.
4. Prices in `config/real.json` match Anthropic's published rates on 2026-09-25 (Haiku 4.5 $1/$5, Sonnet 5.5
   $2/$10, Opus 5.5 $4/$20 per million tokens). **Re-check before quoting savings.**

The live demo in `config/real.json` offers Anthropic, OpenAI and Gemini, three tiers each. The Anthropic ids and prices were checked against Anthropic's published rates;
the **OpenAI and Gemini tier ids and prices are copied from a published third-party comparison and are not verified: check them against the vendors before a live run.**

**Add OpenAI or Gemini** (OpenAI-compatible, no code needed): add a provider and a model, then list the model
in `chain` in price order:

```jsonc
"providers": {
  "openai": { "type": "openai", "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY", "max_tokens_field": "max_completion_tokens" },
  "gemini": { "type": "openai", "base_url": "https://generativelanguage.googleapis.com/v1beta/openai", "api_key_env": "GEMINI_API_KEY" }
},
"models": [ { "id": "gemini-flash", "provider": "gemini", "upstream": "<model id from Google's docs>",
              "in_per_mtok": 0.0, "out_per_mtok": 0.0, "price_verified": true } ]   // fill real prices
```

**Add a private/local tier in front of the cloud models**: copy the `lmstudio` provider and the `qwen-fast`
model from `config/local.json` into `config/real.json`, and put it first in `chain`.

## Security

- **Auth**: set `auth_token_env` (and a separate `admin_token_env`). The gateway **refuses to start** on a
  non-loopback address without a token (override: `"allow_open": true`). Tokens are compared in constant time.
- **Prompt-injection guard** (`guard`): screens every non-system message before any model call for instruction
  override, persona/jailbreak, system-prompt exfiltration, and forged role markers (zero-width characters and
  case tricks normalised). `flag` serves the request and reports matches in the `X-Lagom-Guard` header, `lagom.guard`
  and `/metrics`; `block` returns HTTP 400 and spends nothing. About 17 µs per 2 KB message. **It is a heuristic
  layer, not a guarantee**: it misses paraphrases, other languages, encodings and image-borne attacks. The judge
  verifier is isolated (escaped tags, fixed grader prompt, fail-closed), which protects grading regardless of the guard.
- **Spend**: `budget_usd` is checked on the request path and counted per upstream attempt, and escalation stops
  once it is reached. Concurrent requests already in flight can overshoot by up to one request each.
- **Abuse limits**: caller `max_tokens` is capped; `X-Lagom-Run` / `X-Lagom-Task` labels are sanitised and bounded;
  upstream error bodies are logged, not returned to the caller.
- **Live demo and visitor keys** (`demo` config): a key is used only for that run's calls, held in memory, never stored, logged or echoed;
  provider errors are scrubbed of the key before they are shown. Provider base URLs come only from the config (never from a request),
  each run is hard-capped (default $0.50), at most two runs go at once, and the endpoints need the API token when auth is on.
  This path has been tested against fake providers, **not yet against a real cloud API**.
- `X-Lagom-Verify` and friends are trusted from whoever holds the API token: give that token only to your backend,
  never to end users.

## Results

**Simulation (300 tasks, simulated models priced like Haiku/Sonnet/Opus)**: cost per successful task −62% with
routing, −67% with the cache, success 97.7% → 99.3%. Full table: `docs/RESULTS.md`.
Gateway overhead (M4 Max, one machine): about 0.16 ms per request at light load (0.21 ms streaming), measured on 2026-10-03 with the guard, quality routing and report active: `docs/RESULTS.md`. Requests with `model: "auto"` and no `X-Lagom-Task` header also pay one embedding call when the classifier is configured.

**Real local models (30 tasks, Qwen3 14B via LM Studio, placeholder prices)**: the same model with thinking off, escalating to
thinking mode on a failed check, against always-thinking: cost per task −76%, median latency 8.1 s → 1.3 s, success 100% on both.
This is an effort-tier cascade on one model, so the saving is avoided thinking tokens; it says little about cloud prices.
Method, caveats and a discarded first attempt: `docs/RESULTS.md`.
A 200-example assessment on the same models (six task types) proved the fast tier for classification (about 82% cheaper per task) and left the rest on the thinking tier or "need more
examples"; 40 checked requests through the gateway then showed 100% passing their checks, 75% rerouted and about 59% estimated saving. Method and caveats: `docs/RESULTS.md`, section 4.

What did not work as hoped is reported there too: in both runs the learned router matched, but did not beat,
a plain cheapest-first cascade.

## Layout

```
main.go        one binary:  lagom demo | serve | mock | check | assess | traffic | eval | bench
config/        mock.json  local.json  real.json
docs/          ARCHITECTURE.md (design, requirement status, how to work on it)  RESULTS.md (measured)
internal/
  gateway/     HTTP API, cascade, streaming, injection guard, demo + dashboard pages
  router/      learner (Thompson sampling), verifiers, exact cache
  providers/   OpenAI-compatible and Anthropic adapters + shared request types
  ledger/      async event pipeline, replay, cost and success aggregates, the observation report
  config/      config + .env loading
  assess/      labelled-example assessment: cheapest proven model per workload
  sim/         LLM simulator, synthetic workload, eval and bench tools
```

## Not done yet

Live cloud-API runs of the adapters and of the bring-your-own-key demo · Anthropic streaming and Gemini's native format ·
per-tenant budgets and cache keys · shadow sampling to calibrate the savings estimate · semantic cache and prompt optimisation ·
PDF export beyond the browser's print, accounts and billing, database-backed ledger · load/soak testing and profiling · multi-node deployment. Requirement-by-requirement status and the scaling plan: `docs/ARCHITECTURE.md`.
