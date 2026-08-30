# Struct-Momentum Strategy — Design Doc

**Status:** DRAFT for review · **Author:** Claude (Opus 4.8) · **Date:** 2026-08-22
**Driver:** add alt coins (SOL/XRP/SUI/NEAR/HYPE/LINK) that the mean-reversion (MR) engine fits poorly; give trend/momentum-driven symbols a structure-aligned strategy instead. This finally makes "different strategy per symbol" real.

---

## 1. Scope (state explicitly — IN / OUT)

**IN**
- A **second strategy** (`StructMomentum`) that trades *with* structure/trend, coexisting with the MR engine.
- A **per-symbol strategy selector** so each symbol runs the strategy that fits it.
- Reuse of the existing `signal.AnalyzeStructure` primitives (BOS/CHoCH/樞紐區/measured-move) — no new indicator research.
- Output that maps onto the existing `signal.Signal` shape so **all downstream surfaces (dashboard cards, /setups, journal, ntfy, /chart) work unchanged**.
- A **backtest/forward-log validation plan** gated the same way every strategy change is (`feedback_strategy_changes`: A/B 60/90/120d).

**OUT (explicit non-goals for v1)**
- NOT touching the MR engine's logic (closed-bar purity preserved — `feedback_engine_closed_bar_only`).
- NOT adding the alts to `market.All()` / daemon / ntfy until validated (forward-log first, exactly like SNDK/NVDA — `project_us_stock_symbols`).
- NOT new momentum indicators (ADX/slope) in v1 unless the reuse path fails validation — start with what exists.
- NOT auto-orders. Record-only (journal/setups), same as today.

---

## 2. Why (background)

- The engine is **mean-reversion + confluence**, A/B-tuned per symbol on crypto/metals. MR fades extension → it fights trends. Alts are high-beta, trend/momentum/power-law → **MR is the wrong temperament** (`project_us_stock_symbols`; `project_tf_decisions`: XAU broken on every TF, ETH is the most MR-leaning of the four).
- `AnalyzeStructure` is already **trend-aligned** and gives us everything a structure-following entry needs:
  - `Event`: `EvBOSUp/EvBOSDown` = continuation; `EvCHoCHUp/EvCHoCHDown` = reversal.
  - `Trend`: `StructUptrend/StructDowntrend/StructNeutral` (HH-HL / LH-LL context).
  - `Zone *PivotZone{Dir, Hi(0.5), Lo(0.705), Invalidate, Target}}` = the retrace entry band + a **ready-made structural stop (`Invalidate`) and measured-move target (`Target`)**.
  - `BOSLevel` (break = continuation), `Protected` (break = CHoCH/reversal), `InZone` (close inside the band).
- We are **not starting from zero**: `project_pivot_zone_fade_strategy` (trend-aligned retrace-into-樞紐區 entry) is exactly this idea and already accruing `/setups` instances. This doc formalizes that candidate into a selectable strategy.
- The engine already carries a `MomentumScore` sub-score (`Signal.Score == MRScore + MomentumScore`, "trend / breakout / pattern confluence") — reusable as the momentum gate before we reach for ADX/slope.

---

## 3. Architecture

