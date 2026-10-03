# Measured results

Three runs, each labelled with what was real. The simulator numbers show the mechanism; only run 2 used real models.

## 1. Simulator, 300 tasks

> **SIMULATION.** The upstream models are the bundled mock LLM server. This shows the mechanism, the
> routing behaviour and the gateway overhead. Dollar figures use the configured placeholder prices and are
> not evidence of real-world savings. Re-run with `config/real.json` for real-model numbers.

Workload: 300 tasks map[classify:114 extract:56 faq:57 reason:73] (seed 1); 54 (18%) are exact repeats of an earlier task. Baseline = every task on `mock-large`.
Every run is graded by the same deterministic verifiers.

| Metric | Baseline (always `mock-large`) | Static cascade | Learned routing | Learned + cache |
|---|---:|---:|---:|---:|
| Success rate (verified) | 97.7% | 99.3% | 99.3% | 99.3% |
| Total cost | $0.4700 | $0.1801 | $0.1807 | $0.1586 |
| **Cost per successful task** | $0.00160 | $0.00060 (-62.3%) | $0.00061 (**-62.2%**) | $0.00053 (**-66.8%**) |
| Latency p50 | 662 ms | 104 ms | 104 ms | 104 ms |
| Latency p95 | 5822 ms | 3350 ms | 3349 ms | 3348 ms |
| Cache hit rate | – | – | – | 17.7% |
| Executed tasks that escalated | – | 36.3% | 35.3% | 40.1% |
| Gateway overhead excl. upstream, p50 / p99 | 79 / 178 µs | 88 / 290 µs | 80 / 231 µs | 59 / 183 µs |

**How to read this.** *Static cascade* is the ablation: always start on the cheapest model and escalate on failure, no learning.
*Learned routing* (cache off) vs *static cascade* isolates what learning adds; vs *baseline* it is the total routing effect. Same prompts, quality graded identically.
"Learned + cache" adds the exact-match cache, so it also depends on how repetitive the workload is (here 18% repeats).
Cost per successful task counts every attempt, including failed cheap attempts and any judge calls.

Savings method: `saved = cost the final answer would have had on mock-large (same tokens) − actual cost`; the ledger sums this per request.
Ledger estimate for the cache run: $0.1251 saved of $0.2837 counterfactual. The measured table above is the like-for-like check.

Projection per 100,000 tasks at this mix: baseline $160, static cascade $60, learned $61, learned + cache $53.

### Does it learn? (routing-only run, cache off)

Which model each task class **started** on, first third of the run vs last third:

| Task | Early starts | Late starts |
|---|---|---|
| classify | local×38 | local×38 |
| extract | local×17 small×1 | local×18 |
| faq | local×19 | local×19 |
| reason | local×23 small×1 | local×24 |

Cost and escalations per executed task, by window of tasks:

| Tasks seen | $/executed task | escalation rate | success |
|---:|---:|---:|---:|
| 25 | 0.00004 | 16% | 100% |
| 50 | 0.00011 | 32% | 100% |
| 75 | 0.00034 | 32% | 100% |
| 100 | 0.00026 | 32% | 100% |
| 125 | 0.00119 | 44% | 100% |
| 150 | 0.00044 | 12% | 100% |
| 175 | 0.00074 | 44% | 100% |
| 200 | 0.00050 | 36% | 96% |
| 225 | 0.00051 | 32% | 96% |
| 250 | 0.00029 | 44% | 100% |
| 275 | 0.00072 | 40% | 100% |
| 300 | 0.00209 | 60% | 100% |

### Which workloads should move off the frontier model?

`move`: a cheaper model clears the bar alone (95% lower bound on pass rate ≥ 90%). `first-try`: cheaper in expectation if failures escalate to `mock-large`.

| Task | Volume | Status | Model | Pass rate (95% lower) | Saving per 1k tasks |
|---|---:|---|---|---:|---:|
| extract | 56 | first-try | `mock-local` (private) | 47% (34%) | $0.36 |
| reason | 65 | first-try | `mock-local` (private) | 16% (8%) | $0.30 |
| classify | 114 | first-try | `mock-local` (private) | 89% (82%) | $0.23 |


---

## 2. Real local model, 30 tasks

> **REAL MODELS, SMALL SAMPLE.** Answers, token counts and latencies come from live inference: Qwen3 14B (4-bit) served by
> LM Studio on a MacBook (Apple M4 Max), via `config/local.json`. Run on 2026-10-02, 30 synthetic machine-checkable tasks.
>
> **What was compared.** The *baseline* is the same model in thinking mode (`qwen-think`, what you get by default). The
> *cheap tier* is the same model with thinking switched off (`qwen-fast`, `/no_think`), escalating to the thinking mode when the
> check fails. So this is an **effort-tier** cascade, not a small-model-versus-large-model one.
> **Prices are placeholders** (the same per-token price for both tiers), so the cost difference here is purely tokens
> generated, which is mostly the thinking tokens avoided. It is not evidence of what cloud customers will save.
> **n = 30** is small; treat percentages as indicative.
>
> A first attempt with Llama 3.1 8B as the cheap tier was discarded: the LM Studio build answered every prompt with
> tool-call JSON, so it failed every check. That reflects the model's chat template, not its ability.


