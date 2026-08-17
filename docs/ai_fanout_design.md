# AI Analyze — Fan-out (multi-agent) Design

A System-Design writeup for the multi-agent orchestration of the AI advisor. Pattern:
**scatter-gather (map-reduce)** — cheap specialist workers in parallel → one synthesis.
Follow-on to `ai_analyze_upgrade_spec.md` (Stage 4). Grounded in the existing `ai` package
(provider abstraction, `SystemPromptQuantAdvisor`, model picker) and Go concurrency.

---

## 1. Why fan-out (and when NOT to)

**Single-shot today**: one LLM call holds the whole system prompt + all context and produces
the whole answer. Fine, but:
- one model juggles structure + regime + execution + backtest reasoning at once;
- token/latency scale with the monolith;
- can't cheaply run many symbols at once.

**Fan-out**: split the analysis into focused sub-tasks, run them **in parallel** on a cheap
tier, then a **synthesis** agent (stronger tier) combines their structured outputs.

**When it's worth it**
- Batch: "analyze all 4 symbols" / "review all open /setups" → workers parallelize across
  items; huge wall-clock win.
- Depth: each lens (structure / regime / execution) gets full attention + its own context
  slice → better than a rushed monolith.
- Cost: on Gemini free-tier, N cheap workers ≈ $0, so specialization is nearly free.

**When to skip it (be honest)**
- A SINGLE symbol analyze is probably fine as one strong-tier call (Stage 1+2). Fan-out adds
  a second round-trip (workers → synthesis = 2 hops of latency) and more rate-limit pressure.
- Don't fan out for its own sake. The clear win is **multi-item batch**; single-item is
  marginal.

## 2. Agent decomposition (this domain)

```
                          ┌──────────────────┐
              ┌──────────▶│ structure worker │─┐   (swings, 樞紐區, BOS/CHoCH, trend)
              │           └──────────────────┘ │
 inputs  ─────┤           ┌──────────────────┐ │        ┌───────────────┐
 (ground      ├──────────▶│  regime  worker  │─┼───────▶│  synthesis     │──▶ final
  truth)      │           └──────────────────┘ │        │  agent (strong)│    bilingual
              │           ┌──────────────────┐ │        └───────────────┘    verdict
              └──────────▶│ execution worker │─┘   (entry-placement, no-fill, R, realOutcome)
                          └──────────────────┘
                          run in PARALLEL           run AFTER (gather)
```

- **structure worker** — swing sequence, pivot zone, trend/label-lag, BOS/CHoCH. Output: a
  structured "structure verdict" (trend, is-clean, zone band, invalidation, fade direction).
- **regime worker** — POC drift/stacked, multi-TF bias, funding, higher-TF alignment. Output:
  regime (trend-with / fade / range) + directional bias + conviction.
- **execution worker** — entry-placement rule, no-fill risk, R math, realOutcome prior.
  Output: entry/stop/target proposal + fill-probability + R + caveats.
- **synthesis agent** — consumes the three structured outputs (NOT re-deriving), resolves
  conflicts (structure vs regime vs execution), writes the final GO/WAIT/SKIP + sizing in the
  mandatory bilingual format.

All agents share `SystemPromptQuantAdvisor` + a short role addendum → one voice.

## 3. Structured hand-off (critical)

Workers must emit **structured** output (compact JSON), not prose, so synthesis consumes them
deterministically. Enforce via a per-worker output schema in the role addendum, e.g.:

```json
// structure worker
{"trend":"LH-LL down","clean":true,"zone":{"dir":"down","hi":66.09,"lo":65.92,"inv":66.34,"tgt":64.64},"fade_dir":"short","note":"..."}
```

Synthesis prompt: "Here are three specialist reports (JSON). Do NOT recompute numbers; reconcile
and decide." Reuse the Stage-1 ground-truth rule.

## 4. Go orchestration (the System-Design meat)

```go
type workerOut struct{ Role string; JSON string; Err error }

func fanOut(ctx context.Context, gc *ai.Client, base ai.Opts, workers []workerSpec) []workerOut {
    // Bound concurrency to respect Gemini free-tier RPM (e.g. 5–10 req/min).
    sem := make(chan struct{}, maxConcurrent)          // semaphore
    outs := make([]workerOut, len(workers))
    var wg sync.WaitGroup
    for i, w := range workers {
        wg.Add(1)
        go func(i int, w workerSpec) {
            defer wg.Done()
            sem <- struct{}{}; defer func(){ <-sem }()
            wctx, cancel := context.WithTimeout(ctx, workerTimeout)
            defer cancel()
            resp, err := gc.Send(wctx, ai.Opts{                // cheap tier
                Model: cheapModel, System: base.System + w.RoleAddendum,
                Messages: w.Messages,
            })
            outs[i] = workerOut{Role: w.Role, JSON: resp.Text(), Err: err}
        }(i, w)
    }
    wg.Wait()
    return outs
}
```

Then synthesis:
```go
outs := fanOut(ctx, gc, base, workers)
report := assembleWorkerReports(outs)          // drop failed workers, note them
final, _ := gc.Send(ctx, ai.Opts{Model: strongModel, System: base.System + synthAddendum,
    Messages: []ai.Message{{Role:"user", Content: report}}})
```

Key SD decisions baked in above:
- **Bounded concurrency** (semaphore) — Gemini free-tier has RPM limits; unbounded fan-out
  gets 429s. Cap = your quota / safety margin. `errgroup.SetLimit` is the idiomatic alt.
- **Per-worker timeout + context cancellation** — one slow worker can't stall the whole call.
- **Partial-failure tolerance** — a worker that errors drops to a noted gap; synthesis proceeds
  with what it has (never fail the whole analysis on one worker). "Silence is not success" —
  synthesis must be told which lens is missing.
- **Deterministic assembly** — collect into a fixed-index slice, not a race-y append.

## 5. Tiering & cost

- Workers: `gemini-2.5-flash-lite` (cheapest, fast) or `flash`.
- Synthesis: `gemini-2.5-pro` or `gemini-3-flash-preview` (stronger reasoning for reconciliation).
- On free-tier this is ≈ $0; the real constraint is **RPM**, not $. So batch across symbols
  benefits most when you can pipeline within the rate cap.
- Reuse the existing model picker (`/api/ai/model`, `GEMINI_MODEL`) — add a second knob for the
  worker tier vs synthesis tier.

## 6. Failure modes & mitigations (SD checklist)

| risk | mitigation |
|---|---|
| Gemini 429 (RPM) | bounded concurrency + backoff/retry on the client; degrade to single-shot |
| one worker hangs | per-worker `context.WithTimeout` |
| worker returns junk JSON | validate/parse; on fail, pass its raw text with a "unstructured" flag |
| synthesis invents numbers | Stage-1 ground-truth rule + "reconcile, don't recompute" |
| all workers fail | fall back to the single-shot Stage-1+2 path (never no-op) |
| latency (2 hops) | acceptable for on-demand analyze; for batch, workers overlap so amortized |
| non-determinism | cache worker outputs by (symbol, tf, candle-close) like the bias/tick caches |

## 7. Recommendation / staging

1. **Ship Stage 1+2 first** (done) and see if a single strong-tier call is already good enough
   for single-symbol analyze. Likely yes.
2. **Build fan-out for BATCH** first — "analyze all 4 / all open setups" — where the parallel
   win is real. That's the compelling use case, and a clean SD exercise (scatter-gather with a
   rate cap).
3. Only then consider per-single-symbol fan-out if depth demands it.

Bottom line: fan-out is the right pattern for **breadth (many items at once)**; for a single
setup, the prompt+context upgrade already gets you most of the way.
