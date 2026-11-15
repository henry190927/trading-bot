# pivot-pullback — trend-following entry into the 4h 樞紐區 (design, pre-registered)

Status: DESIGN + BACKTEST ONLY. Not in `EvalAutoTrigger` / `KnownStrategies`,
not in `autotrade.json`. Written 2026-11-15 BEFORE any result was looked at.

## 1. Why

The autotrade book is almost all counter-trend fades (sweep-reject,
range-edge, engine MR, htf-snr). In the November one-way drop they kept
buying dips, and the fee + live-parity re-gate disabled nine of them. The book
needs a rule whose losing regime is NOT the fades' losing regime: something
that sells the bounce in a downtrend and buys the dip in an uptrend.

The user's own discretionary version is `project_pivot_zone_fade_strategy`:
in a confirmed trend, the retrace into the 樞紐區 (0.5–0.705) is a
continuation entry in the trend direction; trigger = rejection at the zone +
structure resumes; invalidation = the leg origin; and the crux is the
**higher-TF** regime read.

## 2. What already failed, and how this differs

| | struct-momentum (`signal/strategy_momentum.go`) | confirmbt CONFIRM arm (2026-09-04) | **pivot-pullback (this)** |
|---|---|---|---|
| trend | 1h HH-HL / LH-LL | none (leg direction only) | **4h HH-HL / LH-LL** (closed 4h bars only) |
| zone | 1h leg 樞紐區 | 1h leg 樞紐區 | **4h leg 樞紐區** |
| entry | any 1h close inside the zone (touch / distance) | 1h close back out of the zone | **1h event**: after a dip into the 4h zone, a 1h close breaks the pullback's last 1h swing high (micro-BOS, structure resumes) |
| extra gate | EMA20/50 alignment | — | — (no indicator) |
| stop | 1h leg origin | 1h leg origin | 4h leg origin (= /chart 失效) |
| target | fixed 1R / 2R (backtest) | 1h zone 1:1 | 4h zone 1:1 measured move (= /chart 目標) |
| result | PASS BTC 1h only, 1/10 | negative everywhere (best −0.063 R/t gross) | ? |

The two differences that matter: (a) the regime and the zone come from 4h,
which is the user's "ask the higher TF" crux, and (b) the entry is an event
(a break of the pullback's own structure) rather than a distance to a level
(`feedback_proximity_votes_dont_score`, `feedback_entry_event_not_guess`).
Fib stays a 0.5–0.705 zone (`feedback_fib_zone_minimal`); no lines added.

## 3. Exact rules (base arm "P0")

Inputs at closed 1h bar `i` (evaluated at its close):

- `h` = the 4h bars whose `CloseTime <= cs[i].OpenTime`, last 200 of them.
  (Stricter than "closed by the 1h close" — the daemon wrapper applies the
  same filter, so live and backtest see the same 4h bars.)
- `st = AnalyzeStructure(h, 2)`; the leg-high/leg-low bars are the last
  swing top / bottom of `FindSwingPoints(h, 2, 0)` (the same pivots
  `AnalyzeStructure` uses; swings confirm 2 bars late by construction).

LONG (short mirrors everything):

1. **Regime**: `st.Trend == StructUptrend` and `st.Zone != nil` and
   `st.Zone.Dir == StructUptrend` (current 4h leg is an intact up-leg, no
   4h CHoCH-down).
2. **Pullback segment**: 1h bars from the 4h bar that printed the leg high
   (its `OpenTime`) up to `i`, at most the last 96 bars. Need ≥ 5 bars.
3. **Structure intact on 1h**: no close in the segment at or below
   `Zone.Invalidate`.
4. **Pullback lower high** `LH`: the last 1h swing top (strength 2, found
   inside the segment, so it confirms 2 bars late) with `LH.Price < LegHigh`.
   None → no trade.
5. **Dip into the zone after LH**: `min(Low) over (LH, i] <= Zone.Hi`
   (Zone.Hi = the 0.5 level for an up-leg).
6. **Event**: `cs[i].Close > LH.Price` and `cs[i-1].Close <= LH.Price`
   (fresh cross on this bar), and `cs[i].Close < LegHigh`.
7. **Bracket**: market entry at `cs[i].Close`; stop = `Zone.Invalidate`
   (4h leg low); TP = `Zone.Target` (4h leg high + span). Skip if reward:risk
   < 1.5 (only bites when the event fires high above the zone — a chase).

No time stop: like every rule harness, `EvaluateFire` holds to TP or stop;
trades still open at the data end are reported separately (0 R).

Scoring: `autotrade.EvaluateFire` → `autotrade.WithFee` →
`autotrade.DedupFires(expiry 6, cooldown 6 bars)` = one position per
(symbol, rule) at a time, like the daemon. Fires are generated only on bars
inside the window; resolution may use bars after the window end (a trade
belongs to the bucket it was opened in).

## 4. Pre-registered variants (at most two, fixed now)

- **V1 "reclaim"** — step 4–6 replaced by: `cs[i].Close > Zone.Hi` and
  `cs[i-1].Close <= Zone.Hi` (the first 1h close back above the zone's near
  edge, i.e. confirmbt's CONFIRM, but on the 4h zone with the 4h trend gate).
  Isolates whether the micro-BOS event matters vs. the simpler reclaim.
- **V2 "tight"** — P0 entry, but stop = the pullback low `min(Low) over
  (LH, i]` (the 1h structure that the event just confirmed), same TP.
  Tests "structure-invalidation stop" vs leg-origin stop. Prior from the repo
  (shadowbt, SM stop-buffer A/B) says stops closer to entry tend to look
  better in R/trade by mechanism; the fee at 10 bps is the honest check.

Fixed parameters (all arms): HTF 4h, HTF lookback 200, zone strength 2,
1h micro strength 2, segment cap 96 bars, MinR 1.5, base TF 1h. No 2h run:
the design is 4h-regime / 1h-trigger by construction; 2h would be an
unregistered third variant.

## 5. Falsification criteria (decided before results)

Run: BTC ETH SOL XRP SUI HYPE NEAR LINK XAG, 1h, fee 6 and 10 bps, nested
60/90/120d + six disjoint 30d buckets (end-offset 0..150), `-warmup` on.

1. **Book-level reject** if the POOLED P0 arm does not PASS `shipgate`
   (absolute, buckets supplied) at 6 bps, or has netR ≤ 0 in any nested
   window at 10 bps.
2. **Concentration**: if dropping the best disjoint bucket takes the bucket
   total to ≤ 0 (gate: Concentrated), it is not robust — in particular if
   the result is only the Nov bucket (end-offset 0), it is one regime, not a
   strategy.
3. **Per symbol**: a symbol is paper-ship-eligible only on a PASS at 6 bps
   AND positive netR in all three nested windows at 10 bps. With 3 arms × 9
   symbols = 27 tests, 1–2 spurious per-symbol passes are expected by chance:
   a per-symbol PASS with a pooled FAIL is a hypothesis, not a ship.
4. **Variants** replace P0 only if they PASS pooled AND beat P0 in every
   nested window at 6 bps. Never "best of three".
5. **Signal vs book**: no-dedup R/trade must have the same sign as the
   deduped R/trade; otherwise the dedup ordering is creating the result.
6. Anything noticed after the results (a different TF, lookback, target,
   gate) is written down as a hypothesis and NOT run in this round.
