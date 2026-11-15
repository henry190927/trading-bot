# HTF trend follower (Donchian breakout + channel trail) — design + pre-registration

Written 2026-11-15 BEFORE any result was computed. Results go in §6 and must
not change §2–§5.

## 1. Why

The autotrade book is almost entirely counter-trend fades (range-edge,
sweep-reject, htf-snr, MR engine, vp-node). In the November 2026 one-way drop
(BTC 80.5k → 66k) they kept buying dips, and the fee + live-parity re-gate
disabled 9 rules. Fees are a big part of it: a tight 1h stop turns a 6 bp
round trip into 0.06–0.20R per trade.

We want the opposite payoff: a LOW-frequency, WIDE-stop, time-series trend
follower that should make money in exactly the regime where the fade book
bleeds. Small win rate, a few large winners, no fixed target.

How it differs from what already failed:

| prior result | what it was | how this differs |
|---|---|---|
| decisive-breakout (2026-08-28) | 1h breakout, fixed 2R target | 4h/1d, no target, trailing exit |
| vpbt arm B | 1h breakout chase to the next volume node | HTF, exit is a trailing channel, not a node |
| 15m/30m engine | lower TF is worse, monotonic | 4h and 1d only |

A fixed 2R target removes the right tail, and the right tail is the whole
edge of a trend follower. That is the hypothesis being tested here: the
earlier breakout failures were a failure of the EXIT and the TF, not of the
breakout as an event.

## 2. Rules (exact)

All evaluation is on CLOSED bars. Bar i is the signal bar; it never sees
bar i+1.

- **Channel.** `upper(i) = max(High[i-N .. i-1])`, `lower(i) = min(Low[i-N .. i-1])`
  — the N bars BEFORE the signal bar, so the bar cannot break its own range.
- **Long entry.** `Close[i] > upper(i)` → market long, filled at `Close[i]`.
  **Short entry.** `Close[i] < lower(i)` → market short at `Close[i]`.
  Both sides always on (symmetric; no side filter, no trend filter).
- **Initial stop.** `entry ∓ K × ATR(20)[i]` (Wilder ATR, bar i included —
  it is closed). **1R = K × ATR(20)[i]** and every result is in R of this
  initial risk, including the trail exit.
- **Trail.** After each closed bar j ≥ i, the stop for bar j+1 is
  `max(stop, min(Low[j-M+1 .. j]))` for a long (mirror for a short): the
  opposite M-bar Donchian channel, ratcheted — it never loosens. The initial
  stop holds until the channel passes it.
- **Exit.** On bar j > i, a long exits if `Low[j] ≤ stop(j)` at `stop(j)`, or at
  `Open[j]` when the bar opens through the stop (gap — XAG weekends). Short
  mirrors. The stop for bar j is fixed BEFORE bar j opens, so the bar that
  would raise the trail cannot also be the bar it is tested on. There is no
  favourable event (no TP), so intrabar ordering cannot flatter the result.
- **One position per symbol.** While a position is open, further signals are
  ignored. After an exit on bar j, the next entry needs a signal on bar ≥ j+1.
- **Fees.** Round trip, `fee_R = feeBps/10000 × entry / |entry − initialStop|`,
  exactly `autotrade.WithFee`. A trade still open at the last bar is marked to
  the last close and pays the fee too.
- **Warm-up.** No signal before bar max(N, 20) + 1 of a symbol's history.

## 3. Pre-registered variants (no others will be run for the verdict)

| variant | TF | N (entry) | M (trail) | K (initial stop, ×ATR20) |
|---|---|---|---|---|
| **A (primary)** | 4h | 20 | 10 | 3.0 |
| B | 1d | 20 | 10 | 3.0 |

Symbols: BTC ETH SOL XRP SUI HYPE NEAR LINK XAG. Fees: 6 bp (repo convention)
and **10 bp (the verdict fee**: a market entry plus a stop-market exit are both
taker, 5 bp each on BingX).

## 4. Harness

`cmd/trendbt` runs ONE continuous simulation per symbol over all available
history (BingX public klines, paged back to the listing date) and attributes
each trade to the window that contains its ENTRY bar. So disjoint buckets
partition the trade list exactly, nested windows are prefixes of it, and a
trade that runs past a window's end is scored to its real exit (or marked to
the last close if still open — flagged as open). This is also what a live
daemon would have done: it never "restarts" at a window boundary.

Reported: n, netR, R/trade, WR, avg win R, avg loss R, payoff SD and SE =
SD/√n, max drawdown in R (closed-trade equity in exit order), trades/month,
implied max leverage from the stop distance via `autotrade.MaxLevForStop`,
and the mark-to-market R earned inside two calendar periods: the Sep–Oct 2026
uptrend (2026-09-01 → 2026-11-01) and the November drop (2026-11-01 → now).
Pooled across symbols is the PRIMARY view (trend following is a portfolio
strategy); per-symbol is secondary and will not be used to pick symbols.

## 5. Falsification criteria (decided before results)

Variant A is **rejected** if ANY of these holds at 10 bp, pooled:

1. netR ≤ 0 in any of the nested 120 / 240 / 360 d windows.
2. Disjoint 60 d buckets: the total goes ≤ 0 when the single best bucket is
   dropped (the edge is one period, not a strategy).