Workload: 30 tasks map[classify:9 extract:10 faq:7 reason:4] (seed 1); 0 (0%) are exact repeats of an earlier task. Baseline = every task on `qwen-think`.
Every run is graded by the same deterministic verifiers.

| Metric | Baseline (always `qwen-think`) | Static cascade | Learned routing | Learned + cache |
|---|---:|---:|---:|---:|
| Success rate (verified) | 100.0% | 100.0% | 100.0% | 100.0% |
| Total cost | $0.0022 | $0.0005 | $0.0005 | $0.0006 |
| **Cost per successful task** | $0.00007 | $0.00002 (-77.5%) | $0.00002 (**-76.0%**) | $0.00002 (**-72.2%**) |
| Latency p50 | 8088 ms | 1283 ms | 1253 ms | 1075 ms |
| Latency p95 | 13176 ms | 4835 ms | 3664 ms | 3493 ms |
| Cache hit rate | – | – | – | 0.0% |
| Executed tasks that escalated | – | 6.7% | 6.7% | 6.7% |
| Gateway overhead excl. upstream, p50 / p99 | 74 / 141 µs | 78 / 137 µs | 57 / 189 µs | 81 / 206 µs |

**How to read this.** *Static cascade* is the ablation: always start on the cheapest model and escalate on failure, no learning.
*Learned routing* (cache off) vs *static cascade* isolates what learning adds; vs *baseline* it is the total routing effect. Same prompts, quality graded identically.
"Learned + cache" adds the exact-match cache, so it also depends on how repetitive the workload is (here 0% repeats).
Cost per successful task counts every attempt, including failed cheap attempts and any judge calls.

Savings method: `saved = cost the final answer would have had on qwen-think (same tokens) − actual cost`; the ledger sums this per request.
Ledger estimate for the cache run: $-0.0000 saved of $0.0006 counterfactual. The measured table above is the like-for-like check.

Projection per 100,000 tasks at this mix: baseline $7, static cascade $2, learned $2, learned + cache $2.

### What this shows, and what it does not

- Verified success stayed at 100% on every route while median latency fell about 6.5x and cost per task fell about 76% (routing without cache). Only 2 of 30 tasks needed the escalation.
- The learned router again matched, and did not beat, the plain cheapest-first cascade: it started every class on the cheap tier from the first task.
- The "workloads that could move" table is empty because no task class reached the 20-task minimum in 30 tasks.
- Not shown: cloud-model prices, a genuinely smaller model as the cheap tier, or real customer traffic.

### Does it learn? (routing-only run, cache off)

Which model each task class **started** on, first third of the run vs last third:

| Task | Early starts | Late starts |
|---|---|---|
| classify | qwen-fast×3 | qwen-fast×3 |
| extract | qwen-fast×3 | qwen-fast×3 |

Cost and escalations per executed task, by window of tasks:

| Tasks seen | $/executed task | escalation rate | success |
|---:|---:|---:|---:|
| 25 | 0.00001 | 4% | 100% |
| 30 | 0.00003 | 20% | 100% |

### Which workloads should move off the frontier model?

`move`: a cheaper model clears the bar alone (95% lower bound on pass rate ≥ 90%). `first-try`: cheaper in expectation if failures escalate to `qwen-think`.

| Task | Volume | Status | Model | Pass rate (95% lower) | Saving per 1k tasks |
|---|---:|---|---|---:|---:|


---

## 3. Gateway overhead

**What was measured:** the same requests sent (a) directly to the upstream and
(b) through the gateway. The upstream is the mock LLM with zero simulated latency
(`mock-instant`), so the upstream does no work and the **difference** is what the
gateway adds: JSON parsing, routing, accounting, one extra network hop, and the
streaming proxy.

**Machine:** Apple M4 Max, 16 cores, macOS. The load generator, the upstream and
the gateway all ran on this one machine, over loopback, so they compete for CPU.

**Method:** `lagom bench -c <clients> -n <requests per target> [-stream]`.
Closed loop: each client sends the next request when the previous one returns.
A 2,000-request warm-up precedes each target. Run on 2026-10-02.

### Results

Latency per request, microseconds (µs). 1,000 µs = 1 ms.

#### Light load (not saturated): the fair "extra delay per request"

