package signal

import (
	"fmt"
	"sort"

	"myFirstGo/trading-bot/market"
)

// Liquidity detection — equal highs / equal lows (EQH/EQL) as resting-liquidity
// pools. When 2+ swing highs (or lows) print at ~the same price, the stops of
// traders leaning the other way (plus breakout orders) pile up just beyond that
// level. SMC price-action reads these as magnets: price tends to run to sweep the
// liquidity before reversing. Extracted from a group-chat SMC method (same source
// as the N-struct work) to be mechanized + A/B'd, NOT taken on faith — see
// docs/ and the backtest --liq flags before wiring into live signals.

// LiquidityKind labels which side the pool sits on.
type LiquidityKind string

const (
	EQH LiquidityKind = "EQH" // equal highs — buy-side liquidity ABOVE price
	EQL LiquidityKind = "EQL" // equal lows — sell-side liquidity BELOW price
)

// LiquidityLevel is one detected pool: a cluster of same-side swings within a
// price tolerance.
type LiquidityLevel struct {
	Kind    LiquidityKind
	Price   float64 // representative level (mean of the cluster)
	Lo, Hi  float64 // cluster band (min/max swing price in it)
	Touches int     // number of swings forming the pool (>=2)
	LastIdx int     // most-recent candle index in the cluster (freshness)
}

// LiqVoteProxPct, when > 0, turns EQH/EQL pool PROXIMITY into a mean-reversion
// confluence vote inside Evaluate: an EQH within this many percent ABOVE price
// votes bear, an EQL within it BELOW price votes bull. 0 = off (the shipped
// default — pools stay an entry trigger via sweep-reject and a display layer,
// contributing nothing to the engine score).
//
// The polarity matches the only pool use that has ever passed an A/B
// (sweep-reject: run above an EQH then close back below → short; mirror for
// EQL → long), i.e. EQH reads bearish and EQL bullish.
//
// A/B-gated per [[feedback_strategy_changes]]: wire via cmd/backtest --liq-vote
// and prove +R across 60/90/120d per symbol before considering a default.
// TESTED 2026-09-02 → REJECTED in BOTH polarities, at two tolerances, across
// 60/90/120d on the core four. Core-4 total netR:
//
//	variant        60d      90d      120d
//	baseline    -11.27    -4.62    +4.36
//	wall 0.5    -17.50   -40.44   -39.89
//	magnet 0.5   -1.59    -7.08   -22.86
//	magnet 0.2  -16.53    -5.25   -12.08
//
// wall is worse every window and worsens with data; magnet flatters only the
// shortest window then degrades monotonically, and swings 15R on a tolerance
// tweak (60d: -1.59 at 0.5 → -16.53 at 0.2) with each window preferring a
// different param. Metals are worse under every variant. Kept off-by-default
// as documentation, alongside cmd/sweepbt's rejected-flag pile.
//
// WHY, and it generalises: signal counts scale monotonically with the tolerance
// (BTC 60d n=47 baseline → 56 at 0.2 → 64 at 0.5), so the vote reliably DILUTES
// a fixed count-threshold with marginal setups — the identical failure mode
// already documented for HVN-proximity voting at engine.go's HVN note, now
// reproduced independently on a different level type. Performance is not even
// monotone in the param, meaning the added votes carry ~no information.
// Proximity to a level is a DISTANCE; the pool edge that works (sweep-reject)
// is an EVENT — crossing then closing back. Distances don't score here.
var LiqVoteProxPct = 0.0

// LiqVoteMagnet inverts LiqVoteProxPct's polarity to test the competing
// "liquidity magnet" reading — that an unswept pool DRAWS price toward it, so
// an EQH above is bullish (price wants to run the stops) and an EQL below is
// bearish. Only meaningful when LiqVoteProxPct > 0. Testing both signs avoids
// concluding "pools don't score" from one arbitrary direction choice.
var LiqVoteMagnet = false

