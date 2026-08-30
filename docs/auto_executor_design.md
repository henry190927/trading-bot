# Auto-Executor — Design Doc

**Status:** DRAFT for review · **Author:** Claude (Opus 4.8) · **Date:** 2026-08-26
**Driver:** user misses good entries while asleep / in meetings. Wants to pre-set per-symbol parameters ONCE, then have a program auto-place bracket orders when a good position is detected — no per-order consent. This is autonomous real-capital execution (the deferred "option B"); it must go through the same ship-gate as any strategy change, and **paper-first is non-negotiable** given today's entries (ETH long stopped, LINK stopped, short underwater) showed the entry/stop mechanics aren't dialed yet.

---

## 1. Scope (state explicitly — IN / OUT)

**IN**
- A **deterministic daemon** (goroutine in cmd/monitor, or a new cmd/autotrade) that reads a **config file** of per-symbol rules and auto-places bracket orders when triggers fire.
- Reuse of the EXISTING order infra: `bingx.PlaceLimit` (entry+stop+tp bracket) / `PlaceStopMarket` / `PlaceReduceOnlyLimit` / `CancelOrder` / `MarketCloseReduceOnly`, all DryRun-capable.
- Reuse of the EXISTING read-aids as triggers: `signal.AnalyzeStructure` (zone/BOS/CHoCH), `computeAlignment` (TF agreement), `computeRange` (box edges), `computeHVNTargets`, `strategyFor`/StructMomentum.
- **Hard safety layer**: global master switch, per-trade + total margin caps, max-concurrent, daily-loss kill-switch, blackout gate, one-position-per-symbol, post-stop cooldown, ntfy on every action.
- **Paper mode** (mock placement + paper journal) as the mandatory first phase.
- Every auto-action recorded to the journal + pushed to ntfy.

