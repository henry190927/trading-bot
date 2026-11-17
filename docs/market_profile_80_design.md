# Market Profile "80% rule" — value-area re-entry + acceptance (design, pre-registered)

Status: DESIGN + BACKTEST ONLY. Not in `EvalAutoTrigger` / `KnownStrategies`,
not in `autotrade.json`. Written 2026-11-17 BEFORE any result was looked at;
committed on its own before `cmd/mp80bt` was run on real data.

## 1. Why

The Market Profile "80% rule" (Dalton): when the session opens outside the
prior session's value area and then re-enters it and is **accepted** there,
the odds favour a traverse to the opposite edge of the value area. The user's
"站穩" (holds after reclaiming) maps onto the acceptance test: two
consecutive 30m closes back inside the VA.

## 2. What already failed, and how this differs

| | opensab | sweepbt AMD | HVN proximity votes | lvlbt (11-17) | **mp80 (this)** |
|---|---|---|---|---|---|
| level | opens (filter) | Asia range H/L | HVN distance | session opens/H/L | **prior-day VAH/VAL (volume-derived)** |
| decision | engine fire | sweep | distance (no event) | 15m/1h close + 1m bar | **open outside VA, then 30m×2 / 1h×1 closes inside** |
| stop | 1h | 1h | — | 1m extreme (~0.35%) | **30m extreme since re-entry, ≥ 0.5%** |
| target | 2R | 2R | — | 2R | **opposite VA edge (structural), RR ≥ 1** |
| result | worse 3/3 | FAIL 0/3 | rejected | REJECTED 0/8 | ? |

New here: the level is volume-derived (not a clock price), the trigger is an
EVENT (re-entry + acceptance), the stop is floored at 0.5% so fee_R ≤ 0.20R
at 10bp, and the target is the structural opposite edge, not a fixed R.

Known headwinds, stated now: clock/day-anchored levels have failed 5×+ here;
VA proximity (HVN distance) did not score; one setup per symbol-day caps the
sample (≈ 189 days × 2 symbols, expected a few dozen trades per window), so
thin windows are likely and the gate's trade floors may decide it.

## 3. Exact rules

