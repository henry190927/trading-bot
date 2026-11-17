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

## 7. Results (run 2026-11-17 07:42 UTC; §1–6 committed as 4b1abeb)

Reproduce: `trendbt -variants-run -cache <dir>` (harness 3987391, shipgate
fix 8e53f77). BingX history: 4h from 2022-10-09 (SUI 2023-05, HYPE 2024-12),
1d from 2021-05 (SOL/NEAR 2021-11, SUI 2023-09, HYPE 2024-12). Default
`trendbt` output was checked byte-identical before/after the harness change
(4h, 1d `-report`, 4h `-report -trades` at 6 bp, same cached klines).

**Harness correction during the run (disclosed).** The first run passed the
baseline to `shipgate.Evaluate` with its jitter readings; Evaluate collapsed
the ARM's jitter to medians but keyed the baseline's raw readings by window,
so the baseline's last reading (end-offset 2) was used. That made T-4h "lose"
the 90 d window (+22.92 vs the baseline's off-2 reading +23.81; its median is
+3.82) → FAIL. Fixed in `shipgate` (baseline collapsed to the median like the
arm, with a test) and re-run; only T-4h's verdict changed (FAIL → UNDECIDED).
No rule, parameter or criterion was changed. Run-1 output kept for reference.

### 7.1 Full history, pooled, 10 bp

| arm | n | netR | R/trade | WR | SD | SE | trd/day | MDD R (closed) | MDD R (daily MTM) | 60 d bucket t | 60 d drop-best |
|---|---|---|---|---|---|---|---|---|---|---|---|
| C-4h (paper) | 2145 | +178.50 | +0.083 | 34% | 1.35 | 0.029 | 1.43 | 44.8 | 53.7 | +2.38 | +136.2 |
| **T-4h** | 2801 | +248.22 | +0.089 | 35% | 1.43 | 0.027 | 1.87 | 43.0 | 52.2 | +2.67 | +192.5 |
| C-1d (paper) | 448 | +78.98 | +0.176 | 39% | 1.88 | 0.089 | 0.23 | 16.5 | 24.9 | +1.55 | +38.7 |
| **T-1d** | 553 | +120.84 | +0.219 | 38% | 2.00 | 0.085 | 0.28 | 17.3 | 23.9 | +1.98 | +76.9 |
| **M30-1d** | 413 | +110.49 | +0.268 | 39% | 2.09 | 0.103 | 0.21 | 10.2 | 15.8 | +2.49 | +74.4 |
| M60-1d | 287 | +108.25 | +0.377 | 37% | 2.53 | 0.150 | 0.15 | 11.3 | 18.3 | +2.06 | +72.5 |

6 bp: C-4h +0.090, T-4h +0.096, C-1d +0.179, T-1d +0.221, M30 +0.270, M60
+0.380 R/trade — fees move nothing (wide stops). T-4h had 58 both-edges-touched
bars (T-1d 0); TSMOM skipped 675 (N30) / 473 (N60) flips that came while in a
position. TSMOM's average loss is −0.36R (C-1d −0.52R); likely (not checked) because at a flip the
10-bar opposite channel is often tighter than 3×ATR, so the trail cuts
losers early.

Per symbol, full history, netR (10 bp):

| | BTC | ETH | SOL | XRP | SUI | HYPE | NEAR | LINK |
|---|---|---|---|---|---|---|---|---|
| C-4h | +43.8 | +29.5 | +32.4 | +25.3 | +15.0 | +9.4 | +22.3 | +1.0 |
| T-4h | +39.3 | +44.1 | +34.5 | +69.5 | +22.1 | +12.2 | +21.4 | +5.1 |
| C-1d | +14.0 | +5.6 | +12.9 | +25.9 | +2.5 | +0.1 | +16.1 | +1.9 |
| T-1d | +16.6 | +17.0 | +20.8 | +33.7 | +4.9 | −2.4 | +26.1 | +4.2 |
| M30-1d | +13.0 | +13.1 | +9.6 | +43.3 | +3.3 | +3.8 | +21.2 | +3.3 |
| M60-1d | +19.3 | +16.7 | +9.7 | +38.1 | +0.5 | +2.0 | +12.1 | +9.8 |

XRP is the largest single contributor in every 1d arm (Nov 2024).

### 7.2 Gate windows (pooled, 10 bp; un-jittered netR (n); six 30 d buckets newest first)

