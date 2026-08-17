# Fundamental Layer — F3: Earnings Blackout (MVP landing spec)

The cheapest, highest-value slice of the US-stock fundamental layer: gate out new
technical entries around earnings releases. Mirrors the existing `macro` package
(CPI/FOMC/NFP blackout, `macro/events.go`) but with two deliberate differences.
Scope: F3 only. F1 (quality bias) / F2 (valuation) are separate, forward-log-validated.

## 0. Why F3 first
- Earnings gaps are the single biggest killer of a technical setup on a stock — the
  synthetic (NCSK*) tracks the underlying, so it gaps when the company reports.
- It's the **only fundamental sub-signal that backtests cleanly**: earnings DATES are
  point-in-time (no look-ahead), unlike restated financials (see the F1/F2 caveat).
- Mechanically it's the same blackout gate we already run for metals macro events.

## 1. Two differences from `macro`
| | macro (existing) | earnings (F3) |
|---|---|---|
| scope | **global** (CPI hits everything) | **per-symbol** (SNDK earnings blacks out only SNDK) |
| source | **embedded** events.json, redeploy to refresh | **runtime** file refreshed by cron (like zones.json) |
| window | minutes (release is a point event) | **hours** (after-hours drop → overnight gap on the 24/7 synthetic) |

So F3 is a NEW package `earnings/`, parallel to `macro/`, not an extension of it.

## 2. Data source
- **Finnhub** free tier: `GET /calendar/earnings?from=&to=&symbol=` → date + hour (bmo/amc) + est.
  Free tier RPM is plenty: 5 symbols × 1 refresh/day.
- Historical earnings dates (for backtest parity) also from Finnhub `/calendar/earnings`
  with a wide `from`.
- Key in `.env` as `FINNHUB_KEY`, per-user (same pattern as `GEMINI_API_KEY`).
  **NEVER grep .env for KEY/SECRET lines** — read presence only, never value.
- Symbol map: `NCSK<TICKER>2USD-USDT → <TICKER>` (SNDK, NVDA). One helper
  `earnings.UnderlyingTicker(sym)`; returns "" for non-stock symbols (BTC/ETH/XAU/XAG
  → no earnings gate, ever).

## 3. Storage — `/opt/trading/earnings.json` (cron-refreshed, like zones.json)
```json
{
  "updated_utc": "2026-08-18T00:00:00Z",
  "events": [
    {"symbol":"SNDK","datetime_utc":"2026-08-27T20:05:00Z","when":"amc",
     "blackout_before_min":120,"blackout_after_min":960},
    {"symbol":"NVDA","datetime_utc":"2026-08-27T20:20:00Z","when":"amc",
     "blackout_before_min":120,"blackout_after_min":960}
  ]
}
```
- `when`: "amc" (after market close) / "bmo" (before market open) — drives default window.
- Default window: `amc` → before 120 / after 960 (16h, covers the overnight gap on the
  synthetic); `bmo` → before 720 / after 240. Tunable per event; sensible defaults if 0
  (mirror macro's `<=0 → default` guard).
- Refresh: `trading-fundamentals` cron (or a goroutine in trading-monitor) daily at ~00:10 UTC:
  fetch Finnhub → write earnings.json → **`chown ubuntu:ubuntu`**. If fetch fails, keep the
  old file (stale-but-present beats empty = "no blackout ever"). Log fetch result.

## 4. Package `earnings/` (mirror macro's shape)
```go
type Event struct {
    Symbol        string    `json:"symbol"`
    DatetimeUTC   time.Time `json:"datetime_utc"`
    When          string    `json:"when"`
    BeforeMinutes int       `json:"blackout_before_min"`
    AfterMinutes  int       `json:"blackout_after_min"`
}
func (e Event) Window() (start, end time.Time)   // same as macro
func (e Event) Contains(t time.Time) bool

// Runtime-loaded (NOT embedded): reads /opt/trading/earnings.json, hot-reloads on mtime
// change like zone.Config. Path overridable via EARNINGS_FILE for tests/backtest.
func Load(path string) error
func ActiveAt(sym string, t time.Time) *Event      // symbol-scoped; nil for BTC/ETH/XAU/XAG
func NextUpcoming(sym string, t time.Time, lookahead time.Duration) *Event
```
Key contrast: every query is **symbol-scoped**. `ActiveAt("BTC", …)` is always nil.

## 5. Gate point — mirror macro exactly, add the symbol scope
macro already gates inside `signal.Evaluate` (returns Flat + "macro blackout" reason).
F3 sits **right next to it**, using `Inputs.Symbol`:
```go
// in Evaluate, alongside the existing macro.ActiveAt check
if ev := earnings.ActiveAt(short(in.Symbol), in.Candles[last].CloseTime); ev != nil {
    return flatSignal(fmt.Sprintf("earnings blackout: %s %s", ev.Symbol, ev.When))
}
```
- Closed-bar parity preserved: the gate is a deterministic function of (symbol, bar-close
  time, event list) — identical in backtest and live **iff the event list is point-in-time**.
  It does NOT touch the closed-bar scorer, so it's consistent with
  "gate in engine is fine, don't change the scorer". [[feedback_engine_closed_bar_only]]
- This is a Layer-0 HARD GATE in the fan-out lattice sense — same role as macro_blackout.
  [[project_ai_analyze_fanout]]

## 6. Surfaces (reuse existing plumbing)
- **Dashboard banner**: handlers.go already renders a macro-blackout banner (line ~170) +
  `NextUpcoming`. Add an earnings variant, per-symbol ("SNDK earnings in 3d — blackout amc").
- **AI context**: ai/context.go + ai/symbol_context.go already list macro events within ±24h.
  Add the symbol's earnings event the same way → the advisor sees it as ground truth.
- **/ops**: earnings.json shown read-only next to zone-config (armed windows + last refresh).

## 7. Backtest = the ship-gate (this is the point-in-time-clean part)
- Load historical earnings dates → re-run the SNDK+veto / NVDA+zone A/B over 60/90/120d
  **with vs without** the earnings gate.
- Hypothesis: skipping earnings windows removes a chunk of the path-dependent losses
  (gap-throughs) without killing the edge. If netR improves or drawdown drops on both
  symbols across all three windows → ship. [[feedback_strategy_changes]]
- Only earnings DATES are used here (point-in-time safe). No fundamentals numbers involved.

## 8. Staging / files
1. `earnings/earnings.go` (package, runtime load, symbol-scoped queries) + unit tests
   (hand-trace the window math before committing expected values). [[feedback_test_authoring_verification]]
2. Finnhub fetch + `earnings.json` writer (cron or monitor goroutine) + chown.
   [[feedback_vps_file_ownership]]
3. Gate hook in `signal.Evaluate` (symbol-scoped, next to macro).
4. Backtest A/B (with/without gate) — decide ship.
5. Banner + AI-context + /ops surfaces.

MVP to first backtest = steps 1–4. Surfaces (5) after the A/B says ship.
```
NCSK<TICKER>2USD-USDT ──map──▶ ticker ──Finnhub──▶ earnings.json ──earnings.ActiveAt(sym,t)──▶ L0 gate in Evaluate
```