All times UTC. Everything is derived from ONE BingX 1m series per symbol.
HTF bars (30m, 1h) are UTC-aligned aggregates of closed 1m bars; a bucket
is used only when complete (its last 1m bar is the bucket's final minute).

### 3.1 Value area (prior UTC day)

- Input: the 1m bars with OpenTime in [D−1 00:00, D 00:00). The day is
  usable only if it has ≥ 1380 of its 1440 bars.
- Volume-at-price: `indicator.BuildVolumeProfile(bars, 100, 1)` with
  `BodyWeight = 0` — each 1m bar's volume is **distributed uniformly across
  its [low, high] range**, into 100 equal-width bins spanning the day's
  low→high. POC = midpoint of the max-volume bin.
- VA = the standard expansion from the POC bin, adding the larger neighbour
  each step, until ≥ 70% of the day's volume is covered. VAL = low edge of
  the lowest included bin, VAH = high edge of the highest. Record VAH, VAL,
  POC.

### 3.2 Setup

- O = open of the 00:00 UTC 1m bar of day D (5-min tolerance, as lvlbt).
- O > VAH → **short** setup (re-entry through VAH, target VAL).
  O < VAL → **long** setup (re-entry through VAL, target VAH).
  VAL ≤ O ≤ VAH → no setup that day.
- Inside = VAL < close < VAH (strict).
- Walk day D's complete HTF bars in order:
  - If a bar closes **beyond the opposite edge** (short setup: close < VAL;
    long: close > VAH) before acceptance → the day is void (the traverse
    already happened without an accepted re-entry).
  - **Acceptance**, variant A: two consecutive 30m bars both close inside;
    variant B: one 1h bar closes inside. The first acceptance of the day
    is the only one considered.
  - The accepting bar(s) must close by D+1 00:00 (no carry into the next
    day).
- **Re-entry bar** = the first bar of the accepting sequence (A: the first
  of the two 30m bars; B: the 1h bar).

### 3.3 Entry / stop / target

- Entry = close of the last accepting bar, **market** (fire time = that
  bar's close).
- Stop: short = max(VAH, highest 1m high from the re-entry bar's open
  through the entry) + 0.25 × ATR14(30m); long = min(VAL, lowest low) −
  0.25 × ATR14(30m). ATR14(30m) is Wilder ATR over the complete 30m
  aggregates whose close ≤ fire time (the last one is the entry bar or the
  30m bar that closes with it).
- Target = the opposite VA edge (short → VAL, long → VAH).
- **Skip** if stop distance < 0.5% of entry (fee_R would be > 0.20R at
  10bp), or reward/risk = |target − entry| / |entry − stop| < 1.0.
- At most one trade per symbol per day (a single setup per day by
  construction). **No timeout, no end-of-day exit** — the position runs
  to stop or target, as the paper executor (`EvaluateFire`, expiry 0;
  same-bar ambiguity → stop). One position per symbol at a time
  (`DedupFires`, cooldown 0): a setup while the previous trade is still
  open is dropped.

## 4. Variants (fixed now, at most 2)

- **A `30mx2`** — two consecutive 30m closes inside (the primary, the
  user's 站穩).
- **B `1hx1`** — one 1h close inside.

Nothing else (bins, 70%, buffer, 0.5%, RR 1.0) has a flag or is varied.

## 5. Falsification criteria (decided before results)

- Symbols BTC, ETH (`feedback_strategy_scope`). BingX 1m, ~189 days.
- Verdict at **10bp** round trip. 6bp and 0bp printed for reference only.
- Windows: 60/90/120d nested + six disjoint 30d buckets (end-offset
  0,30,…,150) through `shipgate.Default()` (absolute floor, per-window,
  bucket breadth, min trades).
- A variant is a **candidate** only if the POOL (BTC+ETH) PASSes at 10bp
  AND the other variant has 120d pooled R/trade > 0 at 10bp. A lone PASS is
  reported, not shipped. UNDECIDED (thin sample) is not a PASS.
- No parameter in §3 is tuned after results. A candidate goes to paper only,
  under a new strategy name with its own executor design; never live from
  this test.
- Printed with every result: median stop % and the 10bp fee in R at the
  median stop (fee_R = 0.10% / stop%), so the friction is visible.

## 6. Tests (written before the real-data run)

- Hand-traced deterministic scenario: a synthetic prior day with a known
  VA, an open above VAH, two 30m closes inside; expected entry / stop /
  target traced by hand in the test comment before the assertion values
  were written.
- No-lookahead: replacing every 1m bar after a cut with an unrelated,
  shifted series must leave every fire whose time ≤ the cut unchanged
  (both variants), and the test must see ≥ 1 fire before the cut.

## 7. Result 2026-11-17 — REJECTED (neither variant passes on the pool at 10bp)

`cmd/mp80bt`, BingX 1m 2026-05-12 → 11-17 (BTC and ETH 272k bars each),
dedup one position per symbol, no timeout. Fires over the whole ~189 days:
30m×2 BTC 14 / ETH 22; 1h×1 BTC 18 / ETH 26 (≈ 1 setup per symbol-week
survives the open-outside + acceptance + stop ≥ 0.5% + RR ≥ 1 filters).

120d, 10bp (verdict) vs 0bp (gross):

| variant | sym | n | WR | R/t @10bp | R/t gross | shipgate @10bp |
|---|---|---|---|---|---|---|
| 30m×2 | BTC | 11 | 27.3% | +0.106 | +0.243 | PASS (n=11, SE 0.81) |
| 30m×2 | ETH | 16 | 25.0% | −0.124 | +0.007 | FAIL |
| 30m×2 | **POOL** | 27 | 25.9% | −0.031 | +0.103 | **FAIL** (120d −0.83R) |
| 1h×1 | BTC | 15 | 20.0% | −0.422 | −0.287 | FAIL |
| 1h×1 | ETH | 17 | 23.5% | −0.039 | +0.092 | FAIL |
| 1h×1 | **POOL** | 32 | 21.9% | −0.218 | −0.086 | **FAIL** (90d, 120d < 0) |

Nested pool netR @10bp (60/90/120d): 30m×2 +2.0 / +1.6 / −0.8; 1h×1
+4.6 / −2.4 / −7.0. Disjoint 30d buckets (pool, off 0…150): 30m×2 +7.6
−5.5 −0.5 −2.4 +4.8 +0.6 (n 9/10/6/2/4/4); 1h×1 +7.2 −2.6 −7.0 −4.6 +6.8
−1.4. Both are carried by the most recent 30 days.

Friction: median stop 0.77% (30m×2) / 0.78% (1h×1) → 10bp costs 0.13R
per trade (mean charged 0.134R). The 0.5% floor did its job — fees are
half of lvlbt's 0.28R — but there is little gross edge to protect: 30m×2
pooled is +0.10R/t gross (SE 0.43), 1h×1 is negative gross.

The "80%" does not show up: the opposite VA edge is reached before the
stop in 22–26% of accepted re-entries (break-even at the median RR ≈
1.5–2 is ~35–40%). At 6bp 30m×2 pool PASSes (+0.023R/t, 27 trades) — a
reference fee, not the verdict, and a mean 1/19th of its SE.

§5: 30m×2 pool FAILs at the verdict fee and the other variant's 120d
pooled R/t is −0.218 → no candidate. BTC 30m×2's PASS rests on 11 trades
(3 wins) with buckets of 0–2 trades; it is not evidence. Nothing in §3 was
changed after the run. Joins opensab / AMD / lvlbt / HVN proximity:
day-anchored levels — volume-derived ones included — do not carry an edge
here once a realistic stop and fees are charged.