**OUT (explicit non-goals for v1)**
- NOT the assistant (me) discretionarily watching + placing — a PROGRAM executes deterministic rules (testable, capped, consistent). Me ad-hoc trading the account is fuzzy and out of scope.
- NOT auto-executing UNVALIDATED setups with real capital — paper-first, then small live only after edge is shown.
- NOT position management beyond the initial bracket in v1 (no trailing/scaling/re-entry logic — the bracket's stop+tp handle the exit). Trailing is v2.
- NOT touching the MR engine / StructMomentum logic. The executor CONSUMES their signals; it doesn't change them.

---

## 2. Why paper-first is non-negotiable (the evidence)
Today (2026-08-25→26), following the CURRENT setups produced: ETH long @2483 stopped (-0.92R) — a trend zone-entry in a RANGE regime, entry at the box top, stop inside the noise; LINK short defensive-closed (-0.59R); the reversed ETH short is underwater. The range-mode + stop-outside-box fixes were only just shipped and are UNPROVEN live. **Auto-executing an un-dialed rule = automating losses.** So the executor must PAPER-trade the exact triggers first, accrue a sample, and show the entry/stop quality is good BEFORE any real capital. This mirrors [[project_struct_momentum_strategy]]'s design→A/B→forward-log→daemon discipline and [[feedback_strategy_changes]] (gather live data before shipping).

---

## 3. Architecture

```
cmd/monitor (or cmd/autotrade) — new goroutine runAutoExecutor(ctx, client)
  every closed-bar tick (+ optional 25s live poll for zone-touch triggers):
    cfg = loadAutoConfig()                 // /opt/trading/autotrade.json, hot-reloaded
    if !cfg.Enabled { continue }           // GLOBAL master switch (env AUTOTRADE_ENABLED)
    if killSwitchTripped() { halt+alert }  // daily-loss guard
    for each rule in cfg.Rules where rule.Enabled:
        if guardsFail(rule) { continue }   // blackout / existing position / cooldown / caps
        sig = evaluateTrigger(rule)        // uses AnalyzeStructure/alignment/range/SM
        if sig.Fires:
            place bracket (PlaceLimit entry+stop+tp) [DryRun if paper]
            record to journal (+ paper flag)
            ntfy "🤖 auto-placed <sym> <side> @<entry>"
```
- **Deterministic + config-driven.** Same input → same action. Fully testable.
- **Hot-reloaded config** (like zones.json) so rules can be edited live.
- **Placement reuses the proven `/journal/open` open_position path** (already DryRun-verified 2026-08-26: SOL long qty 26.04 @96, mock orderId, no real order) — factored into a shared helper both the web form and the executor call.

---

## 4. Config schema (autotrade.json — the params you fill once)

```json
{
  "enabled": false,                    // GLOBAL master (also gated by AUTOTRADE_ENABLED env)
  "paper": true,                       // paper/DryRun mode — mock orders + paper journal
  "maxConcurrentTotal": 2,             // across all symbols
  "maxMarginTotalUSDT": 100,           // total capital at risk cap
  "dailyLossHaltR": -3.0,              // kill-switch: halt all if today's realized R <= this
  "rules": [
    {
      "enabled": true,
      "symbol": "SOL",
      "strategy": "range-edge",        // range-edge | zone-retrace | struct-momentum
      "tf": "1h",
      "side": "long",                  // long | short | auto (follow signal)
      "trigger": {                     // when to fire (strategy-specific)
        "requireAligned": true,        // TF-alignment must agree with side
        "zone": "bottom-third",        // range-edge: bottom-third=buy / top-third=short
        "notInBlackout": true
      },
      "entry": {"type": "limit", "ref": "hvn-support"},  // limit at HVN/zone, or market-on-trigger
      "stopType": "box-outside",       // box-outside | structure-invalidation | pct
      "stopPct": 0.5,                  // buffer beyond the box/structure
      "tp": ["box-top", "measured-move"],
      "marginUSDT": 20,
      "leverage": 50,
      "maxConcurrent": 1,              // per-symbol
      "cooldownBarsAfterStop": 6       // don't re-enter the same failed setup immediately
    }
  ]
}
```
You fill one rule per symbol; the executor watches + fires. Editing the file (hot-reload) tunes it live.

---

## 5. Trigger logic (per strategy — reuse what's built)
- **range-edge** (neutral regime): fire when `computeRange` says isRange + price in bottom-third (long) / top-third (short) + `requireAligned` satisfied. Stop = box-outside (the fix from today). Target = opposite edge / measured. This is the setup that would have AVOIDED today's mid-box ETH entry.
- **zone-retrace** (trend regime): fire when `AnalyzeStructure` has an aligned pullback zone + price InZone + trend confirmed (NOT neutral — the mistake today). Stop = zone invalidation. Target = measured move.
- **struct-momentum**: fire on the StructMomentum signal (BOS-continuation retrace) for its assigned (symbol,TF). Only for symbols where it's been validated.
- All: `requireAligned` gates against TF-conflict; blackout gate (macro + earnings) skips event windows.

---

## 6. Safety layer (hard gates — ALL must pass before a real order)
1. **Global master**: `cfg.Enabled` AND env `AUTOTRADE_ENABLED=true` (two switches; either off = no auto-orders).
2. **Paper mode default ON** (`cfg.paper=true` → DryRun placement + paper journal, zero real orders).
3. **Per-trade margin cap** + **total-exposure cap** (maxMarginTotalUSDT).
4. **Max concurrent** (global + per-symbol) — never stack.
5. **One-position-per-symbol** — skip if an open position/pending order exists (check journal + BingX Positions API).
6. **Daily-loss kill-switch** (dailyLossHaltR) — halt ALL + ntfy + require manual re-arm.
7. **Blackout gate** — no auto-entry inside a macro/earnings window.
8. **Post-stop cooldown** — don't re-enter a just-stopped setup for N bars.
9. **Wrong-side-of-mark guard** (already in the engine) — don't place a limit already through market.
10. **ntfy on EVERY action** (placed / filled / stopped / killed) — full audit trail; silence must be trustworthy.
11. **Hard STOP command** (ops page / env) — instant halt + cancel-all.

---

## 7. Rollout (phased — do NOT skip)
- **Phase 0 — design sign-off** (this doc + §10).
- **Phase 1 — build, DryRun.** Executor + config + trigger eval + safety gates. Unit-test triggers + guards. All DryRun.
- **Phase 2 — PAPER run (mandatory, ≥1-2 weeks).** `paper=true`: mock-place on real triggers, record to a paper journal, ntfy each. Accrue a sample; measure hit-rate, real-hit-rate (fill quality), netR, no-fill %, max drawdown. Compare vs the read-aids' intent. Prove the entry/stop mechanics are dialed (the thing today showed they're NOT yet).
- **Phase 3 — single-symbol SMALL live.** Flip `paper=false` + `AUTOTRADE_ENABLED=true` for ONE symbol, tiny margin (10-20u), tight caps + kill-switch. Watch closely.
- **Phase 4 — scale** only symbols/strategies that held up in paper AND small-live. Metals/BTC/ETH each on whatever won its own validation.