| Mode | Clients | Direct p50 / p99 | Via gateway p50 / p99 | **Gateway adds p50 / p99** |
|---|---:|---|---|---|
| non-streaming | 8 | 86 / 337 | 181 / 457 | **+95 / +120 µs** |
| non-streaming | 32 | 246 / 998 | 512 / 1,318 | **+266 / +320 µs** |
| streaming (SSE) | 8 | 92 / 345 | 220 / 635 | **+128 / +290 µs** |

#### Saturated (256 clients, all cores busy)

| Mode | Direct p50 / p99 | Via gateway p50 / p99 | Gateway adds p50 / p99 |
|---|---|---|---|
| non-streaming | 1,522 / 5,778 | 3,027 / 9,388 | +1,505 / +3,610 µs |
| streaming (SSE) | 2,607 / 6,743 | 6,053 / 13,137 | +3,446 / +6,394 µs |

Throughput at 256 clients: 75k req/s through the gateway vs 144k req/s direct
(non-streaming); 40k vs 94k (streaming).

### How to read this

- At light load the gateway adds roughly **0.1–0.3 ms**. Real model calls take
  hundreds of milliseconds to seconds, so this is well under 1% of a typical request.
- At 256 clients the machine is CPU-bound by all three processes. A closed-loop
  client's latency is roughly `clients / throughput`, so the numbers above mostly
  reflect that the gateway halves the machine's total throughput (it does the work
  of a second hop), not the cost of one request in isolation.
- The gateway also records its own non-upstream time per request
  (`overhead_us` in the ledger). During the eval runs, under light load, that was
  about 50–90 µs at p50 and under 300 µs at p99. For streaming it excludes
  per-chunk forwarding time, which the benchmark above does capture.

### What this does not show

- Production network paths, TLS, or real provider connection behaviour.
- Behaviour with the load generator and upstream on separate machines. A separate
  host for each would remove the CPU competition and is the first thing to redo.
- Memory and GC behaviour over hours. No soak test was run.
- It has not been profiled yet (`pprof`); the obvious next steps are listed in
  `docs/ARCHITECTURE.md`.

---

## 4. Real local models: assessment of 200 examples (the demo's recorded sample)

`lagom assess -synthetic 200 -seed 33 -learn` on `config/local.json`: the same Qwen3 14B (4-bit, LM Studio, Apple M4 Max) as a fast tier (thinking off) and a thinking tier,
six task types, run through the gateway on 2026-10-03. Placeholder prices for local models, so read cost as relative.

| Workload | Model | Answered | Pass rate (95% lower) | Provider errors |
|---|---|---:|---:|---:|
| classify | qwen-fast | 61 | 100% (94%) | 0 |
| classify | qwen-think | 63 | 100% (94%) | 7 |
| extract | qwen-fast | 32 | 100% (89%) | 0 |
| extract | qwen-think | 32 | 100% (89%) | 7 |
| faq | qwen-fast | 19 | 79% (57%) | 0 |
| faq | qwen-think | 19 | 74% (51%) | 3 |
| grounded | qwen-fast | 26 | 100% (87%) | 0 |
| grounded | qwen-think | 30 | 100% (89%) | 2 |
| reason | qwen-fast | 30 | 43% (27%) | 0 |
| reason | qwen-think | 32 | 100% (89%) | 2 |
| summary | qwen-fast | 32 | 100% (89%) | 0 |
| summary | qwen-think | 39 | 100% (91%) | 1 |

Reading it at a 90% bar: classification is **proven** for the fast tier (lower bound 94%, about 82% cheaper per task); extraction, grounded answers and summaries pass every
example but 26 to 32 examples cannot prove 90% (lower bound 87 to 89%), so the verdict is "need more examples"; FAQ and reasoning stay on the thinking tier
(fast tier: 79% and 43%).

**Method note.** The first pass ran three calls at once; LM Studio returned 22 errors for the thinking tier (HTTP 400 "error in iterating prediction stream", HTTP 500, "model crashed", timeouts).
Counting those as wrong answers made the premium tier look worse than it is, so quality is measured on answered examples only and errors are reported apart (reliability). The table above
was recomputed from the gateway's ledger on that basis. A one-call-at-a-time re-run was started and stopped (about 45 s per example). Small samples, one machine, one model family, synthetic tasks.

### Re-measured on 2026-10-03, with every feature active

Same method (`lagom bench -c 8 -n 30000`, simulator upstream with no delay, one machine), run after the injection guard, quality routing and the observation report were added.

| Mode | Direct p50 | Via gateway p50 | Gateway adds p50 / p99 |
|---|---:|---:|---:|
| non-streaming | 104 µs | 263 µs | **+159 / +204 µs** |
| streaming (SSE) | 129 µs | not recorded | **+206 / +334 µs** |

The first re-run showed +393 µs: the embedding classifier was classifying requests that name their model directly, for no benefit. It now runs only for `model: "auto"` requests without an `X-Lagom-Task` header; those pay one embedding call, which with a real embedding model is tens of milliseconds and is not part of this benchmark. Send the task header to avoid it.