| arm | 60 d | 90 d | 120 d | 30 d buckets | shipgate |
|---|---|---|---|---|---|
| C-4h | −12.0 (96) | +3.8 (140) | +30.3 (192) | −3.7 −8.3 +15.8 +26.5 −4.9 +9.7 | FAIL (60 d median −6.70) |
| T-4h vs C | −2.9 (115) | +22.9 (172) | +52.8 (234) | +1.7 −4.6 +25.8 +29.9 −17.4 +14.4 | UNDECIDED: 60 d readings −2.89 / +5.82 / +11.17 |
| C-1d | +6.9 (17) | +10.7 (29) | +14.8 (35) | +8.1 −1.2 +3.8 +4.1 −1.1 +3.4 | PASS (thin) |
| T-1d vs C | +10.8 (19) | +25.2 (32) | +30.0 (38) | +4.6 +6.2 +14.3 +4.8 −2.0 +2.6 | PASS (thin) |
| M30-1d | +12.0 (10) | +18.2 (15) | +19.3 (23) | +10.2 +1.9 +6.2 +1.1 −1.1 +2.2 | PASS (thin: 10 trades in 60 d) |
| M60-1d | +10.4 (9) | +17.8 (13) | +28.6 (20) | +8.4 +2.0 +7.4 +10.7 0 +0.1 | PASS (thin) |

Per symbol: every 1d arm is UNDECIDED on every symbol (1–5 trades per
window). T-4h vs C per symbol: XRP PASS; SUI, HYPE UNDECIDED; BTC, ETH, SOL,
NEAR, LINK FAIL (SOL: lower than C in all three windows). Not used to pick
symbols (§6.3).

### 7.3 Daily-R correlation (Pearson, pooled MTM per UTC day; full from 2022-10-15 | last 360 d)

| arm | vs C-4h | vs C-1d | vs C-book (4h+1d, on paper) |
|---|---|---|---|
| T-4h | 0.953 / 0.971 | 0.318 / 0.538 | 0.889 / 0.945 |
| T-1d | 0.417 / 0.625 | 0.971 / 0.948 | 0.690 / 0.770 |
| M30-1d | 0.300 / 0.412 | **0.812 / 0.714** | **0.537 / 0.532** |
| M60-1d | 0.364 / 0.586 | 0.842 / 0.796 | 0.600 / 0.697 |

### 7.4 Verdicts against §6 (10 bp; identical at 6 bp)

| arm | (1) gate | (2) full R/trade vs C | (3) 60 d paired diff t | verdict |
|---|---|---|---|---|
| T-4h | UNDECIDED (60 d sign flips across window ends) | +0.0886 > +0.0832 ✓ | +1.92 ✓ | **UNDECIDED** |
| T-1d | PASS | +0.2185 > +0.1763 ✓ | +2.49 ✓ | **ADOPT-candidate** |

TSMOM-30: gate PASS ✓; full netR +110.5, 60 d drop-best +74.4 ✓; N=60
neighbour gate PASS, R/trade +0.377 ✓ → **PASS**. Duplicate label vs C-book
0.537 → **overlapping** (not a duplicate by the pre-registered 0.7 line, not
diversifying). Info: gate with C-1d as the baseline also PASS.

### 7.5 Reading (not part of the criteria)

- **T-1d** is the cleanest result: more trades (+23%), higher R/trade, the
  same MDD, and the gain is spread (paired 60 d bucket t +2.49, 7 of 8 symbols
  better). It is a replacement for C-1d (corr 0.97), not an addition. The 60–
  120 d gate windows hold 19–38 trades — thin; the full-history evidence
  (553 trades, SE 0.085) carries more weight than the gate here.
- **T-4h** is better than C-4h on every full-history measure (R/trade +0.005,
  ≈ +70 R more from 30% more trades, bucket t +1.92) but the improvement per
  trade is a fraction of one SE, and the 60 d window's sign depends on the run
  date. Not decided. Note the paper baseline C-4h itself FAILS this window set
  (60 d −6.7 R median); the original 120/240/360 d gate it passed is a
  different window set.
- **TSMOM-30** has the best risk profile of the 1d arms (MDD 10 R closed /
  16 R MTM vs 17 / 25 for C-1d, R/trade +0.27) and passes, but its daily R is
  0.81 correlated with the 1d Donchian leg already on paper. Against the whole
  paper book it is 0.54 ("overlapping") only because the 4h leg dominates the
  book's variance. As an ADDITION it mostly doubles the 1d exposure; as a
  REPLACEMENT for C-1d it is a reasonable candidate (but that comparison was
  not pre-registered as a decision and is info only: gate vs C-1d PASS).
- Thin-window caveat for every 1d result: 9–38 trades per gate window.

### 7.6 Hypotheses NOT tested (would need their own pre-registration)

- T-1d replacing C-1d on paper, and TSMOM-30 replacing (not adding to) C-1d,
  as a head-to-head A/B with a pre-registered rule.
- A 1d book of T-1d + M30 with a combined per-symbol position cap (they fire
  on the same moves).
- Why TSMOM's average loss is smaller (entry location vs the 10-bar trail):
  a TSMOM arm with the trail starting only once it passes the initial stop.
