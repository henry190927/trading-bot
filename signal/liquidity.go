package signal

import (
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
