# level-reclaim — 1m action after a higher-TF close holds / reclaims a session level (design, pre-registered)

Status: DESIGN + BACKTEST ONLY. Not in `EvalAutoTrigger` / `KnownStrategies`,
not in `autotrade.json`. Written 2026-11-16 BEFORE any result was looked at.

## 1. Why

The user's discretionary read (BTC 2026-11-16, 1m chart): price ran through
Asia High at the 22:30 (UTC+8) cash open, could not hold above D/W Open,
and the short was taken on the 1m breakdown with the stop above the chop
high and a 2R target. Their words: watch small-TF action, use **how a few
higher-TF candles close relative to the levels** — 站穩 / 收回 日開、亞洲開
— and enter on the 1m action without waiting for a retest. All levels count.

## 2. What already failed, and how this differs

| | opensab (2026-09-24) | sweepbt AMD (2026-09-30) | sweep-reject (fee re-gate 2026-11-14) | **level-reclaim (this)** |
|---|---|---|---|---|
| level | — (opens used as a FILTER) | Asia range H/L | EQH/EQL pool | **D/W/session opens + session/day H/L** |
| what decides | sweep-reject fire, kept if aligned with opens | sweep inside London→NY | 1h wick through pool, close back | **15m/1h CLOSE relative to the level** (hold or reclaim) |
| entry | 1h close | 1h close | 1h close | **1m displacement bar after the HTF close**, market |
| result | worse 3/3 windows | FAIL 0/3 | FAIL at 6bp | ? |

New here: the level is what is traded (not a filter); the decision is a
higher-TF close (the user's 站穩 / 收回), and the entry/stop are on 1m.

Known headwinds, stated now so a PASS is not over-read:
- **Fees.** fee_R = fee% / stop%. A 0.40% stop at 10bp round trip costs
  0.25R; break-even WR at 2R rises from 33% to ~42%.
- **Clock / open families have failed 5×** here (opensab, AMD, C9 family);
  cash-open bar risk is concentrated (`project_cash_open_bar`).
- **Multiple arms** (§4): 8 cells. One PASS among 8 is weak evidence by
  itself; §5 requires a neighbour to agree.

## 3. Exact rules

All times DST-aware in America/New_York (ET) unless marked UTC, matching the
ICT session indicator on the user's chart. Every 1m bar is CLOSED when read;
HTF bars are aggregated from 1m bars (UTC-aligned buckets), so the HTF bar
closing at T uses only 1m bars with CloseTime ≤ T.

### 3.1 Levels (all active at once)

| id | price | active |
|---|---|---|
| DO | open of the 00:00 UTC 1m bar | that UTC day |
| WO | open of Monday 00:00 UTC | that week |
| AO | open of the 20:00 ET bar (Asia open) | 24h from 20:00 ET |
| LO | open of 02:00 ET (London open) | 24h |
| NYO | open of 09:30 ET (NY cash open) | 24h |
| PDH / PDL | prior UTC day high / low | that UTC day |
| AH / AL | Asia session 20:00–00:00 ET high / low | from 00:00 ET, 24h from session start |
| LH / LL | London 02:00–05:00 ET high / low | from 05:00 ET, 24h from session start |
| NYH / NYL | NY AM 09:30–11:00 ET high / low | from 11:00 ET, 24h from session start |

A level is usable only once it is fully formed (H/L after its session ends).
Two levels within 0.05% of each other are one level (first id kept) so a
coincident D/W open (every Monday; AO in EDT) does not fire twice.
"opens" set = DO WO AO LO NYO. "all" set = every row.

### 3.2 HTF decision (站穩 / 收回)

At the close of HTF bar j (HTF ∈ {15m, 1h}), for each active level L:
- **long event**: close_j > L and (close_{j-1} < L **or** low_j < L)
  — crossed back above (收回), or tested below and closed above (站穩).
- **short event**: mirror.
- K = 1: the event bar is the signal. K = 2: the event at j−1 plus
  close_j on the same side of L; signal at j.
- Each (level, side) signals at most once per active period.

### 3.3 1m action entry (no retest)

From the signal time T, scan the next 60 one-minute bars:
- cancel if a 1m bar closes back on the wrong side of L;
- **trigger** on the first 1m bar b with: body in the trade direction,
  |close−open| ≥ 1.0 × ATR20(1m, as of b−1), close in the outer 30% of the
  bar's range (top 30% long / bottom 30% short), close beyond L.
- entry = close_b, market.

### 3.4 Stop / target

- stop = extreme of the 1m bars from the open of the (first) event HTF bar
  through b (long: lowest low; short: highest high) ± 0.1 × ATR20(1m).
- skip when stop distance < 0.15% (fee would be ≥ 0.67R at 10bp) or > 2%.
- TP = 2R. No timeout (as the paper executor). One position per symbol at
  a time (`DedupFires`, cooldown 0).

## 4. Arms (fixed now)

HTF {15m, 1h} × K {1, 2} × level set {opens, all} = 8 cells.
Symbols BTC, ETH (`feedback_strategy_scope`). 1m BingX history.

## 5. Falsification criteria (decided before results)

- Scored at **10bp** round trip (taker in + taker out; conservative for TP
  exits). 6bp printed for reference only.
- Windows 60/90/120d nested + six disjoint 30d buckets (end-offset
  0…150), through `shipgate.Default()` (absolute floor, bucket breadth).
- A cell is a **candidate** only if: shipgate PASS on the pool, AND at
  least one neighbouring cell (other K, or other HTF, same level set) has
  median R/trade > 0. A lone PASS among 8 is reported, not shipped.
- No parameter in §3 is tuned after results. A candidate goes to paper
  only (new strategy name, separate design for the executor); never live
  from this test.

## 6. Result

(to be filled after the run)
