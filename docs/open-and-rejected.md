# Roadmap — open, and deliberately not done

The hand-maintained changelog that used to live in the README had drifted about three
months: of the nine items listed as "Open", **seven had already shipped**
(R histogram, daily-R calendar, equity curve, WebSocket streams, position-size
math, uPlot mini-charts, JSON poll endpoint). Maintaining a second history
next to git guarantees exactly that outcome, so this section no longer tries.

**For what shipped, `git log` is authoritative** — commit messages here carry
the reasoning and the A/B numbers, not just the diff. This section keeps only
what git cannot tell you: what is deliberately NOT done, and why.

### Genuinely open

| Item | Status |
|---|---|
| Per-symbol score thresholds | Backtested — XAG=3 helps (+11-15R/window across 60/90/120d). Held pending live confirmation. |
| Liquidation heatmap provider | `analyzer/liq_heatmap.go` has the Provider interface and `Cluster()`; no live implementation. The biggest remaining data gap for stop placement. |
| Same-symbol same-direction dedup — **SHIPPED INERT 2026-09-10** | `max_same_symbol_side` in `autotrade.json`, enforced in `CheckCaps` after the daily-loss halt. Refuses a REPEAT of an opinion already on the book: permits ETH long + ETH short (a hedge) and refuses ETH long + ETH long. Direction, not a count, because a count of 2 still permits two ETH longs. Defaults to 0 = unlimited, so it does nothing until set — flip it at a sampling-window boundary or the before/after R is not comparable. |
| Pre-trade cash-open stop check — **3 of 4 SURFACES SHIPPED 2026-09-10** | Now called from `/validate`, `/api/chart/validate` and `cmd/validate`, checked against the 1.5×ATR suggested stop. **Still open, and it is the half that matters**: `/journal/new`'s confirm modal is where the operator types their OWN stop before real orders go out, its warning list is assembled client-side, and it is the only surface the 2026-09-10 SNDK loss would have passed through. |
| Read-only demo build | Every data path is already env-overridable and the account endpoints already 503 without an API key, so a fixture-backed public instance is a route allowlist plus fixtures — not an auth system. |
| Fixture identifier test | Fail CI when a captured API response in a `*_test.go` still carries `userId`/`shortUid`-style keys. |
| Reconcile the naked-position guards fully | `orphanCycle` and `/ops/verify` now enumerate from the EXCHANGE and use the journal only to exclude (2026-09-10) — a position placed outside the journal is no longer invisible. `bracketCycle` itself still enumerates journal-first for its per-trade path; folding the two into one reconciliation pass would remove the remaining duplication. |
| Make "zero armed auto zones" say WHY | `/ops/zones` can now show `armed: []` for two different reasons — no trend-aligned pivot zone exists, or something broke — and cannot tell them apart. Needs a skip-reason count per (symbol, TF). |

### Rejected, and kept rejected

Recorded so the same idea does not get relitigated. Several cost real work to
disprove, which is the point of writing them down.

| Idea | Verdict |
|---|---|
| Higher-TF bias filter | Hurt the mean-reversion edge (ETH +6.5R → −2.4R). Revisited after sweep-invalidation and range-expansion voting shipped; still no. |
| Engine reading the forming bar | Breaks live/backtest parity. The closed-bar rule is load-bearing — fix fill problems at the execution layer, never in the engine. |
| Shelf-retest strategy (`shelfbt`) | Run and rejected: the parameter surface has no peak. |
| Session-open alignment as a scoring factor | Measured 2026-09-09 with `opensab` at fire-generation: `mixed`, `dw-aligned` and `mo-aligned` all FAIL `shipgate` in every window, losing to the unfiltered baseline. One per-symbol PASS (BTC) out of 18 gate runs — about what noise produces. |
| Proximity votes (distance to HVN, to EQH/EQL, to an open) | Distance dilutes a count threshold. Only EVENTS score. Rejected three separate times, once per level type. |
| New trading pairs beyond the disciplined roster | Scope discipline; the roster is deliberately small. |
| BingX chart drawing | No API for it. |
| FVG / imbalance continuation (`fvgbt`) | Measured 2026-10-07: FAIL. The best arm (`-max-age 3`) ran 60/90/120d +19 / +16 / −7R — it beat the baseline in all three windows and still missed the absolute floor ("better than a disaster is not an edge"). Gap size was monotonically harmful; big imbalances are impulses, not levels. |
| **Arming the rest of the live-setups grid (2026-10-21)** | Every unarmed (symbol × strategy) cell on `/ops/scan` — 51 of them, 17 symbols × {range-edge, engine, sweep-reject, htf-snr} minus the configured ones — went through its own harness (`rangebt` / `backtest` / `sweepbt` / `snrbt`) at 60/90/120d, then `cmd/gate` on absolute grounds; cells that passed got four DISJOINT 30-day buckets (`-end-offset` 0/30/60/90). **36 FAIL · 9 UNDECIDED (one month carries the whole result) · 6 PASS.** Of the six, only two clear ≈2 standard errors on R/trade at 120d (SE ≈ 1.4/√n): **MSTR range-edge** (+0.37R/trade, n=112, ≈2.8 SE) and **SNDK range-edge** (+0.31, n=106, ≈2.3 SE); **NEAR sweep-reject** is weaker (+0.16, ≈1.2 SE) but is the only cell with no losing bucket. Those three were ARMED as paper rules; XRP sweep-reject (≈0.9 SE), XRP engine (≈0.7 SE, and `backtest` is not the daemon's exact engine rule) and MSTR sweep-reject (≈0.5 SE) are watched, not armed. Fifty-one cells at once means a few passes are expected from noise alone — the SE column is why only three went in. UNDECIDED: XAU/XAG/APP/SOL/SUI/HYPE/UNI range-edge, XAG/HYPE sweep-reject. Runner: `scripts/gate_grid.py`. |
| **Extending the N字 structure veto past 1h** | Measured 2026-09-10 with `cmd/backtest -struct-veto`: 48 runs, 4 symbols × {2h,4h} × 60/90/120d × {base, veto}, each cell through `shipgate.Evaluate` against its own baseline. **0 PASS — 5 FAIL, 3 UNDECIDED (the veto thins them below the sample floor).** XAG 2h settles it: the veto arm is PROFITABLE in absolute terms (median R/trade +0.201) and still fails, because it beat the baseline in 0 of 3 windows — it cuts the best edge in the book from +9.64/+15.45/+17.09R down to +1.81/+5.73/+2.70R. ETH 4h is the only cell that behaved as designed (beat baseline 3/3) and is UNDECIDED on n=5. The un-vetoed 2h/4h gap is therefore a MEASURED cost, not an oversight. |

### Still on the table

1. **Minimum fee-budget filter** — skip any setup where `fee_R > 0.3R`. Cheap, untested.
2. **MTF veto for breakdown only** — the narrow form of the rejected MTF bias: do not suppress all counter-trend entries, only longs into a clean 4h breakdown. Note that the *broad* form of this idea — extending the N字 structure veto to 2h/4h — was measured and rejected on 2026-09-10 (see above), so this narrower version starts from a worse prior than it did.
