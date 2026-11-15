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

## 6. Results

(filled in after the runs; see below)
