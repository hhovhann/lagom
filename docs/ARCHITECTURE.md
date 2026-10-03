# Architecture

```
 client (any OpenAI SDK, base_url → gateway, model = "auto")
        │  POST /v1/chat/completions   (+ optional X-Lagom-* headers)
        ▼
┌──────────────────────────── gateway (Go, stdlib only) ────────────────────────────┐
│ 1 parse, guard, classify     injection screen (flag|block), then X-Lagom-Task / heuristics│
│ 2 cache lookup               exact match; re-verified on hit; key includes verifier│
│ 3 choose start model         Thompson sampling over the cascade (per task class)   │
│ 4 cascade                    attempt → verify → escalate to next model on failure  │
│        providers: OpenAI-compatible (OpenAI, Gemini, Ollama, vLLM, mock) │ Anthropic│
│ 5 respond                    JSON or SSE; lagom{cost, baseline, saved, path} attached│
│ 6 emit event                 non-blocking channel send; never waits on bookkeeping  │
└───────────────────────────────────────────┬────────────────────────────────────────┘
                                            ▼ (single consumer goroutine)
                         ledger: aggregates + JSONL file + feeds the learner
                                            │
                      /v1/stats  /v1/report/candidates  /metrics  /  /dashboard
```

## Request lifecycle

1. **Parse and guard.** Body is capped at 8 MB. Non-system messages are screened for prompt injection (`internal/gateway/guard.go`): in `block` mode a match returns 400 before any model call; in `flag` mode it is reported in `X-Lagom-Guard`, `lagom.guard` and `/metrics`. Run and task labels are sanitised and bounded. `model: "auto"` means optimise; any configured model id means passthrough (used for the baseline, still verified and recorded).
2. **Cache.** Only for `auto`. The key hashes messages, `max_tokens` and the verifier spec. On a hit the stored answer is **re-checked** against the verifier before it is served, and the request is recorded at $0 with the avoided cost as saving.
3. **Start model.** The learner returns an index into the cost-ordered chain.
4. **Cascade.** Run the model, verify, and on failure move up the chain (max attempts is configurable). A provider **error** fails over to the next model; a circuit breaker skips a model that failed 3 times in a row for 10 s.
5. **Respond.** Non-stream: JSON. Stream without a verifier: bytes are proxied as they arrive (no buffering). Stream with a verifier: the answer must be buffered to be checked, then replayed as SSE (a deliberate trade-off, documented).
6. **Record.** One event per task: every attempt (model, tokens, cost, latency, verdict), final verdict, cost, counterfactual cost, saving, gateway overhead.

## The routing policy

For a task class, with models ordered cheap → capable:

```
E_i = c_i + (1 − p_i) · E_{i+1}          E_last = c_last + (1 − p_last) · penalty
```

`c_i` is the mean cost of one attempt on model *i* (dollars + a configurable price on waiting), `p_i` the probability an attempt passes the verifier. The learner keeps a Beta posterior for each `p_i` per task class and **samples** it (Thompson sampling); the task starts at `argmin E_i`. Every attempt of a cascade is an observation for the model that ran, so escalations teach it about the larger models too.

**What the evidence says (see README, "Findings"):** with a near-free first tier, a plain cheapest-first cascade is already close to optimal, and the learned policy matched it rather than beating it in my simulation. Learning should matter where a wasted first attempt is expensive or slow (tiers close in price, slow local models, high latency cost). That is untested on real traffic.

## Savings accounting

```
baseline_usd = prompt tokens × baseline input price + estimated baseline output tokens × baseline output price
actual_usd   = Σ attempts (including failed ones) + judge-model calls
saved_usd    = baseline_usd − actual_usd
```

Cache hits: actual = 0, baseline = what that answer would have cost. The baseline's output tokens are the **mean it has been measured to write** for this task type when the learner has at least 10 measured attempts of it (from `assess -learn` or baseline traffic); otherwise the answering model's own token count is assumed. The calibration matters: without it, a thinking baseline looks no more expensive than the lean model that answered, and the saving is understated (a real 40-request run showed 2.7% saved uncalibrated and 59% calibrated, against 76% in the like-for-like eval). It is still an **estimate**. The honest check is the like-for-like eval (`lagom eval`) that runs the same tasks on the baseline for real. In production the equivalent is **shadow sampling**: run the baseline on a small random slice of traffic to calibrate the estimate.

