# Cross-sectional momentum (weekly rotation) — design + pre-registration

Status: BACKTEST ONLY. No change to `autotrade.json`, the monitor, the
executor, the web or `autostrat` (the live code). Written 2026-11-17 BEFORE any
result was computed. Results go in §8 and must not change §1–§7.

## 1. Why

The paper book's core is htf-trend (Donchian breakout + trail, 4h close entry
and 1d touch entry, 8 symbols). It is a TIME-SERIES trend book: every symbol
asks "is this coin trending?" and the whole book fires together (max
concurrent 9 of 9, docs/htf_trend_design.md §6). The user wants more order
flow from something that is NOT another copy of that.

Everything else tried for flow in this repo was a level/session fade (opens,
session H/L, Market Profile 80 %, sweep-reclaim) and failed once fees were
charged (docs/level_reclaim_design.md, docs/market_profile_80_design.md): the
stops were tight (≈0.3–0.6 %), so 10 bp was 0.2–0.3 R per trade.

Cross-sectional momentum asks a different question: "which coins are
strongest RELATIVE to the others?" — long the top k, short the bottom k. With
k long and k short legs of equal risk, the book is roughly market-neutral, so
its daily PnL should depend on the dispersion between coins, not on the
direction of the market that the htf-trend book is exposed to. The stop is
wide (3×ATR(20) on 1d), so fee drag should be small like htf-trend's.

Known headwinds, stated now:
- 8 symbols is a tiny cross-section (academic XSMOM uses 50–100+ coins); with
  k = 2 the "rank" is noisy.
- Crypto alts are highly co-moving; the long and short legs may mostly cancel.
- Weekly rebalance + equal risk: expected ≈ 1–3 new legs per week ⇒ thin
  30 d buckets (gate floors may decide UNDECIDED).
- Short-horizon (1-week) reversal is documented in crypto; no skip is used
  (§3.2), so that effect is in the result, not designed around.

## 2. Universe and data

Symbols: BTC ETH SOL XRP SUI HYPE NEAR LINK (the paper book's 8; no others,
none dropped). BingX public 1d klines (UTC 00:00 bars), paged back to
2019-01-01 or listing, fetched in chunks with per-chunk retry on code 100410,
cached. A symbol participates from the first rebalance at which it is eligible
(§3.3); SUI and HYPE enter late, so the cross-section is 6 → 8 over history.

## 3. Rules (exact)

All evaluation is on CLOSED 1d bars.

### 3.1 Rebalance schedule

A rebalance bar is a daily bar whose OpenTime is **Sunday 00:00 UTC** — its
close is Monday 00:00 UTC. Decisions use data up to and including that bar's
close; fills are at that bar's Close (a market order at the Monday 00:00 UTC
open, modelled as the Sunday close, as htf-trend fills at the signal close).

### 3.2 Signal

`ret_s = Close_s[t] / Close_s[t − L days] − 1`, using the bar whose OpenTime is
exactly L days before t (no skip of the most recent bar, S = 0).

### 3.3 Eligibility and ranking

A symbol is eligible at rebalance t iff it has a bar at t, a bar exactly L days
earlier, and ATR(20)[t] > 0 (≥ 21 bars of history). If fewer than **2k + 1**
symbols are eligible, the desired book is empty (every open leg is closed at
Close[t]). Otherwise rank by `ret` descending, ties broken by symbol name
ascending; desired side = long for the top k, short for the bottom k, flat for
the rest.

### 3.4 Legs, sizing and stop

- Each leg risks the same fraction of equity (0.25 % in paper terms). The
  stop distance is `D = 3 × ATR(20)[t]` (Wilder, `indicator.ATR`, bar t
  included — it is closed). **1R = D**; a leg's R is `side × (exit − entry) / D`.
  Equal risk per leg ⇒ R is directly summable across legs = portfolio R.
- The stop is FIXED (no trail, no target). On every bar j after the entry
  bar: a long exits at `Open[j]` if `Open[j] ≤ stop` (gap), else at `stop` if
  `Low[j] ≤ stop`. Short mirrors. There is no favourable intrabar event, so
  intrabar ordering cannot flatter the result.
- A stopped leg stays flat until the next rebalance (no mid-week re-entry).

### 3.5 Order of events on a rebalance bar t

1. Stop check on bar t for legs entered before t (as any bar).
2. Rank (§3.3) on Close[t].
3. An open leg whose desired side differs (other side or flat) exits at
   Close[t].
4. An open leg whose desired side is unchanged is HELD: same trade, same
   entry, same stop, same 1R. No fee, no resize.
5. A desired side with no open leg (new, flipped, or stopped earlier —
   including stopped on bar t itself) enters at Close[t] with a fresh stop.

A flip (long → short) is therefore two trades: the old leg's exit and the new
leg's entry, each paying its own round trip.

### 3.6 Fees, funding, open legs

