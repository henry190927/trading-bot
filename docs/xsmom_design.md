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

## 8. Results (run 2026-11-17 09:24 UTC; §1–7 committed as cb0f953, harness 6b4936f)

Reproduce: `xsmombt -cache <dir>` (env without BingX keys). BingX 1d history:
BTC/ETH/LINK 2021-05-14, XRP 2021-06-01, SOL/NEAR 2021-11-04, SUI 2023-09-24,
HYPE 2024-12-19; ETH has 3 missing daily bars (an open ETH leg is simply held
over a gap; a rebalance on a missing bar treats ETH as ineligible). First
book: A 2021-12-06, B 2021-11-29, C 2023-10-30 (k = 3 needs 7 eligible).
No rule, parameter or criterion was changed after the run.

### 8.1 Full history, pooled

| arm | fee | n | netR | R/trade | WR | avgW / avgL | SD | SE | trd/day | MDD R closed | MDD R daily MTM | 60 d bucket t | 60 d drop-best |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **A** L30 k2 | 10 bp | 375 | +43.62 | +0.116 | 46% | +0.96 / −0.61 | 1.57 | 0.081 | 0.21 | 10.7 | 14.6 | +1.27 | +13.0 |
| A | 0 bp (gross) | 375 | +45.82 | +0.122 | 47% | | | | | 10.5 | 14.4 | +1.33 | +15.1 |
| B L14 k2 | 10 bp | 514 | +30.32 | +0.059 | 46% | +0.77 / −0.55 | 0.96 | 0.042 | 0.28 | 8.2 | 8.2 | +2.10 | +24.4 |
| C L30 k3 | 10 bp | 306 | +37.26 | +0.122 | 48% | +1.04 / −0.72 | 1.58 | 0.090 | 0.28 | 13.7 | 13.3 | +1.80 | +22.6 |

6 bp: A +0.119, B +0.061, C +0.124 R/trade. Fees are ≈0.006 R/trade (wide
1d stops) — fees are not the issue here.

Per symbol, A at 10 bp (netR, legs): BTC −0.5 (54), ETH −0.3 (52), SOL +17.9
(56), XRP +8.6 (53), SUI −1.4 (29), HYPE +4.5 (24), NEAR +7.7 (48), LINK +7.2
(59). Gross: BTC −0.0, ETH +0.0, SOL +18.1, XRP +8.9, SUI −1.3, HYPE +4.6,
NEAR +7.9, LINK +7.5.

**Side split, A at 10 bp: long legs +43.33 R (190, +0.228/t), short legs
+0.29 R (185, +0.002/t).** B: long +33.2 / short −2.9; C: long +43.2 / short
−6.0. The whole full-history result is the LONG side; the short bottom-k earns
nothing. Exits (A): stop 88, rebalance 283, open 4.

### 8.2 Gate — nested 60/90/120 (+1/2 d jitter) + six 30 d buckets, 10 bp, pooled

| arm | 60 d | 90 d | 120 d | 30 d buckets (off 0 → 150), netR/n | shipgate.Default() |
|---|---|---|---|---|---|
| **A** | −3.2 (20) | −1.3 (27) | +0.4 (29) | −1.9/12 −1.3/8 +1.8/7 +1.8/2 +2.3/6 +1.5/4 | **FAIL** (60 d, 90 d ≤ 0; median R/trade −0.050) |
| B | −1.1 (18) | +0.8 (28) | +4.5 (34) | −1.9/9 +0.8/9 +1.9/10 +3.7/6 +1.2/12 +1.7/7 | **FAIL** (60 d ≤ 0) |
| C | −3.7 (25) | −2.8 (34) | −2.3 (40) | −2.6/15 −1.1/10 +1.0/9 +0.5/6 +6.3/10 +0.1/6 | **FAIL** (all three windows ≤ 0) |

Gross (0 bp) gives the same three FAILs (A 60 d −3.05, 90 d −1.15). Every
per-symbol gate is UNDECIDED (1–6 legs per window).

### 8.3 Correlation with the htf-trend paper book (daily R, 10 bp)

Book on the same data: H4 = 4h close entry (n 2145, +0.083 R/t), T1 = 1d touch
(n 553, +0.219 R/t), BOOK = H4 + T1. Span 2022-10-15 → 2026-11-17 (1494 days).

| arm | vs H4 | vs T1 | vs BOOK | (last 360 d: vs BOOK) |
|---|---|---|---|---|
| A | +0.153 | +0.481 | **+0.308** | +0.264 |
| B | +0.130 | +0.448 | +0.277 | +0.235 |
| C | +0.120 | +0.439 | +0.265 | +0.295 |

Same span: BOOK daily mean +0.195 R, SD 3.43, MDD 65.6 R; A +0.027 R/day, SD
0.56, MDD 14.6 R; BOOK + A: mean +0.222, SD 3.64, MDD 65.4 R — A adds a little
return and essentially no drawdown relief at equal per-leg risk.

### 8.4 Funding (NOT modelled; estimate)

BingX returns ≤ 1000 funding points ≈ the last 11 months (HYPE since
2026-06). On A's 67 legs inside that history: **+0.18 R total, +0.003 R/leg
received** (range −0.006 … +0.019 R/leg by symbol). Two orders of magnitude
below the R/trade and below the fee; ignoring funding does not change any
verdict.

### 8.5 Decision (§6)

1. Gate A: **FAIL** — 60 d −3.19 R, 90 d −1.34 R, median R/trade −0.050.
2. Full-history netR +43.62 > 0 and 60 d drop-best +13.0 > 0 — holds.
3. Neighbour B: R/trade +0.059 but its gate FAILs (60 d −1.11) — fails.
4. corr(A, BOOK) +0.31 — would have been "diversifying" (< 0.4); vs the 1d
   touch leg alone it is +0.48.

**Verdict: REJECTED** (criterion 1). Not wired; no paper rule.

### 8.6 Reading (facts, then untested hypotheses)

- Flow: A is 0.21 legs/day (≈ 1.5 per week); B 0.28. It would not add the
  order flow that was the reason to look at it — the 4h htf-trend leg alone is
  1.4 trades/day.
- The full-history PASS-shaped numbers (+43.6 R, t 1.27) are the long side
  only, concentrated in SOL/XRP/NEAR/LINK. A long-top-k book with a ≈ 0 short
  side is a directional long-alt-momentum book, which is why its daily R
  correlates +0.48 with the 1d trend leg. The "market-neutral dispersion"
  premise of §1 did not show up in this 8-coin universe.
- The latest 60–90 days lose on every arm, so it fails the same recency test
  that htf-trend's own latest year is weak on.
- Hypotheses NOT tested here (each would need its own pre-registration):
  a long-only top-k rotation (the short side carries no edge but full risk);
  a larger universe (XSMOM literature uses dozens of coins; 8 with k = 2 is a
  noisy rank); vol-scaled rather than stop-scaled sizing. None is recommended
  as a flow source: at ≈ 0.2–0.3 legs/day the frequency problem stays.
