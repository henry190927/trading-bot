# AI Analyze Upgrade — Spec

Goal: make the on-chart **AI analyze** read the market the way the daily back-and-forth
does — pivot-zone-fade, structure-over-bias, regime, zone entry-placement, no-fill /
execution discipline — so the user doesn't have to ask a human each time. **Not a
rewrite** — the infra is already mature; this is a prompt + context + orchestration
upgrade.

Related memory: `feedback_web_ai_advisor_constraints`, `project_pivot_zone_fade_strategy`,
`project_setups_real_outcome`, `project_journal_lessons_tips`, `project_nfe_structure_veto`.

---

## 1. What already exists (grounded in code)

- **Routes**: `POST /ai/analyze/:id` (trade), `/ai/analyze/symbol/:short/:tf` (symbol),
  `/ai/analyze/validate`; `GET|POST /api/ai/model` (model picker). — `cmd/web/main.go`
- **Provider abstraction**: Anthropic (Claude) **and** Gemini, selectable at runtime.
  Same system prompt, only the wire protocol differs. — `ai/provider.go`, `ai/gemini.go`,
  `ai/client.go`
- **API keys**: `ANTHROPIC_API_KEY` / `GEMINI_API_KEY` from env; endpoints error if unset
  (or `*_DRY_RUN=true`). Per the advisor-constraints memory these should stay **per-user**,
  not hardcoded. — `cmd/web/main.go`
- **Canonical persona**: `ai.SystemPromptQuantAdvisor` — ONE system prompt for every agent
  (primary + future sub-agents), enforcing: senior-Quant persona, **mandatory bilingual
  `[EN]…[/EN]` + `[ZH-TW]…[/ZH-TW]`** output, strategy framing, verbatim backtest facts,
  output flavors. — `ai/prompts.go`
- **Context builders**: higher-TF summaries, structure notes, HVN/POC lists, per-trade &
  per-symbol snapshots with a fixed output skeleton (status → setup-quality-vs-backtest →
  path → regime → verdict). — `ai/context.go`, `ai/symbol_context.go`, `cmd/web/ai_helpers.go`

**Verdict:** the plumbing (providers, keys, bilingual, structured context, output skeleton)
is done. The **methodology it reasons with is stale** and the **inputs are incomplete**.

## 2. Gaps to close

1. **Stale system prompt.** `SystemPromptQuantAdvisor` still frames the world as
   "mean-reversion, 4 symbols, don't add pairs" and knows nothing of the frameworks built
   since: pivot-zone-fade, structure>bias, regime, zone entry-placement, no-fill/execution
   lens, defensive-close, the tip cards. It also hard-conflicts with the US-stock exploration.
2. **Incomplete context.** The AI isn't fed the exact inputs the human read uses: the
   **multi-TF bias table**, the **pivot zones** (0.5/0.705 band, dir, invalidation, target),
   the **structure swing sequence + trend label** (and the caveat that the 1h trend LABEL
   lags — read swings), the **POC regime** (stacked/drift), and the **realOutcome /
   execution-quality** context.
3. **No real orchestration.** Prompt anticipates sub-agents but calls are single-shot. Per
   the advisor-constraints memory, multi-agent should fan out to save primary-token spend.
4. **Grounding not enforced.** The biggest LLM risk here is **inventing price levels**. The
   design must feed exact engine-computed levels and forbid the model from generating numbers.

## 3. Design

### 3a. System-prompt upgrade (the core change)
Update `ai.SystemPromptQuantAdvisor` to encode the current methodology (distilled from the
strategy/feedback memory files — keep it the single canonical prompt):

- **Read order = structure first, bias second.** Classify trend from the SWING SEQUENCE
  (HH-HL / LH-LL), not the possibly-lagging 1h trend label; use 2h when 1h label lags.
  When structure is a clean trend, the trend IS the edge — mixed bias/oscillators are noise
  to discount (this is the "engine MR = counter-indicator in a trend" rule).
