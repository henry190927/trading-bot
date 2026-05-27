# Confluence Review — Patches & A/B Plan

**Context.** Multi-agent review of the confluence engine on 2026-05-27 (BTC/ETH/XAU/XAG, 1h, mean-rev biased). This doc captures every patch and how to verify it so the implementer can pick up cold.

**Scope of this doc (read top-to-bottom; each section is independently shippable):**
1. **Must-fix correctness patches** — Patches 1-3. Real bugs. Ship before any A/B work.
2. **Engine-level A/B patches** — A/B-1 through A/B-6. Strategy tunings; each has a pre-committed decision rule.
3. **Validator A/B patches** — V-1 through V-5. Rubric tunings; replay-based A/B (not backtest).
4. **Backtest harness patches** — B-1 through B-3. Realism + tooling improvements that improve A/B measurement.

**Reading order for the implementer.** Sections are ordered by dependency: must-fix → engine A/B → validator A/B → backtest. But B-1 and B-2 should land *before* re-running any earlier A/B (see "Backtest harness patches" preamble). The cross-patch prerequisites recap at the bottom names every such dependency.

**Diagnosis correction from the original review.** Reviewers initially flagged "backtest lookahead bias" via `slice := candles[:i+1]`. That claim was wrong — the backtest operates on historical fully-closed candles and the serve daemon waits for `nextBoundary + 2s` before scanning, so bar `i` is closed in both paths. **The real lookahead is in the live BingX fetch**: `client.Klines()` returns the still-forming current bar, and `engine.Evaluate()` treats it as `bar[last]`. Backtest and live therefore diverge — e.g., the range-expansion vote can fire in backtest but is structurally suppressed in live (a 2-second-old bar has near-zero range/volume).

---

## Must-fix correctness — order of application

This order applies to **Patches 1-3 only** (the must-fix correctness section). Each subsequent major section (Engine A/B, Validator A/B, Backtest) has its own ordering rules in its preamble.

1. **Patch 3 (OI sign)** — pure correctness, no A/B gate. Ship first.
2. **Patch 1 (forming-bar trim)** — correctness, changes live signal timing by ~one bar. Observe live for 1–2 days after merge.
3. **Patch 2 (RSI-neutral gate on range-expansion)** — strategy change. Run 60/90/120d A/B on all 4 symbols before merging. Per the user's gate, also gather a few live observations before declaring robust.

After all three land, **re-run the full backtest as the new baseline.** Engine-level A/B candidates from the wider review (factor independence, regime gate, funding percentile, etc.) should be measured against the corrected reference.

---

## Patch 1 — Drop the still-forming candle from BingX Klines fetches

**Why.** BingX (like most exchanges) includes the current forming bar in `/openApi/swap/v3/quote/klines` responses. The engine assumes every candle it sees is closed: range-expansion vote reads `bar.High - bar.Low` and `bar.Volume` on `last`; MACD cross compares `[last]` vs `[last-1]`; sweep detection requires `SweepIdx == last`. With a partial bar at the tail, these mechanics fire (or fail to fire) on incomplete data, and live behavior diverges from backtest.

**Where.** Apply at the BingX client layer so every caller benefits (serve daemon, `/analyze`, `/validate`, anywhere else that calls `Klines` or `KlinesRange`).

### Implementation

Extract a small helper for testability, then call it from both fetch paths.

**New helper** (e.g., add to `bingx/client.go` near the bottom, or to a new `bingx/candles.go`):

```go
// dropForming returns candles minus a trailing still-forming bar.
// A candle is "forming" if its CloseTime is strictly after `now`; at the
// exact close instant we keep it (the bar has just closed).
// Backtest paths don't call this — they operate on historical closed bars.
func dropForming(candles []market.Candle, now time.Time) []market.Candle {
    if len(candles) == 0 {
        return candles
    }
    if candles[len(candles)-1].CloseTime.After(now) {
        return candles[:len(candles)-1]
    }
    return candles
}
```

**Call site 1 — `bingx/client.go::Klines`**, just before `return out, nil`:

```go
out = dropForming(out, time.Now())
return out, nil
```

**Call site 2 — `bingx/klines_range.go::KlinesRange`**, just before the final `return`:

```go
out = dropForming(out, time.Now())
return out, nil
```

(`KlinesRange` is used for historical fetches with an explicit `end` time; in nearly all cases the last bar will already be closed and the helper is a no-op. We still apply it defensively in case `end` is set to `time.Now()` or beyond.)

### Test cases (new file: `bingx/candles_test.go`)

```go
package bingx

import (
    "testing"
    "time"

    "myFirstGo/trading-bot/market"
)

func TestDropForming(t *testing.T) {
    now := time.Date(2026, 5, 28, 19, 0, 5, 0, time.UTC) // 5s past 19:00 UTC

    // Helper: build a candle with given CloseTime.
    mk := func(ct time.Time) market.Candle {
        return market.Candle{OpenTime: ct.Add(-time.Hour), CloseTime: ct}
    }

    tests := []struct {
        name    string
        in      []market.Candle
        wantLen int
        wantLastClose time.Time // zero value means "don't check"
    }{
        {
            name:    "empty slice returns empty",
            in:      nil,
            wantLen: 0,
        },
        {
            name: "single forming candle is trimmed to empty",
            in: []market.Candle{
                mk(now.Add(55 * time.Minute)), // closes in the future
            },
            wantLen: 0,
        },
        {
            name: "single closed candle kept",
            in: []market.Candle{
                mk(now.Add(-1 * time.Second)),
            },
            wantLen: 1,
            wantLastClose: now.Add(-1 * time.Second),
        },
        {
            name: "trailing forming bar dropped, prior bars kept",
            in: []market.Candle{
                mk(now.Add(-2 * time.Hour)),       // closed
                mk(now.Add(-1 * time.Hour)),       // closed
                mk(now.Add(55 * time.Minute)),     // forming
            },
            wantLen: 2,
            wantLastClose: now.Add(-1 * time.Hour),
        },
        {
            name: "all bars closed → no-op",
            in: []market.Candle{
                mk(now.Add(-3 * time.Hour)),
                mk(now.Add(-2 * time.Hour)),
                mk(now.Add(-1 * time.Hour)),
            },
            wantLen: 3,
            wantLastClose: now.Add(-1 * time.Hour),
        },
        {
            name: "bar closing exactly at `now` is kept (boundary)",
            in: []market.Candle{
                mk(now.Add(-1 * time.Hour)),
                mk(now), // CloseTime == now → After(now) is false → keep
            },
            wantLen: 2,
            wantLastClose: now,
        },
        {
            name: "bar closing one nanosecond past `now` is dropped",
            in: []market.Candle{
                mk(now.Add(-1 * time.Hour)),
                mk(now.Add(1 * time.Nanosecond)),
            },
            wantLen: 1,
            wantLastClose: now.Add(-1 * time.Hour),
        },
        {
            name: "two trailing forming bars: only one trimmed (defensive, malformed input)",
            // Real APIs never return >1 forming bar at the head, but the helper
            // intentionally trims only the last bar so an upstream bug doesn't
            // silently eat real data.
            in: []market.Candle{
                mk(now.Add(-1 * time.Hour)),
                mk(now.Add(30 * time.Minute)),
                mk(now.Add(90 * time.Minute)),
            },
            wantLen: 2,
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            got := dropForming(tc.in, now)
            if len(got) != tc.wantLen {
                t.Fatalf("len = %d, want %d (got: %+v)", len(got), tc.wantLen, got)
            }
            if !tc.wantLastClose.IsZero() && len(got) > 0 {
                last := got[len(got)-1].CloseTime
                if !last.Equal(tc.wantLastClose) {
                    t.Errorf("last CloseTime = %v, want %v", last, tc.wantLastClose)
                }
            }
        })
    }
}

// TestDropForming_DoesNotMutate guards against the helper aliasing the input.
// (Trimming via slice resliced is fine; mutating in place would be a bug.)
func TestDropForming_DoesNotMutate(t *testing.T) {
    now := time.Date(2026, 5, 28, 19, 0, 0, 0, time.UTC)
    in := []market.Candle{
        {CloseTime: now.Add(-time.Hour)},
        {CloseTime: now.Add(time.Hour)}, // forming
    }
    origLen := len(in)
    origLast := in[len(in)-1].CloseTime
    _ = dropForming(in, now)
    if len(in) != origLen || !in[origLen-1].CloseTime.Equal(origLast) {
        t.Errorf("dropForming mutated input slice")
    }
}
```