## Hot path vs cold path

| Hot path (per request) | Cold path (async) |
|---|---|
| JSON decode, classify, cache lookup, learner sample (microseconds), provider call, response | aggregation, percentile maths, JSONL write, learner update, reports |

A full event channel drops events (counted, exposed in `/metrics`) rather than blocking a customer's request.

## Known limits (be upfront about these)

- Outcome signal in the demo is a deterministic verifier that knows the answer. Real customers need their own signal; `POST /v1/outcomes` accepts delayed outcomes for tasks without an inline check.
- Single node. The cache is in memory. The learner is rebuilt from the ledger JSONL at startup; the log has no rotation, and `/v1/admin/reset` moves the old one aside.
- Task classification is keyword heuristics; production should take a task id from the caller or use a learned classifier.
- Exact-match cache only. No semantic cache, no prompt or context optimisation yet.
- Auth is two optional bearer tokens (API and admin, constant-time compare); the listener defaults to loopback, and the gateway refuses to start on a non-loopback address without a token. No per-tenant budgets or rate limits and no TLS termination (put it behind a proxy).
- The prompt-injection guard is a keyword/regex screen (about 17 µs per 2 KB). It catches commodity attacks, not paraphrases, other languages, encodings or image-borne text, and there is no PII scanning. A model-based classifier can replace it behind the same `Guard.Scan` interface.
- Spend is counted per upstream attempt on the request path and escalation stops at the budget, but requests already in flight can overshoot by one request each, and a timed-out attempt is billed upstream without usage being reported (counted as $0). The provider's billing page is the source of truth.
- `X-Lagom-Verify`, `X-Lagom-Baseline-Model` and friends are trusted from whoever holds the API token. Only the customer's backend should hold it, never end users.
- The cache key has no tenant; a multi-customer deployment needs one.
- Streaming overhead per chunk is measured by the benchmark but not isolated in the ledger.
- Not profiled with `pprof`; not soak-tested.
- The Anthropic and OpenAI adapters are tested against fake servers that mimic the wire formats, not against the live APIs.

## What changes at scale

| Scale | Change |
|---|---|
| ~1M requests/day | Run N stateless gateways behind a load balancer. Ledger events go to a queue (Kafka/Redis streams) instead of an in-process channel. Shared cache (Redis). Learner state in a store, updated by a single worker. |
| ~1B requests/day | ClickHouse (or similar) for the ledger. Learner trained from the event stream on a schedule and pushed to gateways as a small policy snapshot (gateways never wait on it). Regional deployments, connection pooling per provider, hedged requests, per-tenant budgets. |
| Open/private models | vLLM behind the same OpenAI-compatible adapter. The `/v1/report/candidates` output is the input to the "move this workload to a private model" decision. |

## Requirements: what exists and what does not

The target: make every AI task cheaper without making it worse, by learning from production traffic
(request → execution → cost → latency → outcome → learn → optimise), measured as cost per successful task.

| Step | Built | Where |
|---|---|---|
| request | OpenAI-compatible `/v1/chat/completions`, streaming; `model: "auto"` optimises | `gateway/chat.go` |
| execution | cascade: cheapest start, escalate on a failed check, fail over on provider error, circuit breaker | `chat.go`, `providers/` |
| cost | every attempt priced from real token counts, including failed tries and judge calls; compared with the baseline model | `chat.go`, `ledger/` |
| latency | per attempt and per request; gateway overhead recorded separately | `ledger/` |
| quality / outcome | inline verifiers (`equals`, `oneof`, `number`, `regex`, `json`, `jsoneq`, `fields`, `rows`, LLM `judge`) or a delayed result via `POST /v1/outcomes` | `router/verify.go`, `server.go` |
| learn | Beta posterior per (task class, model), Thompson sampling picks the start model; rebuilt from the ledger at startup | `router/learner.go`, `ledger.Replay` |
| optimise | start model per class, exact cache, quality bar for unchecked traffic, `/v1/report/candidates` | `chat.go`, `server.go` |

