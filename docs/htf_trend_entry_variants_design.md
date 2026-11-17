# htf-trend entry variants — TOUCH entry and TSMOM (design + pre-registration)

Status: BACKTEST ONLY. No change to `autotrade.json`, the monitor, the
executor, the web or `autostrat` (the live code). Written 2026-11-17 BEFORE any
result was computed. Results go in §7 and must not change §1–§6.

Baseline: htf-trend as on paper since 2026-11-16 (docs/htf_trend_design.md
§2, N20 / M10 / K3, both sides, 4h `htf-trend` and 1d `htf-trend-1d`,
`autostrat.SimulateHTFTrend`). Symbols: BTC ETH SOL XRP SUI HYPE NEAR LINK
(the paper book's 8; XAG is not in it and is not run).

## 1. Why

Two questions about the ENTRY of the strategy now on paper, with the exit held
fixed:

1. **TOUCH (不看收盤的順勢).** The baseline waits for the 4h / 1d bar to CLOSE
   beyond the prior 20-bar channel and buys that close. A resting stop-entry at
   the channel edge gets in earlier — at the level instead of at a close that
   may be well beyond it — at the cost of the false breaks that poke the level
   and close back inside. Which effect is larger is an empirical question.
2. **TSMOM (time-series momentum).** A different trend signal — the sign of the
   N-day return — through the same stop/trail. If it is profitable AND not
   just the Donchian book again, it is a diversifier; if it is highly
   correlated with the baseline, it adds nothing to what is already on paper.

## 2. Fixed across every arm (only the entry differs)

- Initial stop = 3 × ATR(20) (Wilder, `indicator.ATR`) from the fill; 1R = that
  distance; every R is of the initial risk.
- Trail = `autotrade.WalkTrail` from the entry bar: the stop for bar j+1 is the
  ratcheted opposite 10-bar channel of bars ≤ j (includes the entry bar), exit
  at the stop or at the bar open on a gap through it. No target.
- One position per symbol; after an exit on bar j the next entry needs bar ≥ j+1.
- Fees: round trip through the same `autotrade.WithFee` conversion as the
  baseline (`trendFeeR`). **10 bp is the verdict fee**, 6 bp is reported for
  reference. An open trade is marked to the last close and pays the fee.
- Warm-up: no entry before bar max(20, 20) + 1 = 21 of a symbol's history
  (TSMOM additionally needs bar N+1).
- A trade belongs to the window / bucket containing its ENTRY bar (bar
  CloseTime, for every arm — for TOUCH the fill is inside that bar).

## 3. Arms (exact rules)

### 3.1 C — close entry (baseline), 4h and 1d

`autostrat.SimulateHTFTrend`, unchanged (docs/htf_trend_design.md §2): close of
bar i beyond the channel of bars i-20..i-1, filled at Close[i], stop from
ATR[i].

### 3.2 T — touch entry, 4h and 1d

While flat, before bar i opens (i ≥ 21, i ≥ last exit + 1), two stop-entry
orders rest at `upper = max High[i-20..i-1]` and `lower = min Low[i-20..i-1]`
— both known before bar i opens. ATR = ATR(20)[i-1] (known before bar i).

- Long touched if `High[i] ≥ upper`; short touched if `Low[i] ≤ lower`.
- Fill: long `max(upper, Open[i])`, short `min(lower, Open[i])` (a gap through
  the level fills at the open).
- Both touched in one bar: the order whose level is nearer Open[i] filled
  first (an order the open gapped through has distance 0); the other is
  cancelled (one position). Exact tie → no entry on that bar. The count of
  both-touched bars is reported.
- Initial stop: long `fill − 3×ATR[i-1]`, short mirrors.
- **Same-bar ambiguity, conservative:** if the entry bar also reaches the
  initial stop (`Low[i] ≤ stop` for a long), the trade is a stop on bar i at
  the stop price, regardless of the true intrabar order.
- Otherwise the trade is walked by `autotrade.WalkTrail(cs, i, side, stop, 10)`
  exactly like the baseline (first trail update uses bars ≤ i).

### 3.3 M — TSMOM, 1d only

On closed 1d bars: `r_i = Close[i] / Close[i-N] − 1`, `s_i = sign(r_i)`; an
exact 0 keeps `s_{i-1}`. No deadband, no flat state.

- **Entry:** while flat, on a FLIP bar (`s_i ≠ s_{i-1}`, i ≥ max(21, N+1)),
  enter side s_i at Close[i] with stop 3×ATR(20)[i] — i.e. the baseline's
  `runTrail` on a different trigger bar.
- A flip while a position is open, or on its exit bar, is skipped (as the
  baseline ignores signals while in a position). The trail is the only exit;
  no exit on a flip. After a trail exit the next entry is the next flip.
- **Primary N = 30** (bars = days). **Robustness neighbour N = 60** (the only
  other value run). Nothing else.

Pre-registered expectation, written so a result can be checked against it:
TOUCH should have more trades, a lower WR, a smaller average loss (fill nearer
the level) and a daily-R correlation with C well above 0.7 — it is a
replacement candidate, not a diversifier. TSMOM-30 should be slower and less
correlated with C.

## 4. Harness

`cmd/trendbt`:
- `-entry close|touch|tsmom` (default `close`) and `-tsmom-n` (default 30) on
  the existing single-arm path; with the defaults the output is byte-identical
  to before (checked by diff of the default run).
- `-variants-run`: the pre-registered run of §5 in one command, all arms on the
  same cached klines, both fees.
- Tests: no-lookahead (poison every bar after a cut; every entry before the
  cut — side, bar, fill, initial stop — is identical), inert-param (touch and
  tsmom produce different trade lists from close on the same data; N=30 ≠
  N=60), touch mechanics (gap fill, same-bar stop, both-touched order) and
  TSMOM flip bookkeeping on hand-built bars.

## 5. Measurements

For each arm, pooled over the 8 symbols and per symbol, at 10 bp and 6 bp:

- Nested windows 60 / 90 / 120 d ending now, each also read with the end
  shifted 1 and 2 days (jitter), and six disjoint 30 d buckets at end-offset
  0, 30, …, 150 → `shipgate.Evaluate(…, shipgate.Default())`.
- Full available history (informational and for criteria below): n, netR,
  R/trade, WR, SD, SE = SD/√n, trades/day, MDD in R (closed-trade equity in
  exit order, as trendbt), and the 60 d disjoint-bucket series over the whole
  history (bucket-level t, drop-best total).
- Daily R: per UTC day, the pooled change of `TrendTrade.MTMR` at the last bar
  closed by that day's end (fees at entry). Pearson correlation of daily R of
  each arm with C-4h, C-1d and C-book (= C-4h + C-1d, what is on paper), over
  every day from **2022-10-15** (first 4h warm signal) to the last closed day,
  and over the last 360 days.

## 6. Decision rules (decided before results; 10 bp, pooled)

### 6.1 TOUCH (judged per TF against C on the same TF)

- **ADOPT-candidate** iff ALL:
  1. `shipgate.Default()` with C as the baseline (beat C's netR in every
     nested window, absolute floors, sign-robust under jitter, bucket breadth)
     → PASS;
  2. full-history R/trade(T) > R/trade(C);
  3. full-history 60 d bucket-level paired difference (T − C netR per bucket)
     has t ≥ 1 (not one period).
- **FAIL** if the gate says FAIL, or (2) fails.
- Otherwise **UNDECIDED**.
- Correlation with C is reported, not judged (T is a replacement, not an
  addition).

### 6.2 TSMOM (N = 30)

- **PASS** iff ALL:
  1. `shipgate.Default()` absolute (no baseline) → PASS;
  2. full-history netR > 0 and the 60 d bucket total without the best bucket
     > 0;
  3. neighbour N = 60: full-history R/trade > 0 and its gate is not FAIL —
     otherwise a PASS is downgraded to UNDECIDED (parameter-fragile).
- **FAIL** if the gate says FAIL or (2) fails.
- Duplicate test (separate label, applied to a PASS): Pearson daily-R
  correlation with C-book over the full common sample. > 0.7 →
  **DUPLICATE** (no portfolio value over what is on paper); 0.4–0.7 →
  overlapping; < 0.4 → diversifying.
- The gate run with C-1d as baseline is reported for information only.

### 6.3 Not done regardless of results

No change to N20 / M10 / K3, the ATR period, the trail or the fee; no TSMOM N
other than 30 and 60; no deadband or flat state added after the fact; no
symbol dropped; no 4h TSMOM. Per-symbol verdicts are reported and never used
to choose symbols. Any idea from the results is written as an untested
hypothesis in §7.

## 7. Results

(filled after the run; §1–6 frozen at the commit that introduced this file)