---

## 8. Risks / mitigations
- **Auto-ing an unvalidated edge** → paper-first + only-validated-setups; kill-switch.
- **Runaway / bug places many orders** → max-concurrent + total-margin cap + global switch + hard STOP + ntfy audit.
- **Stop-hunt / stopped-then-reversed** (today's recurring pain) → stop-outside-box rule + post-stop cooldown; measured in paper before live.
- **Correlated stacking** (long ETH + long SOL + …) → total-exposure cap + max-concurrent counts correlated crypto as shared risk.
- **Exchange/API failure mid-place** → idempotency (one order per rule-fire, dedup by rule+bar), reconcile against BingX Positions on each tick.
- **Key perms** → confirm the BINGX key has trade perms scoped to what's needed; consider a separate sub-account with capped balance for auto-trading.

---

## 9. Cross-cutting checklist (state, don't assume)
- [ ] Two independent kill switches (cfg.Enabled + AUTOTRADE_ENABLED); default OFF.
- [ ] paper=true default; real orders impossible until BOTH paper=false and AUTOTRADE_ENABLED=true.
- [ ] Daily-loss halt + manual re-arm; ntfy on every placed/filled/stopped/killed.
- [ ] Reconcile open positions vs BingX each tick (no double-entry after a restart).
- [ ] Blackout (macro+earnings) respected; closed-bar triggers (backtest parity) except zone-touch which is live-poll.
- [ ] Every auto-trade written to journal with an `auto=true` tag + rule id.
- [ ] chown any written state files (autotrade.json / paper journal) to ubuntu:ubuntu.
- [ ] Separate/capped sub-account recommended before real capital.

---

## 10. Open decisions for your sign-off (before any build)
1. **First strategy(ies) to auto**: range-edge (rec — it's the fix for today) · zone-retrace · struct-momentum. Start with ONE.
2. **First symbol(s)**: SOL (aligned-long candidate) · others? Start with ONE in paper.
3. **Caps**: per-trade margin (rec 20u) · maxConcurrentTotal (rec 2) · maxMarginTotal (rec 100u) · dailyLossHaltR (rec -3R).
4. **Entry style**: limit-at-zone/HVN (rec — resting order fills while away) · market-on-trigger.
5. **Paper duration / acceptance** before live: rec ≥1-2 weeks AND netR>0 AND real-hit-rate ≈ close-hit-rate (fill quality OK) AND no-fill acceptable.
6. **Executor home**: goroutine in cmd/monitor (rec — reuses ntfy/config plumbing) · separate cmd/autotrade.
7. **Sub-account**: use a capped BingX sub-account for auto-trading? (rec yes for blast-radius control.)