| Requirement | Status | Note |
|---|---|---|
| Low-latency routing and streaming | Done, measured | ~0.16 ms at light load (0.21 ms streaming), re-measured with every feature on; the embedding classifier adds one embedding call to unlabelled `auto` requests; see `RESULTS.md` |
| High-concurrency traffic | Partial | tested to 256 clients on one machine; no soak test or profiling |
| Multi-provider inference | Partial | OpenAI-compatible + Anthropic adapters outbound; OpenAI and Anthropic (text, non-streaming) wire formats inbound. Run against fake servers and local models, **not the live cloud APIs**; Gemini only through its OpenAI-compatible endpoint |
| Cost-per-task measurement | Done | savings are an estimate; `lagom eval` is the like-for-like check; shadow sampling not built |
| Model and prompt experimentation | Partial | `assess` runs labelled examples on every model; eval ablations; per-model `prompt_suffix`; no prompt A/B framework |
| Quality-aware routing | Done | per-workload bar; only models whose 95% lower bound clears it answer unchecked traffic; unclassified prompts go to premium. Needs a check, a delayed outcome, or an assessment to prove a model |
| Task classification | Done, basic | embedding similarity to reference examples with score and margin thresholds; about 90% placed, 8% held, 2.5% wrong on the synthetic workload with a real embedding model |
| Evaluate on labelled examples | Done | `lagom assess`, JSONL in, workload analysis table out |
| Customer report | Done | `/report`: monthly baseline and saving, efficiency score, rerouted share, daily and cumulative charts, by workload and model, fee panel. Built from the ledger, so it survives restarts. Score formulas are stated assumptions, not a standard. No accounts, billing or PDF generation (browser print) |
| Task packs | Partial | classify, extract, faq, reason, grounded answers (retrieval) and summaries with hard-gate checks (`rules:`); no code-diagnosis pack, no agent/tool-use tasks |
| Live demo with a visitor's key | Built, untested live | five-step page; key used per run only, spend-capped; tested against fake providers, not a real cloud API |
| Caching and context optimisation | Partial | exact cache only; no semantic cache or prompt compression |
| Continuous learning | Partial | learns live and is rebuilt from the ledger on restart; picks only the start model; matched but did not beat a plain cascade; scheduled re-assessment is `assess` run from cron |
| Move workloads to private models | Basic | 95% lower-bound quality bar; needs 20+ tasks per class |
| Enterprise volumes | Not done | single node; design above |

Next, in order: a database-backed ledger (the JSONL log is single-node and grows without rotation); shared learning via an event stream and policy snapshots;
more arms than the start model (prompt variants, semantic cache); a trustworthy per-customer outcome signal with shadow
sampling; per-tenant budgets, cache keys and rate limits.

## Working on the code

```bash
make test       # vet + all tests with the race detector
make demo       # simulator + gateway + five-step demo at /, report at /report, race at /race
./lagom assess -synthetic 100 -bar 0.9     # cheapest proven model per workload (gateway running)
./lagom traffic -n 40                      # sample traffic: fills /report
```

`gateway/chat.go` is the file to read first: auth → budget → parse → injection guard (`guard.go`) → task class → cache
(re-verified) → start model → cascade (call, verify, escalate or return) → respond → one event to the ledger, off the request path.

| Change | Where |
|---|---|
| New model or provider | JSON in `config/` only, if the API is OpenAI-compatible |
| New success check | a case in `router/verify.go` `Parse` + a row in `verify_test.go` |
| New task pack for the demo | a generator in `sim/workload.go`, its success probabilities in `sim/mock.go`, reference examples in the config's `classifier.examples` |
| Change the report | the aggregation is `ledger/observation.go` (with a test); the page is `gateway/report.html` |
| New guard rule | `guardRules` in `gateway/guard.go` with a keyword hint, plus one attack and one benign sentence in `guard_test.go`; `go test ./internal/gateway -bench Guard` keeps it fast |
| New endpoint | register in `Handler()`; wrap with `s.api` (customer) or `s.admin` (operator) |

Rules: standard library only; nothing slow on the request path; every behaviour change ships with a test; run `make test` and
`staticcheck ./...` before a change; never return prompts, keys or upstream error bodies to callers; every number in the docs comes
from a reproducible run with the date and model named.