### Manual verification after merge

1. **Live daemon, immediately after a 1h boundary.** Run `serve` and confirm the first scan at HH:00:02 logs a `bar[last].CloseTime` equal to the just-closed boundary (e.g., 19:00:00 UTC), not the new bar (which would close at 20:00).
2. **Backtest unchanged.** Re-run a baseline backtest and confirm headline metrics (n signals, win rate, totalR) are byte-identical to pre-patch — the harness uses historical closed candles and should not be affected.
3. **Range-expansion vote can now fire live.** On a symbol that printed a range-expansion bar on the prior 1h close, the post-patch live scan should include the corresponding `Reasons` entry (which it would not have before, given the partial bar's tiny range/volume).

---

## Patch 2 — Range-expansion bar only votes when RSI is neutral

**Why.** The range-expansion vote was promoted from display-only to a +1 vote on 2026-05-27 to capture the XAG miss. It's a momentum/trend factor and conflicts with the mean-rev core when RSI is at extremes — risk of self-cancellation (RSI < 30 votes long, simultaneous bearish range bar votes short → tie → Flat, but one side was right). Gating the vote to neutral RSI keeps the factor useful in choppy regimes (where it adds genuine information) and prevents conflict at extremes (where mean-rev already drives the decision).

**File.** `signal/engine.go:199-221`. Replace the existing block:

```go
// Range expansion + volume confirmation. Voting symmetric: bullish bar
// votes long, bearish bar votes short — BUT only when RSI is in the
// neutral band. At RSI extremes, mean-rev oscillators already drive the
// decision and a counter-trend range bar would tie/cancel the score.
// Backtest A/B (2026-05-27 baseline) will decide whether the gate stays.
if last >= 20 && rsi[last] >= 30 && rsi[last] <= 70 {
    bar := in.Candles[last]
    barRange := bar.High - bar.Low
    atrSeries := indicator.ATR(in.Candles, 14)
    a14 := atrSeries[last]
    var avgVol float64
    for i := last - 19; i <= last; i++ {
        avgVol += in.Candles[i].Volume
    }
    avgVol /= 20.0
    if a14 > 0 && avgVol > 0 && barRange > 1.5*a14 && bar.Volume > 1.5*avgVol {
        switch {
        case bar.Close > bar.Open:
            bullVotes++
            sig.Reasons = append(sig.Reasons,
                fmt.Sprintf("Range expansion + volume bullish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
        case bar.Close < bar.Open:
            bearVotes++
            sig.Reasons = append(sig.Reasons,
                fmt.Sprintf("Range expansion + volume bearish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
        }
    }
}
```

### A/B test plan

Pre-commit comparison statistic (don't move goalposts after seeing results):
- **Primary:** total net R per symbol across 60d, 90d, 120d windows ending 2026-05-27, with `fee=6bp roundtrip`.
- **Secondary:** win rate, max drawdown in R.
- Run on BTC, ETH, XAU, XAG at 1h.
- **Decision rule:** ship if total net R is at least flat on 3 of 4 symbols across all three windows AND no symbol regresses by more than 10R in any single window. Otherwise revert or iterate.

After A/B passes, follow the user's gate: gather a few live observations before declaring robust.

---

## Patch 3 — Fix OI delta sign for short signals

**Why.** Current code (`signal/engine.go:308-318`) warns SHORT signals when OI drops, calling it a "short-side squeeze." That's wrong on two counts:

1. **Mechanic.** A short squeeze is a price rally that forces shorts to cover — OI rises with new shorts entering or stays flat while shorts capitulate. OI *falling* on a down-move means longs are unwinding, which *confirms* the short trade direction rather than threatening it.
2. **Text.** The warning literally says "squeeze, not new flow" while describing the opposite mechanic.

The corrected logic: warn LONG when OI falls (long unwind driving the move = no fresh demand), warn SHORT when OI rises (new shorts crowding in = squeeze setup).

**File.** `signal/engine.go:308-318`. Replace the block:

```go
if ctx.PrevOpenInterest > 0 && ctx.OpenInterest > 0 {
    oiDelta := (ctx.OpenInterest - ctx.PrevOpenInterest) / ctx.PrevOpenInterest
    if sig.Side == Long && oiDelta < -0.02 {
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("OI down %.2f%% — long unwind driving the move, not new buying", oiDelta*100))
    }
    if sig.Side == Short && oiDelta > 0.02 {
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("OI up %.2f%% — shorts crowding, squeeze risk", oiDelta*100))
    }
}
```

### Verification

No A/B gate (correctness fix, not a strategy change). Spot-check 5–10 historical shorts that triggered the old warning:
- Under the new code those should fall silent (OI was falling = long unwind = confirming the short).
- Spot-check 5–10 historical shorts where OI rose during the setup: those should now warn (correctly flagging short crowding).

---

## Engine-level A/B patches

These run **after** the must-fix patches above are merged and a fresh backtest baseline is captured. Each is independently testable; do not bundle them in a single PR — that defeats the purpose of A/B.

**Re-baseline between landings.** When any A/B patch ships, capture a new backtest baseline before measuring the next one. Comparing A/B-N+1 against the original pre-patch reference will conflate effects. In particular: **A/B-1 lowers the maximum confluence score** (RSI + BOLL collapse into one vote, so the ceiling drops from 8 to 7). After A/B-1 lands, retune the `min-score` flag in `cmd/serve/main.go` and the `threshold` argument in backtest runs — what was "score ≥ 4" pre-patch is not the same population as "score ≥ 4" post-patch. Decide the new threshold by quantile-matching signal count, not by keeping the literal number.

**Pre-committed comparison statistic for all A/B-N items below** (so we don't move goalposts after seeing results):
- Symbols: BTC, ETH, XAU, XAG at 1h.
- Windows: 60d, 90d, 120d, all ending on the latest closed bar.
- Fees: `feeBpsRoundTrip = 6` (BingX taker+maker default).
- **Headline:** total net R per (symbol, window).
- **Guardrails:** win rate, max DD in R, signal count.
- Each patch's section below names its own decision rule.

---

### A/B-1 — Factor independence (RSI + BOLL cluster as one vote)

**Why.** On 1h crypto, RSI extreme and BOLL edge fire together ~70% of the time (same blow-off / same compression). Counting both as independent +1 votes inflates the apparent confluence and miscalibrates any score threshold. MACD stays independent (different smoothings; lower empirical overlap with the cluster).

**File.** `signal/engine.go:110-134`. Replace the existing RSI / MACD / BOLL block with:

```go
// Mean-rev cluster (RSI extreme + BOLL edge) is ~0.7 correlated on 1h
// crypto; count the cluster as a single vote per direction rather than
// summing. MACD stays independent below.
rsiBull, rsiBear := false, false
bollBull, bollBear := false, false

if rsi[last] < 30 {
    rsiBull = true
    sig.Reasons = append(sig.Reasons, "RSI oversold")
} else if rsi[last] > 70 {
    rsiBear = true
    sig.Reasons = append(sig.Reasons, "RSI overbought")
}

if b := boll[last]; b.Lower != 0 {
    if price <= b.Lower {
        bollBull = true
        sig.Reasons = append(sig.Reasons, "Price at lower Bollinger")
    } else if price >= b.Upper {
        bollBear = true
        sig.Reasons = append(sig.Reasons, "Price at upper Bollinger")
    }
}

if rsiBull || bollBull {
    bullVotes++
}
if rsiBear || bollBear {
    bearVotes++
}

if m := macd[last]; m.Histogram > 0 && macd[last-1].Histogram <= 0 {
    bullVotes++
    sig.Reasons = append(sig.Reasons, "MACD bullish cross")
} else if m.Histogram < 0 && macd[last-1].Histogram >= 0 {
    bearVotes++
    sig.Reasons = append(sig.Reasons, "MACD bearish cross")
}
```

**Decision rule.** Ship if signal count drops <30% AND average net R per trade improves or stays flat (≥ −5%) on 3 of 4 symbols across all 3 windows. The score threshold downstream callers use (`threshold` in backtest, `min-score` in serve) may need re-tuning after this lands — note in commit message.

---

### A/B-2 — Funding crowd threshold: symbol-relative percentiles

**Why.** Constant ±0.05% per interval over-warns on small caps and under-warns on BTC/ETH (which routinely spike past 0.10% during basis arb frenzies without being "crowded"). Switching to 25th/75th percentile of the symbol's own recent funding distribution auto-calibrates to regime and liquidity.

**File 1.** `signal/engine.go` — extend `Context`:

```go
type Context struct {
    FundingRate      float64
    OpenInterest     float64
    PrevOpenInterest float64
    // FundingRateHistory is the rolling funding-rate series for this
    // symbol (oldest first, most recent last). When ≥10 samples are
    // provided, crowding thresholds switch from the fixed
    // FundingCrowdedLong/Short constants to the 75th/25th percentile of
    // this series. Empty/short → falls back to constants.
    FundingRateHistory []float64
}
```

**File 2.** `signal/engine.go` — `applyContextFilters`. Replace the funding block (lines 300-307) with:

```go
crowdedLong, crowdedShort := FundingCrowdedLong, FundingCrowdedShort
if len(ctx.FundingRateHistory) >= 10 {
    crowdedLong = percentile(ctx.FundingRateHistory, 0.75)
    crowdedShort = percentile(ctx.FundingRateHistory, 0.25)
}
if sig.Side == Long && ctx.FundingRate > crowdedLong {
    sig.Warnings = append(sig.Warnings,
        fmt.Sprintf("Crowded longs (funding %.4f%% > p75 %.4f%%); size down or wait",
            ctx.FundingRate*100, crowdedLong*100))
}
if sig.Side == Short && ctx.FundingRate < crowdedShort {
    sig.Warnings = append(sig.Warnings,
        fmt.Sprintf("Crowded shorts (funding %.4f%% < p25 %.4f%%); squeeze risk",
            ctx.FundingRate*100, crowdedShort*100))
}
// (OI delta block unchanged — see Patch 3 in must-fix.)
```

**File 3.** Add `percentile` helper (top or bottom of `signal/engine.go`):

```go
// percentile returns the p-th percentile (0..1) of xs using nearest-rank.
// Allocates a sorted copy — fine at the cadence of one call per signal eval.
func percentile(xs []float64, p float64) float64 {
    if len(xs) == 0 {
        return 0
    }
    sorted := make([]float64, len(xs))
    copy(sorted, xs)
    sort.Float64s(sorted)
    idx := int(float64(len(sorted)-1) * p)
    if idx < 0 {
        idx = 0
    } else if idx >= len(sorted) {
        idx = len(sorted) - 1
    }
    return sorted[idx]
}
```

Add `"sort"` to imports.

**Caller wiring (out of scope for the patch itself but required to A/B):**
- Live daemon (`cmd/serve/main.go`): fetch funding history via BingX's funding-history endpoint, populate `sigCtx.FundingRateHistory` with the last ~60 intervals (≈20 days at 8h funding).
- Backtest harness (`backtest/backtest.go`): precompute per-symbol funding series aligned to base candles, same pattern as `precomputeBiases` / `precomputeDXYTrends`. The backtest currently doesn't model funding at all, so this is a net-new requirement — flag for the implementer.

**Decision rule.** This is a *warning* tuning, not a vote — it doesn't directly change R. To measure: count "crowded" warnings under old vs new on the same trade set; new code should warn fewer times on BTC/ETH (regime-relative) and more times on lower-liquidity pairs. Manual review of 20 warned/unwarned trades determines whether the new gate aligns with discretionary judgment.

---

### A/B-3 — Regime gate (SMA50 same-TF)

**Why.** The engine has no implicit trend filter. Mean-rev shorts in uptrends and mean-rev longs in downtrends are the canonical blowup vector for this strategy class. SMA(50) at the signal timeframe is the standard coarse regime split.

**Caveat.** This is a *hard gate* — it can halve signal count. Could be too restrictive on choppy regimes; a buffered version (price must be > SMA50 × 1.005 to count as uptrend) is a later A/B candidate if this lands too binary.

**File.** `signal/engine.go`. Insert between the bias filter (ends at line 259) and the DXY veto (starts at line 273):

```go
// Regime gate. Mean-rev shorts in uptrends and mean-rev longs in
// downtrends are the classic blowup paths for this engine. SMA(50) at
// the signal TF is a coarse trend filter: only allow pro-regime
// mean-reversion. Buffered variants are a later A/B candidate.
if sig.Side != Flat && last >= 49 {
    var sum float64
    for i := last - 49; i <= last; i++ {
        sum += closes[i]
    }
    sma50 := sum / 50
    if sig.Side == Long && price < sma50 {
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("Price %.4f < SMA50 %.4f — downtrend regime, long suppressed", price, sma50))
        sig.Side = Flat
        sig.Score = 0
    } else if sig.Side == Short && price > sma50 {
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("Price %.4f > SMA50 %.4f — uptrend regime, short suppressed", price, sma50))
        sig.Side = Flat
        sig.Score = 0
    }
}
```

**Decision rule.** Ship if **average net R per filled trade rises ≥ 0.20R on 3 of 4 symbols across all 3 windows**, even if total signal count drops by up to 50%. The thesis is "fewer but better trades"; a flat avgR with halved count means the gate added no edge and just throttled volume — don't ship.

**Note on backtest representation.** The backtest's `precomputeBiases` already implements a higher-TF MACD bias filter; this same-TF SMA50 gate is *additional*. They are independent and may both be active.

---

### A/B-4 — Divergence confirmation (require pivot ≥5 bars old)

**Why.** With `pivotWidth=2`, the divergence detector accepts a pivot at index `len-3` — i.e., barely confirmed. On 1h crypto that's "today's pivot detected today", which the next 1-2 bars often invalidate.

**File 1.** `analyzer/divergence.go`. Extend `Detect` signature with `confirmBars int`:

```go
// Detect scans for divergence between price and an oscillator using the
// two most recent pivots within `lookback` bars. A pivot is a local
// extreme with `pivotWidth` bars on each side that don't exceed it.
// `confirmBars` further requires the most recent pivot to be that many
// bars older than the latest index (0 = no extra confirmation, original
// behavior). For 1h crypto, 5 filters out unconfirmed reversals.
func Detect(price, osc []float64, lookback, pivotWidth, confirmBars int) Divergence {
    if len(price) != len(osc) || len(price) < lookback || pivotWidth < 1 || confirmBars < 0 {
        return Divergence{}
    }
    start := len(price) - lookback
    if start < pivotWidth {
        start = pivotWidth
    }

    maxPivotIdx := len(price) - 1 - confirmBars

    highs := trimNewerThan(findPivots(price, start, pivotWidth, true), maxPivotIdx)
    lows := trimNewerThan(findPivots(price, start, pivotWidth, false), maxPivotIdx)

    if len(highs) >= 2 {
        a, b := highs[len(highs)-2], highs[len(highs)-1]
        if price[b] > price[a] && osc[b] < osc[a] {
            return Divergence{BearishRegular, a, b, price[a], price[b], osc[a], osc[b]}
        }
        if price[b] < price[a] && osc[b] > osc[a] {
            return Divergence{BearishHidden, a, b, price[a], price[b], osc[a], osc[b]}
        }
    }
    if len(lows) >= 2 {
        a, b := lows[len(lows)-2], lows[len(lows)-1]
        if price[b] < price[a] && osc[b] > osc[a] {
            return Divergence{BullishRegular, a, b, price[a], price[b], osc[a], osc[b]}
        }
        if price[b] > price[a] && osc[b] < osc[a] {
            return Divergence{BullishHidden, a, b, price[a], price[b], osc[a], osc[b]}
        }
    }
    return Divergence{}
}

func trimNewerThan(idxs []int, maxIdx int) []int {
    out := make([]int, 0, len(idxs))
    for _, i := range idxs {
        if i <= maxIdx {
            out = append(out, i)
        }
    }
    return out
}
```

**File 2.** `signal/engine.go` — update both `Detect` callers:

```go
rsiDiv := analyzer.Detect(closes, rsi, 60, 2, 5)   // engine.go:93
// ...
cvdDiv = analyzer.Detect(closes, cvd, 60, 2, 5)    // engine.go:104
```

**Decision rule.** Same as A/B-3 — ship if avgR per filled trade rises ≥ 0.15R on 3 of 4 symbols and total signal count drops ≤ 25%. Expect a small count reduction (divergence is one of 8 votes, and confirmation only removes the most-recent-pivot edge cases).

---

### A/B-5 — Taker buy/sell ratio divergence

**Prerequisite — blocking.** The backtest harness (`backtest/backtest.go::Run`) does not currently accept a trade stream. `signal.Inputs.Trades` is empty in every backtest evaluation, which means **the existing CVD divergence vote already does not fire in backtest** (only in live), and the new taker-ratio vote would silently do the same. Before A/B-5 is measurable, ship a prerequisite PR that:
- Adds a `trades [][]market.Trade` parameter (or per-symbol map) to `backtest.Run`.
- Populates `signal.Inputs.Trades` per evaluation by slicing the trade history up to the current candle's CloseTime (no lookahead).
- Re-baselines existing backtest numbers under "with-trades" — this alone will change CVD-divergence behaviour and shift R for symbols where CVD was previously dead weight.

Only after that PR lands and the new baseline is captured should A/B-5 be run.

**Why.** CVD divergence catches absolute-flow exhaustion; taker-ratio divergence catches *relative-flow* exhaustion (e.g., price still pushing lower while the share of taker sells contracts — sellers are losing aggression). On 1h BTC/ETH it's a faster exhaustion read than CVD alone for the same trade stream.

**Caveat — factor independence.** Taker ratio and CVD are derived from the same trades, so they will correlate. Three sub-tests:
- **5a.** Add taker-ratio divergence as an additional independent vote (risk: double-counting with CVD).
- **5b.** Cluster CVD-div and taker-ratio-div as one vote (analogous to A/B-1's RSI+BOLL cluster).
- **5c.** Replace CVD-div with taker-ratio-div entirely (cleaner signal, no cumulative drift).

**Recommend running 5b first** — it's the most defensible independence treatment.

**File 1.** `analyzer/cvd.go`. Add alongside `CVDByCandle`:

```go
// TakerBuyRatioByCandle returns per-candle taker-buy share of total taker
// volume. Range [0, 1]; 0.5 = balanced, >0.5 = buyer-dominated.
// Candles with no trades return 0.5 (neutral) to avoid spurious divergence
// signals from missing data.
func TakerBuyRatioByCandle(candles []market.Candle, trades []market.Trade) []float64 {
    out := make([]float64, len(candles))
    if len(candles) == 0 || len(trades) == 0 {
        for i := range out {
            out[i] = 0.5
        }
        return out
    }
    ti := 0
    for ci, c := range candles {
        var buyQ, sellQ float64
        for ti < len(trades) && !trades[ti].Time.After(c.CloseTime) {
            if trades[ti].Time.Before(c.OpenTime) {
                ti++
                continue
            }
            if trades[ti].BuyerMaker {
                sellQ += trades[ti].Quantity
            } else {
                buyQ += trades[ti].Quantity
            }
            ti++
        }
        total := buyQ + sellQ
        if total == 0 {
            out[ci] = 0.5
            continue
        }
        out[ci] = buyQ / total
    }
    return out
}
```

**File 2.** `signal/engine.go`. After the existing CVD divergence block (around line 184-191), add (for sub-test 5a — independent vote):

```go
var takerDiv analyzer.Divergence
if len(in.Trades) > 0 {
    ratio := analyzer.TakerBuyRatioByCandle(in.Candles, in.Trades)
    takerDiv = analyzer.Detect(closes, ratio, 60, 2, 5)
}
switch takerDiv.Kind {
case analyzer.BullishRegular:
    bullVotes++
    sig.Reasons = append(sig.Reasons, "Taker-ratio bullish divergence")
case analyzer.BearishRegular:
    bearVotes++
    sig.Reasons = append(sig.Reasons, "Taker-ratio bearish divergence")
}
```

For sub-test **5b (clustered)**, replace the CVD div block + new taker block with:

```go
var cvdDiv, takerDiv analyzer.Divergence
if len(in.Trades) > 0 {
    cvd := analyzer.CVDByCandle(in.Candles, in.Trades)
    cvdDiv = analyzer.Detect(closes, cvd, 60, 2, 5)
    ratio := analyzer.TakerBuyRatioByCandle(in.Candles, in.Trades)
    takerDiv = analyzer.Detect(closes, ratio, 60, 2, 5)
}
flowBull := cvdDiv.Kind == analyzer.BullishRegular || takerDiv.Kind == analyzer.BullishRegular
flowBear := cvdDiv.Kind == analyzer.BearishRegular || takerDiv.Kind == analyzer.BearishRegular
if flowBull {
    bullVotes++
    if cvdDiv.Kind == analyzer.BullishRegular {
        sig.Reasons = append(sig.Reasons, "CVD bullish divergence")
    }
    if takerDiv.Kind == analyzer.BullishRegular {
        sig.Reasons = append(sig.Reasons, "Taker-ratio bullish divergence")
    }
}
if flowBear {
    bearVotes++
    if cvdDiv.Kind == analyzer.BearishRegular {
        sig.Reasons = append(sig.Reasons, "CVD bearish divergence")
    }
    if takerDiv.Kind == analyzer.BearishRegular {
        sig.Reasons = append(sig.Reasons, "Taker-ratio bearish divergence")
    }
}
```

**Data prerequisite.** Backtest harness needs to load trades alongside candles (`in.Trades`). Currently `backtest.Run()` doesn't accept trades — confirm whether the harness has trade-level data available, and if not, this A/B is blocked on a backtest-loader change first. Live daemon already populates `in.Trades` if a trade subscription is active.

**Decision rule.** Run 5b first. Ship 5b if avgR per filled trade rises ≥ 0.10R on 3 of 4 symbols with signal count within ±15%. Run 5a only if 5b fails to add edge (suggests they're not redundant after all). Run 5c only if absolute CVD trend has known regime-dependence issues you want to eliminate.

---

### A/B-6 — DXY as sizing multiplier (metals only)

**Why.** The hard DXY veto cost ~46R over 60d at 1h (per the existing comment in engine.go:263-272) — macro-divergent dips into a strengthening dollar are exactly where this mean-rev strategy has its best edge on metals. But the trades aren't *risk-free* — they're profitable on average with elevated drawdown. Halving size instead of vetoing keeps the edge while reducing drawdown contribution.

**File 1.** `signal/engine.go`. Add field to `Signal` struct:

```go
type Signal struct {
    Symbol    market.Symbol
    Timeframe market.Timeframe
    Side      Side
    Score     int
    Reasons   []string
    Notes     []string
    Warnings  []string
    Price     float64
    Fib       indicator.FibRetracement
    VP        indicator.VolumeProfile
    Opens     Opens
    Plan      Plan
    // SizeMultiplier is a discretionary position-size scalar in (0, 1].
    // Default 1.0 (full size). Currently set to 0.5 by the DXY soft-gate
    // on metals when DXY trends against the trade. Downstream consumers
    // (validator display, notifier output, executor) apply this when
    // sizing.
    SizeMultiplier float64
}
```

**File 2.** `signal/engine.go::Evaluate`. Initialize `SizeMultiplier: 1.0` in the `sig := Signal{...}` literal (currently line 107).

**File 3.** `signal/engine.go`. Replace the DXY veto block (lines 273-286):

```go
if isPreciousMetal(in.Symbol) && sig.Side != Flat {
    switch {
    case sig.Side == Long && in.DXYTrend == dxy.Up:
        sig.SizeMultiplier = 0.5
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("DXY uptrend — long %s into USD strength, size halved",
                shortName(in.Symbol)))
    case sig.Side == Short && in.DXYTrend == dxy.Down:
        sig.SizeMultiplier = 0.5
        sig.Warnings = append(sig.Warnings,
            fmt.Sprintf("DXY downtrend — short %s into USD weakness, size halved",
                shortName(in.Symbol)))
    }
}
```

**Struct-literal callers must be updated.** Adding `SizeMultiplier` to `Signal` means every place in the repo that constructs `Signal{...}` as a struct literal needs `SizeMultiplier: 1.0` set explicitly, or it defaults to 0 and any downstream multiplier-aware sizing silently zero-out positions. Before merging, grep for `signal.Signal{` and `Signal{` across the repo (tests, fixtures, mocks especially) and patch each site. `Evaluate()` is one of those sites and is already covered in File 2 above — others may exist in test/helper code.

**Downstream wiring (required for A/B but not in the patch itself):**
- **Backtest.** `backtest/backtest.go::computeStats` — also compute a size-weighted variant: `weightedR = sum(trade.R * trade.SizeMultiplier)`. Easiest: store `SizeMultiplier` on each `Trade` (populate from `sig.SizeMultiplier` at simulate time), then compute both `TotalR` (unscaled, for diagnostic) and `TotalSizeWeightedR` (the realistic portfolio outcome). Compare the latter across A/B arms.
- **Validator** (`validator/validator.go`) — surface `EngineSizeMultiplier` in the result and apply it to the "suggested size" line in the verdict.
- **Notifier** — include the multiplier in the alert when < 1.0.
- **Live DXY data source — separate piece of work.** The live daemon (`cmd/serve/main.go`) currently passes `Inputs{}` without populating `DXYTrend`, so the soft-gate would never fire in live until DXY candles are fetched and classified each scan. Backtest already supports DXY via `opts.DXYCandles`. Wiring live DXY (which provider? cache cadence? failure fallback?) is its own design decision — file as a follow-up issue rather than blocking A/B-6. A/B-6 is still measurable in backtest as-is.

**Decision rule.** XAU/XAG only (BTC/ETH unaffected). Compare three arms:
- **Arm 0 (current):** DXY off in CLI, `DXYTrend = dxy.Flat`, all signals at SizeMultiplier 1.0.
- **Arm 1 (legacy hard veto):** DXY on, current veto code, signal Flat on adverse trend.
- **Arm 2 (new soft-gate):** DXY on, this patch, SizeMultiplier 0.5 on adverse trend.

Ship Arm 2 if **size-weighted net R > Arm 0** on both metals across all 3 windows, **AND** max-DD-in-R is lower than Arm 0. If both can't be achieved, stay on Arm 0 (DXY off).

---

## Validator A/B patches

These tune the validator scoring rubric in `validator/validator.go`. Unlike the engine-level patches, validator changes are **not backtestable in the same way** — the validator scores *manually proposed* entries, not auto-fired signals. There is no automatic P&L delta to compare.

**How to A/B a validator change.** Replay the rubric against a population of historical entries with known outcomes:
- Pull every entry from `journal/` over the last 60-120d that you actually took (or rejected).
- Re-score each entry under the new rubric.
- Check whether the new verdict tier (AVOID / WEAK / NEUTRAL / TAKE / STRONG TAKE) better separates winners from losers — e.g., win rate of TAKE-or-better should rise, win rate of WEAK-or-worse should fall.
- **Pre-committed decision rule for V-N below** unless overridden: ship if `(STRONG TAKE + TAKE)` net R per trade rises ≥ 0.10R on a population of ≥30 historical entries, AND `AVOID + WEAK` average R does not rise (we want the worst tier to stay the worst tier).

Each patch is small and independently shippable; bundle V-1 through V-3 in one PR is OK since they don't interact materially. V-4 is its own PR (new mechanism). V-5 is an audit, not a code change.

---

### V-1 — BOLL weight: +0.5 → +1.0

**Why.** The engine treats BOLL as a co-equal confluence vote alongside RSI/MACD/Fib/Sweep. The validator gives it only +0.5, less than half the weight of "engine score ≥ 3" or "fib 0.618 alignment". Bumping to +1.0 brings the rubric in line with how the engine actually consumes BOLL.

**File.** `validator/validator.go:252-259`. Change both `+0.5` values to `+1.0`:

```go
if r.BollLower != 0 {
    if r.Side == signal.Long && math.Abs(r.BollLower-r.Entry)/r.Entry <= 0.005 {
        fs = append(fs, Factor{"entry at lower Bollinger", +1.0, fmt.Sprintf("BOLL lower @ %.4f", r.BollLower)})
    }
    if r.Side == signal.Short && math.Abs(r.BollUpper-r.Entry)/r.Entry <= 0.005 {
        fs = append(fs, Factor{"entry at upper Bollinger", +1.0, fmt.Sprintf("BOLL upper @ %.4f", r.BollUpper)})
    }
}
```

**Decision rule.** Standard replay rule above. Note: this only affects entries placed near a BOLL edge (~0.5% tolerance), so the rule needs at least 30 such entries in the replay population to be statistically meaningful — pull a longer journal window if needed.

---

### V-2 — HVN scoring: make penalty/bonus magnitudes symmetric

**Why.** Current asymmetry: right-side HVN +1.5, wrong-side HVN −1.0. The bonus rewards "HVN supports my direction"; the penalty under-weights "HVN works against my direction". On 1h crypto, HVNs absorb price equally in both directions — the rubric should reflect that.

**File.** `validator/validator.go:264-273`. Two sub-arms; ship one:

**V-2a (recommended) — symmetric at ±1.5:**

```go
if r.IsHVNPOC {
    fs = append(fs, Factor{"entry at POC", -1.0, fmt.Sprintf("POC @ %.4f — chop/equilibrium, weak mean-rev edge", r.NearestHVN)})
} else {
    if r.Side == signal.Long && r.Entry <= r.NearestHVN {
        fs = append(fs, Factor{"entry below non-POC HVN (chip support)", +1.5, fmt.Sprintf("HVN @ %.4f acts as support", r.NearestHVN)})
    } else if r.Side == signal.Short && r.Entry >= r.NearestHVN {
        fs = append(fs, Factor{"entry above non-POC HVN (chip resistance)", +1.5, fmt.Sprintf("HVN @ %.4f acts as resistance", r.NearestHVN)})
    } else {
        fs = append(fs, Factor{"entry near HVN but wrong side for direction", -1.5, fmt.Sprintf("HVN @ %.4f fights the %s", r.NearestHVN, r.Side)})
    }
}
```

**V-2b (conservative alternative) — symmetric at ±1.0:** same code but both `+1.5` become `+1.0` and `-1.5` becomes `-1.0`. Less impact per HVN factor; safer if HVN-as-S/R is a noisier signal than current calibration assumes.

**Decision rule.** Replay rule above. Additional check: walk 10 historical entries where the old code gave +1.5 and the new code (V-2a) gives +1.5 — these should be unchanged (sanity). Walk 10 where old gave -1.0 and new V-2a gives -1.5 — these should be cases you'd genuinely call "wrong side" in retrospect, not edge cases at the HVN boundary.

---

### V-3 — Tighten chase tiers (5× tighter)

**Why.** Current chase tiers (0.2% / 0.5% / 1.5%) are calibrated for moves so large they're already past the "chase" stage. At BingX taker fees (5bp) and 1h BTC spread/depth, anything past 0.05% is materially worse than mid; past 0.30% the edge is gone.

**File.** `validator/validator.go:312-336`. Replace the chase switch block:

```go
// Chase tiers (post-2026-05-27 retune). Old tiers (0.2%/0.5%/1.5%) were
// 5× too lax — at BingX taker fees and 1h BTC depth, 0.05% is already
// material slippage, 0.30%+ is paying up for a played-out move.
chase := (r.Entry - r.Price) / r.Price
chasePct := chase * 100 // raw % (positive = entry above market) for display
if r.Side == signal.Short {
    chase = -chase // flip so positive = chasing in either direction
}
switch {
case chase > 0.003:
    fs = append(fs, Factor{
        "chasing market significantly",
        -2.5,
        fmt.Sprintf("entry %+.2f%% vs market — paying up for a move that already happened", chasePct),
    })
case chase > 0.001:
    fs = append(fs, Factor{
        "chasing market",
        -1.5,
        fmt.Sprintf("entry %+.2f%% vs market — late on the move", chasePct),
    })
case chase > 0.0005:
    fs = append(fs, Factor{
        "mild market chase",
        -0.5,
        fmt.Sprintf("entry %+.2f%% vs market", chasePct),
    })
}
```

**Manual calibration step before A/B.** Walk through 10 recent validated entries and check the chase % under each. If most fall above the new "mild" threshold (0.05%), the tiers are biting too hard — relax to 0.07% / 0.15% / 0.40% as a softer first move. If most still score "no penalty", the new tiers are about right.

**Decision rule.** Replay rule above, but the relevant cohort is entries with `chasePct > 0.05%`. The new rubric should rank these *lower* on average than the old rubric did, and the lowest-ranked among them (those scoring AVOID/WEAK under new) should have a worse net R than the rest.

---

### V-4 — Macro-event penalty

**Why.** Mean-reversion strategies blow up around scheduled high-impact macro releases (US CPI, FOMC, NFP, PCE) — volatility spikes, spreads widen, mean-rev levels get vaporized. Currently the validator has no awareness of the macro calendar.

**Design.** Keep the validator pure. Expose a package-level macro-event list that the caller populates at startup from whatever source it prefers (hardcoded slice, YAML file, API). Validator checks the latest candle's CloseTime against the list and adds a −1.5 penalty when within the configured window.

**File 1.** Add a new file `validator/macro.go`:

```go
package validator

import "time"

// MacroEvents is the caller-provided list of high-impact macro release
// timestamps (UTC). The validator emits a -1.5 penalty when the proposed
// entry's bar CloseTime falls within MacroEventPreWindow before, or
// MacroEventPostWindow after, any entry. Callers populate this at startup
// from their preferred source. Empty slice = no penalty applied.
//
// Recommended events (US/crypto-relevant):
//   - CPI release: monthly, 12:30 UTC (08:30 ET)
//   - FOMC rate decision: 8x/year, 18:00 UTC (14:00 ET)
//   - FOMC presser: 8x/year, 18:30 UTC
//   - NFP: monthly first Friday, 12:30 UTC
//   - PCE: monthly, 12:30 UTC
var (
    MacroEvents          []time.Time
    MacroEventPreWindow  = 30 * time.Minute // suppress signals this far before an event
    MacroEventPostWindow = 90 * time.Minute // and this far after (vol decay)
)

// withinMacroWindow returns the offending event time and true if `t` is
// within the pre/post window of any MacroEvents entry.
func withinMacroWindow(t time.Time) (time.Time, bool) {
    for _, e := range MacroEvents {
        if !t.Before(e.Add(-MacroEventPreWindow)) && !t.After(e.Add(MacroEventPostWindow)) {
            return e, true
        }
    }
    return time.Time{}, false
}
```

**File 2.** `validator/validator.go` — extend `Result` with the bar time, and pass it through:

```go
type Result struct {
    // ... existing fields ...
    BarTime time.Time // CloseTime of the latest candle; used for macro-window check
    // ... rest unchanged ...
}
```

In `Validate()`, populate it (just before the existing `r := Result{...}` line, or as a new field in the literal):

```go
r := Result{
    Symbol:      sym,
    Timeframe:   tf,
    Side:        side,
    Entry:       entry,
    Price:       price,
    BarTime:     candles[len(candles)-1].CloseTime,
    // ... rest unchanged ...
}
```

**File 3.** `validator/validator.go::scoreFactors` — add a new factor near the bottom (after the fee math block):

```go
if !r.BarTime.IsZero() {
    if event, hit := withinMacroWindow(r.BarTime); hit {
        fs = append(fs, Factor{
            "inside macro-event window",
            -1.5,
            fmt.Sprintf("within %v of %s — vol spike risk",
                MacroEventPostWindow, event.UTC().Format("2006-01-02 15:04 UTC")),
        })
    }
}
```

**Caller wiring (separate, not part of the patch).** Add a small helper or config file that populates `validator.MacroEvents` at startup. Simplest: a hand-maintained Go slice in `cmd/validate/macro_events_2026.go` (and the same for the web entry point) committed alongside the code. The user refreshes it monthly. If the slice goes stale, no harm — the penalty just stops firing.

**Decision rule.** Different from the replay rule: count how many entries in the historical journal fell within a macro window. If it's < 5 entries over 90d, this patch adds almost no signal and isn't worth shipping — the rubric is fine without it. If it's ≥ 10 entries, replay them: their net R under the old rubric was likely poor (high vol → wider stops → worse fill), and the new −1.5 should push them toward AVOID/WEAK. Ship if so.

---

### V-5 — Audit last 20 STRONG TAKE entries (process, no code)

**Not a code patch — a workflow item.** Walk the most recent 20 entries from `journal/` that scored ≥ 8 ("STRONG TAKE — full size") under the current rubric. For each, ask:

1. **How many *independent* factors fired?** A STRONG TAKE could be (engine aligned +2) + (engine score ≥3 +1) + (sweep at level +2) + (fib 0.618 +1.5) + (BOLL edge +0.5) + (fee healthy +1) = 8.0, which is *4 independent factors* (sweep, fib, BOLL edge, fee). Engine alignment and engine-score-tradeable are correlated by construction. List the independent factors per entry.

2. **What was the realized R?** Pull from journal.

3. **Cluster correlated factors.** If "engine aligned + engine score ≥ 3" and "sweep at level + entry sweep-direction" are showing up in the same entries (they often will), the rubric is gaming itself: one underlying truth (a sweep-aligned engine signal) is scoring twice.

**Decision.** If ≥ 5 of 20 STRONG TAKEs had ≤ 3 independent factors *and* their realized R was at-or-below the TAKE-tier median, the rubric is over-scoring. Candidates to A/B in response:
- Cap the engine-related factors (alignment + score ≥ 3) at a combined +2.0 instead of +3.0.
- Add a "factor diversity bonus" only when ≥ 4 distinct factor *categories* fire.

Don't change anything until the audit data is in hand — this is the "look first, then decide" item.

---

## Backtest harness patches

These tighten the backtest's realism so A/B comparisons measure something closer to live performance. None of them change strategy logic — they change how we *measure* it.

**Order.** B-1 must land **before** B-2 — the B-2 code includes the comment "see B-1" referencing the annotation B-1 introduces. B-1 is trivial (one comment), so this is not a real bottleneck. B-2 should land before re-running any A/B from the engine/validator sections so the new baseline reflects realistic stop fills. B-3 is tooling that wraps existing backtest runs, no impact on numbers themselves; ship it whenever.

**Re-baseline after B-2.** Any earlier A/B result captured under the pre-B-2 simulator should be recomputed before declaring it shipped — the headline R will shift.

---

### B-1 — Document the existing pessimistic stop-before-TP ordering

**Why.** `backtest/backtest.go:211-225` checks `c.Low <= plan.StopLoss` *before* `c.High >= tp` (long path; mirrored for short). When a bar touches both the stop and TP, the simulator resolves to stop — the pessimistic case. The reviewer team initially flagged this as a missing pessimism feature; it's actually already there, just undocumented. Add a comment so a future reader doesn't "fix" it by reordering the checks.

**File.** `backtest/backtest.go:211`, just before the `if sig.Side == signal.Long {` block. Add:

```go
// Intra-bar SL/TP resolution is deliberately pessimistic: the stop check
// runs before the TP check, so a bar that touches both resolves to stop.
// OHLC doesn't disclose intra-bar order, and assuming "TP first" would
// bias results upward. Do not reorder these blocks.
```

No behavioural change. Pure annotation.

---

### B-2 — Realistic stop fills (slippage + same-bar fill-then-stop)

**Why.** Two optimism leaks in `simulate()`:

1. **Stop fills price exactly at `plan.StopLoss`.** In live markets, fast moves slip past the stop trigger by 1–5 bps before a market exit lands — especially on 1h crypto stops, which often coincide with liquidity vacuums.
2. **The fill bar skips its own stop/TP check.** A limit-fill bar that filled at `plan.Entry` then continued past the stop (e.g., long fill at $100 with bar low $97, stop $98) currently records *no exit* on that bar; stop/TP only get checked from the next bar onward. That's optimistic — the trade should have been stopped out same bar.

**File 1.** `backtest/backtest.go::Options`. Add a new field:

```go
type Options struct {
    FeeBpsRoundTrip float64
    SweepOnly       bool
    DXYCandles      []market.Candle

    // StopSlippageBps is extra slippage (in bps of entry price) applied to
    // stop-loss exits only. Models the "market sells past the stop trigger"
    // dynamic on fast moves. Typical: 1-2 bps for BTC/ETH at 1h, 3-5 bps for
    // metals. Zero = no extra slippage (current behaviour). TP fills are
    // assumed to be limit exits at the target and are not slipped.
    StopSlippageBps float64
}
```

**File 2.** `backtest/backtest.go::simulate`. Update the loop and stop math:

```go
// Compute the slipped stop exit price once (long: stop - slip; short: stop + slip).
slipFrac := opts.StopSlippageBps / 10000.0
slippedStopLong := plan.StopLoss - slipFrac*plan.Entry
slippedStopShort := plan.StopLoss + slipFrac*plan.Entry

filled := false
var fillIdx int
for i, c := range future {
    if !filled {
        if c.Low <= plan.Entry && plan.Entry <= c.High {
            filled = true
            fillIdx = i
            // Fill-bar stop check (pessimistic): if the same bar already
            // ranged past the stop in the wrong direction after the limit
            // fill could plausibly have triggered, treat it as stopped
            // out same bar. We can't tell intra-bar order from OHLC, so we
            // assume worst case for the trader.
            if sig.Side == signal.Long && c.Low <= plan.StopLoss {
                exit := slippedStopLong
                r := (exit - plan.Entry) / risk
                return mkTrade(r, exit, c.CloseTime, "stop"), i
            }
            if sig.Side == signal.Short && c.High >= plan.StopLoss {
                exit := slippedStopShort
                r := (plan.Entry - exit) / risk
                return mkTrade(r, exit, c.CloseTime, "stop"), i
            }
        }
        continue
    }
    // Intra-bar SL/TP resolution is deliberately pessimistic: stop is
    // checked before TP, so a bar that touches both resolves to stop.
    // OHLC doesn't disclose intra-bar order; assuming "TP first" would
    // bias results upward. Do not reorder these blocks.
    if sig.Side == signal.Long {
        if c.Low <= plan.StopLoss {
            exit := slippedStopLong
            r := (exit - plan.Entry) / risk
            return mkTrade(r, exit, c.CloseTime, "stop"), i
        }
        if c.High >= tp {
            return mkTrade(2, tp, c.CloseTime, "tp2"), i
        }
    } else {
        if c.High >= plan.StopLoss {
            exit := slippedStopShort
            r := (plan.Entry - exit) / risk
            return mkTrade(r, exit, c.CloseTime, "stop"), i
        }
        if c.Low <= tp {
            return mkTrade(2, tp, c.CloseTime, "tp2"), i
        }
    }
}
```

**File 3.** `simulate()` signature — it currently doesn't receive `opts`. Either thread `opts.StopSlippageBps` through as a parameter, or thread the whole `Options`. Cleanest is to add `slip float64` as a new parameter and let the caller pass it:

```go
// simulate signature
func simulate(sym market.Symbol, sig signal.Signal, future []market.Candle, feeBps, stopSlipBps float64) (*Trade, int) {
```

And the call site in `Run` (line 113):

```go
tr, lastIdx := simulate(sym, sig, candles[i+1:end], opts.FeeBpsRoundTrip, opts.StopSlippageBps)
```

**Decision rule.** No A/B ship/no-ship — this is a realism fix, not a strategy change. Expected effect: total R drops slightly (every stopped trade costs ~slip bps more, plus the fill-bar stops now happen). Re-baseline all engine/validator A/B results under the new simulator. Sanity check: with `StopSlippageBps = 0`, the new simulator should produce *almost* identical results to the old one, except where fill-bar stops are now caught — diff those few trades by hand to confirm they look right.

**Default value to commit.** Set `StopSlippageBps` default at the CLI runner level (in `cmd/backtest/`), not in the package. Recommend **1.5 bps for BTC/ETH at 1h** and **3 bps for XAU/XAG at 1h** based on rough order-book observation; refine after live observation accumulates.

---

### B-3 — High-volatility window guard for period selection

**Why.** A 60/90/120d backtest cherry-picked over a calm regime overstates mean-reversion edge — BOLL/RSI/sweep all behave well in chop. The strategy has to hold up through at least one high-vol episode to be credible. Today nothing prevents a user from running 60d ending in a calm month and shipping on those numbers.

**Approach.** Don't *force* a window selection — just *warn* the user when the chosen backtest period doesn't overlap any high-vol episode in the surrounding 365d. The user decides whether to extend.

**File 1.** Add a new file `backtest/vol_windows.go`:

```go
package backtest

import (
    "time"

    "myFirstGo/trading-bot/indicator"
    "myFirstGo/trading-bot/market"
)

// VolWindow is a contiguous span where ATR(14) ran hot relative to its
// own rolling baseline. Used to verify backtest period coverage.
type VolWindow struct {
    Start time.Time
    End   time.Time
    PeakATRMul float64 // peak ATR / baseline, for diagnostic
}

// FindHighVolWindows scans candles for spans where ATR(14) exceeded
// `multiple` times its trailing 200-bar mean for at least `minBars`
// consecutive bars. Default thresholds for 1h crypto: multiple=1.8,
// minBars=12 (~half-day sustained vol).
func FindHighVolWindows(candles []market.Candle, multiple float64, minBars int) []VolWindow {
    if len(candles) < 220 {
        return nil
    }
    atr := indicator.ATR(candles, 14)

    rollingMean := func(end int) float64 {
        start := end - 200
        if start < 0 {
            start = 0
        }
        var sum float64
        var n int
        for i := start; i < end; i++ {
            sum += atr[i]
            n++
        }
        if n == 0 {
            return 0
        }
        return sum / float64(n)
    }

    var out []VolWindow
    inWindow := false
    var curStart int
    var curPeakMul float64

    for i := 200; i < len(candles); i++ {
        base := rollingMean(i)
        if base == 0 {
            continue
        }
        mul := atr[i] / base
        if mul >= multiple {
            if !inWindow {
                inWindow = true
                curStart = i
                curPeakMul = mul
            } else if mul > curPeakMul {
                curPeakMul = mul
            }
        } else if inWindow {
            if i-curStart >= minBars {
                out = append(out, VolWindow{
                    Start:      candles[curStart].OpenTime,
                    End:        candles[i-1].CloseTime,
                    PeakATRMul: curPeakMul,
                })
            }
            inWindow = false
            curPeakMul = 0
        }
    }
    if inWindow && len(candles)-curStart >= minBars {
        out = append(out, VolWindow{
            Start:      candles[curStart].OpenTime,
            End:        candles[len(candles)-1].CloseTime,
            PeakATRMul: curPeakMul,
        })
    }
    return out
}

// CoversHighVol reports whether the backtest window [start, end] overlaps
// any of the supplied high-vol windows.
func CoversHighVol(start, end time.Time, windows []VolWindow) bool {
    for _, w := range windows {
        if !w.End.Before(start) && !w.Start.After(end) {
            return true
        }
    }
    return false
}
```

**File 2.** Integrate into the backtest CLI runner (`cmd/backtest/main.go` or equivalent). After running `backtest.Run`, before printing the summary, add:

```go
windows := backtest.FindHighVolWindows(candles, 1.8, 12)
btStart := candles[warmup].OpenTime
btEnd := candles[len(candles)-1].CloseTime
if !backtest.CoversHighVol(btStart, btEnd, windows) {
    log.Printf("WARN: backtest period [%s..%s] does not overlap any high-vol window in the loaded history. Available windows:", btStart.Format("2006-01-02"), btEnd.Format("2006-01-02"))
    for _, w := range windows {
        log.Printf("  %s..%s (peak ATR %.2fx)", w.Start.Format("2006-01-02"), w.End.Format("2006-01-02"), w.PeakATRMul)
    }
    log.Printf("Consider extending the backtest window or running an additional pass over one of the listed periods before shipping.")
}
```

(The exact `warmup` constant is `100` per `backtest.go:87` — replicate or export it.)

**Decision rule.** No ship/no-ship; this patch only emits a warning. Implementer should verify by running a deliberately calm period (e.g., a quiet quarter on BTC) and confirming the warning fires, then running a window known to include a high-vol episode and confirming it doesn't fire.

**Calibration knobs.** `multiple=1.8, minBars=12` is the suggested default for 1h crypto — picks up most material moves without flagging every minor wobble. Run `FindHighVolWindows` over 365d of BTC 1h data and inspect the output; if more than ~6 windows fire per year the threshold is too sensitive (raise `multiple`), if fewer than ~3 it's too strict (lower `multiple` or `minBars`).

---

## Reference — remaining queue

Everything from the original review is now drafted. Validator V-5 is an audit, not a code change; B-1 is a doc-only annotation. All other items are concrete patches above with decision rules.

**Cross-patch prerequisites recap** (so the implementer doesn't get stuck mid-A/B):
- Engine A/B-5 (taker ratio) requires a separate "plumb Trades into backtest" PR first — see A/B-5 section.
- Engine A/B-6 (DXY soft-gate) is measurable in backtest but for live use needs a DXY data fetcher in the daemon — separate follow-up.
- Validator V-4 (macro penalty) requires hand-maintained `MacroEvents` slice — separate follow-up.
- Backtest B-2 (stop slippage) should land before re-baselining any engine/validator A/B.