- **Regime decides direction** (POC-drift: stacked-rising→trend-with-it, flat→mean-revert).
  Same pivot zone is a long in a bull regime, a short in a bear regime — get regime wrong
  and you're inverted.
- **Pivot-zone-fade**: in a confirmed trend, a retrace into the 樞紐區 (0.5–0.705) is a
  trend-direction fade entry. **Entry-placement rule**: LIMIT inside the zone (upper-middle
  ~0.6 default; 0.5 for surer fill; 0.705 for max R), never market-chase above it. Stop
  below the zone invalidation.
- **Execution/no-fill discipline**: flag no-fill risk when price won't retrace to a limit;
  don't chase; report the R cost of chasing. Reference the realOutcome lens (read-right vs
  trade-profitable gap = stop/execution quality).
- **Defensive-close discipline** (+EV even if price later returns), **macro/earnings
  blackout** awareness, and the tip-card principles (don't-fade-stacked-regime, macro
  flatten, stop-then-reverse = fix execution not stop width).
- **Scope**: replace the hard "4-symbol only, don't add pairs" with the current reality —
  BTC/ETH/XAU/XAG live; US-stock synthetics (NCSK*) under evaluation (SNDK+veto / NVDA+zone
  survived backtest). Keep the "don't over-expand" discipline but stop contradicting the
  active work.
- **Refresh the backtest-facts block** if newer A/B numbers exist; keep the "cite verbatim,
  never misalign columns" rule.

### 3b. Context enrichment
Extend the per-symbol context builder to include, as GROUND TRUTH the model must not alter:
- multi-TF bias row (5m/15m/1h/2h/4h dir + score) — reuse `zone`/bias compute;
- pivot zone(s): `{dir, lo(0.705), hi(0.5), invalidate, target}` per relevant TF;
- structure: swing sequence (last ~6), trend label, latest event (BOS/CHoCH), bos/protected;
- POC regime: drift %, stacked, trend;
- key levels list (support/resistance ladder) pre-computed;
- optional: the symbol's /setups realOutcome tally (execution-quality prior).

Add an explicit instruction: **"All prices/levels below are computed ground truth. Reason
over them; NEVER invent or recompute a number."**

### 3c. Multi-agent orchestration (token-efficient)
Per advisor-constraints memory, fan out cheap sub-agents and synthesize once:
- **structure agent** (swings/zones/trend), **regime agent** (POC/bias/funding),
  **execution agent** (entry-placement + no-fill + realOutcome) → **synthesis agent**
  produces the final bilingual verdict.
- All share `SystemPromptQuantAdvisor`; sub-agents can run on the cheaper tier (Gemini
  Flash), synthesis on the stronger tier. Keeps primary-token spend down.

### 3d. Provider / keys
- Keep the Claude+Gemini abstraction and per-user key. **Provider matters less than prompt
  + grounding.** Recommend: default Gemini (already integrated, free-tier capable) for
  sub-agents; allow Claude for synthesis if the nuanced methodology drifts on Gemini.
- Gemini reliability: fine on a capable tier (Pro / recent Flash) **with** hard grounding.
  The failure mode is hallucinated numbers — mitigated by 3b's ground-truth rule.

## 4. Implementation stages (ship-gate friendly)
1. **Prompt-only** (biggest bang, cheapest): rewrite `SystemPromptQuantAdvisor` with the
   frameworks + fix scope. Test on existing endpoints. No new infra.
2. **Context enrichment**: add bias/zones/structure/regime/levels to the context builders.
3. **Grounding hardening**: the "never invent numbers" rule + verify against a few live
   symbols that it doesn't drift.
4. **Multi-agent**: only if single-shot token cost is a problem; otherwise defer.
5. Validate qualitatively: does the on-chart analyze match a human read on 3–4 live setups?

## 5. Open questions
- Refresh the backtest-facts block with the latest A/B before shipping the new prompt?
- Include US-stock (SNDK/NVDA) framing now, or keep the prompt crypto/metals-only until
  they're daemon-promoted?
- Sub-agent fan-out worth the complexity yet, or is a single strong-tier call fine at
  current usage?