### 3.1 Strategy selector (the mechanism the user imagined, made real)
```
signal.StrategyKind:  StrategyMR | StrategyStructMomentum
func strategyFor(sym market.Symbol) StrategyKind   // per-symbol allowlist, same pattern as isStructureVetoSymbol
```
- `signal.Evaluate` gains a thin dispatch at the top: `switch strategyFor(in.Symbol) { case StrategyStructMomentum: return evaluateStructMomentum(in) ; default: <existing MR path unchanged> }`.
- **The entire existing MR body stays byte-for-byte** — StructMomentum is a *sibling* function, not a modification. Closed-bar purity + backtest parity preserved.
- Default = `StrategyMR` (all current symbols keep exactly today's behavior). Only symbols explicitly listed in `strategyFor` get StructMomentum.

### 3.2 Output contract (reuse `signal.Signal`)
`evaluateStructMomentum` returns a normal `Signal{Side, Score, MRScore, MomentumScore, Price, Timeframe, ...}`:
- `Side` = Long/Short/Flat from structure direction.
- `Score` (0–N, comparable band to MR's total) so the dashboard score chip, `MIN_SCORE` ntfy gate, and tradeable banner all keep working with no template/handler changes.
- `MRScore = 0`, `MomentumScore = Score` (semantically honest: this strategy is all momentum/structure).
- Populate `Entry/Stop/Target` from the zone so /setups + journal + chart plan lines render.

This is the key design choice: **new strategy, same output shape → zero downstream churn.**

---

## 4. Strategy logic (v1 — RECOMMENDED, with open decisions flagged ⚑)

**Core thesis:** trade *with* an established structural trend, enter on the retrace into the 樞紐區, stop at structure invalidation, target the measured move.

### 4.1 Regime gate (must pass or Flat)
- `Trend` must be `StructUptrend` (longs only) or `StructDowntrend` (shorts only). `StructNeutral` → **Flat** (this strategy does nothing in chop — that's MR's job, and it's why it fits alts: alts trend, then we participate; alts chop, we sit out).

### 4.2 Entry (⚑ OPEN DECISION — recommended default = A)
- **A. Continuation-retrace (RECOMMENDED):** after a confirmed `EvBOSUp` (uptrend) or `EvBOSDown` (downtrend), arm an entry; fire when price retraces into `Zone` (`InZone == true`) in the trend direction. This is the highest-quality, lowest-chase entry and reuses `Zone` directly.
- **B. Reversal (CHoCH):** enter on `EvCHoCHUp/EvCHoCHDown` (early trend flip). Higher R:R but lower hit-rate; **recommend OFF in v1**, add later if data supports.
- **C. Breakout-momentum:** enter on the BOS bar itself (no retrace wait). Best capture in violent alt moves but worst no-fill/slippage. **Recommend OFF in v1** (our biggest existing leak is no-fill 31% — chasing breakouts worsens it).
- ⚑ *Decide:* A only (recommended) · A+B · A+C.

### 4.3 Momentum gate (⚑ OPEN DECISION — recommended = reuse MomentumScore)
- **Recommended:** require the engine's existing `MomentumScore ≥ T` on the entry bar as a "is the trend actually pushing" filter (cheap, already computed, no new indicator). Tune `T` in backtest.
- **Alternatives (later):** ADX ≥ 20–25, or EMA-slope sign — only if MomentumScore proves too coarse in validation.
- ⚑ *Decide:* reuse MomentumScore gate (recommended) · add ADX/slope now.

### 4.4 Stop / Target / Exit
- **Stop = `Zone.Invalidate`** (the leg-origin pivot; a clean structural stop — if it breaks, the thesis is void). This is *already computed*; no guesswork.
- **Target = `Zone.Target`** (1:1 measured move) for the base exit; ⚑ optional runner beyond target with a structure trail (trail to new `BOSLevel` on each continuation). *Decide: fixed measured-move only (simpler, recommended v1) · + runner.*
- **Time-stop:** expire the armed setup if no `InZone` fill within N bars (reuse the /setups 20-bar convention) → avoids stale arms.

### 4.5 Timeframe (⚑ OPEN DECISION — recommended = 1h + 2h)
- Recommend **1h primary, 2h secondary** (`project_tf_decisions`: XAG 2h is the structural sweet spot; **15m is poison — exclude**; daemon stays 1h). Validate per symbol; a symbol may only earn one TF.
- ⚑ *Decide:* {1h}, {1h,2h}, or {1h,4h}.

---

## 5. Coexistence, config, forward-log

- StructMomentum symbols are added to `uiSymbols` (chart/journal/scan) + the Scan forward-log section (like SNDK/NVDA), **NOT `market.All()`** → out of daemon/ntfy until validated.
- `strategyFor` is the single source of truth for which symbol runs which strategy; ships defaulting every current symbol to MR (no behavior change on day one).
- A symbol can be **both** a valid MR symbol today AND a StructMomentum candidate later — but only one strategy is active per symbol at a time (selector returns one kind). If we ever want both, that's a v2 "ensemble" discussion — OUT of scope now.

---

## 6. Validation plan (ship-gate — the sequence YOU chose: prove on existing, then add alts)

**Phase 0 — data pre-check (blocker):** confirm BingX `Klines` returns ≥120d history for each alt (SOL/XRP/SUI/NEAR/HYPE/LINK). **HYPE and SUI are newer listings — likely short history → may fail the 120d A/B gate and get deferred.** No code until this is known.

**Phase 1 — build + backtest on EXISTING symbols first.** Implement `evaluateStructMomentum` behind the selector; A/B it on BTC/ETH (and XAU/XAG) across **60/90/120d** vs the current MR engine, on 1h & 2h. Metrics: hit-rate, **real-hit-rate (first-touch+fill)**, netR, **no-fill %**, trade count. This is validating the *strategy* with zero alt risk. Cross-check against the `pivot-zone-fade` `/setups` instances already logged (they're a real-money prior for this exact entry).

**Phase 2 — forward-log the strategy** on whichever existing symbols it scored best, via /setups + the scan cards (record-only), to confirm backtest→live parity (watch the no-fill gap — `project_setups_real_outcome`).

**Phase 3 — add alts as forward-log symbols** (uiSymbols + scan section, NOT daemon), each running StructMomentum. A/B each alt 60/90/120d + forward-log.

**Phase 4 — daemon entry** only for alts that clear the gate (edge in both backtest AND forward-log). Metals/BTC/ETH stay on whatever won *their* A/B.

**Acceptance criteria (draft, ⚑ confirm):** StructMomentum beats MR on the candidate symbol on ≥2 of {60,90,120}d by netR **and** real-hit-rate, with no-fill % not worse than the MR baseline, on ≥1 TF.

---

## 7. Candidate alts (this request)
SOL · XRP · SUI · NEAR · HYPE · LINK.
- Likely-fine history: SOL, XRP, LINK, NEAR.
- Watch: **SUI, HYPE** (newer → short history, may not clear 120d; forward-log-only until enough bars).
- Symbol format / availability on BingX to be confirmed in Phase 0 (same `resolveWebSymbol`/`shortSymbol` extension pattern as SNDK/NVDA).

---

## 8. Risks / mitigations
- **Strategy has no proven edge yet** → Phase 1 backtest on existing symbols before any alt; kill if it doesn't beat MR.
- **No-fill worsens on thin alts** (our #1 leak) → retrace entry (not breakout chase) + track real-hit-rate explicitly; alt spread/liquidity assessed in forward-log before daemon.
- **Short history (HYPE/SUI)** → data pre-check gate; forward-log-only if <120d.
- **Scope creep** (`feedback_strategy_scope` said stay 4-symbol) → this is your explicit call; kept disciplined via the gate + forward-log-first, and the MR framework is untouched.
- **Two-strategy maintenance** → selector isolates them; shared `Signal` output; shared backtest harness.

---

## 9. Cross-cutting checklist (state, don't assume)
- [ ] Backtest parity: StructMomentum evaluated **closed-bar only**, same as MR.
- [ ] `strategyFor` default = MR for every existing symbol (zero day-one behavior change) — locked by a test.
- [ ] Out-of-`market.All()` assertion test for each new alt (like the SNDK/NVDA stays-out test).
- [ ] Signal output invariants (`Side/Score/Entry/Stop/Target`) so dashboard/setups/journal/ntfy/chart render unchanged — snapshot test.
- [ ] Per-symbol TF allowlist (no 15m).
- [ ] chown data files (journal/setups) back to ubuntu:ubuntu after any VPS write.
- [ ] No auto-orders; record-only.

---

## 10. Open decisions for your sign-off (before any code)
1. **Entry set** (§4.2): A only *(rec)* · A+B · A+C.
2. **Momentum gate** (§4.3): reuse `MomentumScore` *(rec)* · add ADX/slope now.
3. **Exit** (§4.4): measured-move only *(rec)* · + structure-trailed runner.
4. **TF** (§4.5): {1h,2h} *(rec)* · {1h} · {1h,4h}.
5. **Acceptance criteria** (§6): confirm or adjust.
6. **First validation symbols** (Phase 1): BTC+ETH *(rec)* · include XAU/XAG.