3. The November 2026 drop period earns ≤ 0 R mark-to-market — the strategy
   exists to pay when the fades bleed; if it does not, it fails its purpose
   even if it is profitable elsewhere.
4. `cmd/gate` returns FAIL on the nested windows + buckets.

Variant A is **paper-ship eligible** if none of 1–4 hold. `cmd/gate`
UNDECIDED on sample size is acceptable for a PAPER forward test (not live),
provided 1–3 pass; the report must state the n the gate would need.

Variant B is judged on the same criteria; it is expected to be thin
(≈ 1 trade per symbol per month or fewer) and may only be UNDECIDED.

Things that will NOT be done regardless of results: tune N/M/K, add a trend
filter, drop symbols that lost, switch to an ATR trail. Any such idea is
recorded as a hypothesis in §6 for a separate, pre-registered run.

## 6. Results (run 2026-11-15, commit of §1–5 = 01ceab0)

History: BingX 4h pages back only to **2022-10-07** (≈9000 bars) for every
symbol; 1d reaches 2021-05. SUI from 2023-05, HYPE 2024-12, **XAG only from
2026-02** (9 months). Reproduce: `trendbt -tf 4h -fee-bps 10 -report`.

### Variant A — 4h N20/M10/K3, pooled, 10 bp (verdict fee)

| window | n | netR | R/trade | WR | avgW / avgL | SD | SE | MDD R | trd/mo |
|---|---|---|---|---|---|---|---|---|---|
| 120d | 220 | +31.71 | +0.144 | 30% | +1.78 / −0.56 | 1.86 | 0.125 | 33.1 | 55.8 |
| 240d | 439 | +23.54 | +0.054 | 31% | +1.37 / −0.55 | 1.50 | 0.072 | 33.1 | 55.7 |
| 360d | 639 | +19.34 | +0.030 | 31% | +1.33 / −0.54 | 1.39 | 0.055 | 46.5 | 54.0 |
| all (25 mo) | 2205 | +181.93 | +0.083 | 34% | +1.20 / −0.50 | 1.34 | 0.029 | 46.5 | 44.8 |

6 bp: +33.57 / +27.12 / +24.18 / +197.03 — the fee costs ≈0.007 R/trade
(median stop 5–6.5 % of price), i.e. the wide-stop design did remove fee drag.

- Disjoint 60 d buckets: 25, 17 positive, total +181.93, without the best
  +145.92 (spread). Bucket-level t = 2.74 (each period one observation —
  the honest test, since cross-symbol trades are correlated).
- **But the latest year is weak:** 360d R/trade +0.030, 95 % CI
  [−0.077, +0.138]; its six buckets total +19.34 and **−8.80 without the
  best** (Jul–Sep 2026 +28.1). The full-history PASS rests on 2023–2024.
- Calendar, mark-to-market: **Sep–Oct 2026 −19.59 R**, **Nov 2026 +12.03 R**
  (7 of 9 symbols positive).
- Per-symbol, all history: 8 of 9 positive (LINK +1.7 flat, XAG −3.1 on 9 months).
- Max concurrent positions: 9 of 9 (the whole book fires together).
- Implied leverage (MaxLevForStop): median 7–9×, worst 1–2×.
- `cmd/gate` (nested 120/240/360 with ±1–2 d jitter + 25 buckets): **PASS**.

### Variant B — 1d N20/M10/K3, pooled, 10 bp

| window | n | netR | R/trade | WR | SE | MDD R | trd/mo |
|---|---|---|---|---|---|---|---|
| 120d | 39 | +17.37 | +0.445 | 54% | 0.211 | 4.7 | 9.9 |
| 240d | 77 | +15.22 | +0.198 | 40% | 0.126 | 8.3 | 9.8 |
| 360d | 108 | +18.52 | +0.171 | 42% | 0.100 | 11.3 | 9.1 |
| all (5.5 y) | 459 | +78.04 | +0.170 | 39% | 0.087 | 18.1 | 7.0 |

Buckets 33 (18 positive), total +78.0, without best +37.8 (XRP Nov 2024
alone +30.9); bucket t = 1.51. Sep–Oct −1.96 R, Nov +9.59 R. Median stop
14–18 % → 2–3× max leverage; 4 trades in 2021–22 had stops > 50 % and would
be blocked by the liq guard even at 1×. Gate: PASS.

### Verdict against §5

A at 10 bp: (1) all nested windows > 0 ✓ (2) drop-best total +145.9 ✓
(3) Nov +12.0 R ✓ (4) gate PASS ✓ → **paper-ship eligible**. B: same, thinner.

Caveats that the criteria do not capture: the last 12 months alone are
concentrated in one bucket; the strategy lost −19.6 R in Sep–Oct (it bleeds
in the regime where fades earn — which is the hedge working, but it is not
free); MDD 46 R with 9 correlated concurrent positions means the book's
1.5 %/trade sizing would be a ~70 % drawdown.

### Hypotheses NOT tested (separate pre-registration needed)

- M=20 trail on 4h: avg win is only +1.2–1.8 R, so the 10-bar trail (40 h)
  exits trends early; the 1d variant (same bars, 6× the time) has the
  better R/trade.
- A portfolio cap on same-direction concurrent positions, or vol-scaled risk.