// liqVoteVerdict reports the MR votes pool proximity implies at `price`, given
// pools computed from bars STRICTLY BEFORE the evaluated bar (no look-ahead).
// Returns (bull, bear) increments and a reason string for whichever fired.
func liqVoteVerdict(pools []LiquidityLevel, price float64) (bull, bear int, reason string) {
	if LiqVoteProxPct <= 0 || price <= 0 {
		return 0, 0, ""
	}
	above, below := NearestLiquidity(pools, price)
	tol := LiqVoteProxPct / 100.0
	// Only the pool on each side that is actually within tolerance counts, and
	// only EQH-above / EQL-below are considered — an EQH that price has already
	// traded through sits BELOW and is a spent pool, which is a different
	// (untested) polarity-flip idea, deliberately not folded in here.
	if above != nil && above.Kind == EQH && (above.Price-price)/price <= tol {
		if LiqVoteMagnet {
			bull++
			reason = fmt.Sprintf("EQH pool %.4f within %.2f%% above (magnet: draws price up)", above.Price, LiqVoteProxPct)
		} else {
			bear++
			reason = fmt.Sprintf("EQH pool %.4f within %.2f%% above (overhead supply)", above.Price, LiqVoteProxPct)
		}
	}
	if below != nil && below.Kind == EQL && (price-below.Price)/price <= tol {
		if LiqVoteMagnet {
			bear++
			if reason != "" {
				reason += " · "
			}
			reason += fmt.Sprintf("EQL pool %.4f within %.2f%% below (magnet: draws price down)", below.Price, LiqVoteProxPct)
		} else {
			bull++
			if reason != "" {
				reason += " · "
			}
			reason += fmt.Sprintf("EQL pool %.4f within %.2f%% below (underlying demand)", below.Price, LiqVoteProxPct)
		}
	}
	return bull, bear, reason
}

// FindLiquidity returns EQH and EQL pools from the last `lookback` swings, where a
// pool is 2+ same-side swings whose prices sit within tolPct of each other (e.g.
// tolPct 0.0015 = 0.15%). strength is the swing pivot strength (2 matches the rest
// of the engine / web bias). Results are sorted by price ascending.
func FindLiquidity(candles []market.Candle, strength, lookback int, tolPct float64) []LiquidityLevel {
	pts := FindSwingPoints(candles, strength, lookback)
	var highs, lows []SwingPoint
	for _, p := range pts {
		if p.IsTop {
			highs = append(highs, p)
		} else {
			lows = append(lows, p)
		}
	}
	out := append(clusterSwings(highs, EQH, tolPct), clusterSwings(lows, EQL, tolPct)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out
}

// clusterSwings groups same-side swings whose prices are within tolPct into pools.
// Greedy over price-sorted swings: a swing joins the current cluster while it's
// within tolPct of the cluster's anchor price; a cluster of >=2 becomes a pool.
func clusterSwings(sw []SwingPoint, kind LiquidityKind, tolPct float64) []LiquidityLevel {
	if len(sw) < 2 {
		return nil
	}
	sorted := make([]SwingPoint, len(sw))
	copy(sorted, sw)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Price < sorted[j].Price })

	var out []LiquidityLevel
	i := 0
	for i < len(sorted) {
		anchor := sorted[i].Price
		lo, hi := anchor, anchor
		sum := 0.0
		n := 0
		lastIdx := 0
		j := i
		for j < len(sorted) && (sorted[j].Price-anchor) <= anchor*tolPct {
			p := sorted[j]
			if p.Price < lo {
				lo = p.Price
			}
			if p.Price > hi {
				hi = p.Price
			}
			sum += p.Price
			if p.Index > lastIdx {
				lastIdx = p.Index
			}
			n++
			j++
		}
		if n >= 2 {
			out = append(out, LiquidityLevel{
				Kind: kind, Price: sum / float64(n), Lo: lo, Hi: hi, Touches: n, LastIdx: lastIdx,
			})
		}
		i = j
	}
	return out
}

// NearestLiquidity returns the closest EQH strictly above price and the closest
// EQL strictly below it (nil if none) — the immediate upside/downside magnets.
func NearestLiquidity(levels []LiquidityLevel, price float64) (above, below *LiquidityLevel) {
	for i := range levels {
		l := &levels[i]
		if l.Kind == EQH && l.Price > price {
			if above == nil || l.Price < above.Price {
				above = l
			}
		}
		if l.Kind == EQL && l.Price < price {
			if below == nil || l.Price > below.Price {
				below = l
			}
		}
	}
	return above, below
}