- **Fee** per trade (= leg), round trip, `fee_R = feeBps/10000 × entry / D` —
  the `autotrade.WithFee` conversion the rest of the book uses. **10 bp is the
  verdict fee** (taker in + taker/stop-market out); 6 bp and 0 bp (gross) are
  reported for reference.
- **Funding is NOT modelled in the verdict.** An estimate is reported: for
  legs whose whole life lies inside the BingX funding history the endpoint
  returns (≤ 1000 points per symbol), `funding_R = Σ side × rate_k × entry / D`
  over funding times in (entry, exit] (a long pays a positive rate). Mean
  funding R/trade and its total over that sub-sample are informational.
- A leg still open at the last closed bar is marked to that close, counted as
  open, and pays the fee.

### 3.7 Definition of a "trade" for the gate

A trade is ONE LEG from entry to exit (stop, rebalance exit, or mark at the
end). It is attributed to the window / bucket containing its ENTRY bar
(CloseTime), exactly as trendbt does, so disjoint buckets partition the trade
list.

## 4. Arms (pre-registered; no others will be run for the verdict)

| arm | L (days) | k | role |
|---|---|---|---|
| **A** | 30 | 2 | primary |
| B | 14 | 2 | robustness neighbour on L |
| C | 30 | 3 | k neighbour (informational; cannot rescue A) |

Fixed for all: 1d, Sunday-bar rebalance, S = 0, stop 3×ATR(20), no trail.

## 5. Harness — `cmd/xsmombt`

- One continuous simulation over the common daily timeline of the 8 symbols.
- Reported for each arm, pooled, at 10 / 6 / 0 bp: n, netR, R/trade, WR,
  avgW/avgL, SD, SE = SD/√n, trades/day, MDD in R (closed trades in exit
  order), daily mark-to-market R (per UTC day, change of the leg's
  `TrendTrade.MTMR` at the last bar closed by the day's end, fees at entry)
  with its MDD, mean/SD; per-symbol netR contribution; long/short split
  (informational).
- Gate: nested 60/90/120 d ending now, each also read with the end shifted 1
  and 2 days (jitter), plus six disjoint 30 d buckets at end-offset 0..150,
  through `shipgate.Evaluate(arm, nil, shipgate.Default())` (absolute; no
  baseline — this is a new strategy). Full-history 60 d disjoint buckets:
  bucket-level t and total without the best bucket.
- Correlation: Pearson correlation of A/B/C daily R with the paper htf-trend
  book run on the same 8 symbols at 10 bp and default parameters —
  **H4 = `autostrat.SimulateHTFTrend` on 4h**, **T1 = `autostrat.SimulateHTFTrendTouch`
  on 1d**, and **BOOK = H4 + T1** — over every day from
  max(2022-10-15, first xsmom entry) to the last closed day, and over the
  last 360 days.
- Tests: hand-traced small scenario (5 symbols, every-2-day rebalance, L = 2,
  k = 1, ATR period 2; expected values traced by hand before writing them);
  no-lookahead poison test (every bar after a cut replaced by garbage — every
  leg entered at or before the cut has identical side/entry bar/entry/stop,
  and every leg exited before the cut is identical); inert-param test (L, k,
  stop multiple and fee each change the output); stop gap-fill and
  stopped-then-re-entered-at-rebalance mechanics.

## 6. Decision rules (decided before results; arm A, 10 bp, pooled)

- **PASS (paper-eligible; wiring is a separate decision)** iff ALL:
  1. `shipgate.Default()` (no baseline) on nested + jitter + 30 d buckets →
     PASS;
  2. full-history netR > 0 and the full-history 60 d bucket total without the
     best bucket > 0;
  3. neighbour B (L = 14): full-history R/trade > 0 and its gate is not FAIL —
     otherwise a PASS is downgraded to UNDECIDED (parameter-fragile);
  4. Pearson daily-R correlation with BOOK over the full common span < 0.4
     (diversifying). 0.4–0.7 → downgraded to UNDECIDED (overlapping);
     > 0.7 → REJECTED (duplicate — the purpose of this strategy).
- **REJECTED** if (1) is FAIL or (2) fails.
- Otherwise **UNDECIDED** (e.g. the gate says UNDECIDED on sample size): not
  wired; at most a user-decided paper forward test.
- C (k = 3) is reported and cannot change A's verdict.
- Gross (0 bp) is reported to separate "no edge" from "edge eaten by fees".

## 7. Not done regardless of results

No other L, k, skip, rebalance day/frequency, stop multiple, ATR period,
trail, vol-scaling, market-neutral beta hedge, regime filter or TF. No symbol
dropped or added. Per-symbol contributions are reported and never used to
choose symbols. Any idea from the results is written as an untested
hypothesis in §8.

## 8. Results

(filled after the run; §1–7 above are frozen at the commit that adds this file)
