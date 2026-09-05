package signal

// Swing shelves — the second half of A2 (auto-drawn S/R bands).
//
// The EQH/EQL layer in liquidity.go clusters swings of the SAME side: N highs
// at one price is buy-side liquidity resting above, which is a sweep TARGET.
// A shelf is the other thing: a band that swing highs AND swing lows both fall
// into, i.e. a price the market has pivoted around in both directions. That is
// what "support/resistance" actually means, and it is not what an EQH pool
// measures — a pool of 5 highs and a shelf touched from both sides look
// identical to a same-side clusterer and behave differently.
//
// DISPLAY ONLY, and deliberately so. This votes nothing and gates nothing.
// Every pool-derived scoring idea tried so far has been rejected by A/B
// (proximity votes, EQH/EQL-as-vote, polarity flip), so a new band detector
// arrives as a drawing, not as a signal. If it ever earns a vote it does so
// through cmd/gate like everything else.

import (
	"sort"

	"myFirstGo/trading-bot/market"
)

// Shelf is a price band touched by swings of both kinds.
type Shelf struct {
	Price  float64 // mean of every pivot in the band
	Lo, Hi float64 // band edges (min/max pivot price)

	Highs int // swing highs in the band
	Lows  int // swing lows in the band

	FirstIdx int // earliest pivot index — with LastIdx gives the band's age
	LastIdx  int // most recent pivot index (freshness)
}

// Touches is the total pivot count forming the shelf.
func (s Shelf) Touches() int { return s.Highs + s.Lows }

// Span is how many bars separate the first and last touch. A band touched
// across 300 bars is a different object from one touched five times inside a
// ten-bar chop, and nothing else in the struct distinguishes them — the touch
// count alone rates the chop higher.
func (s Shelf) Span() int { return s.LastIdx - s.FirstIdx }

// FindShelves returns bands where at least one swing high and one swing low
// cluster within tolPct, with at least minTouches pivots in total.
//
// tolPct is fractional (0.0015 = 15bps), matching FindLiquidity. minTouches
// below 2 is raised to 2, since a "band" needs at least one of each side and
// that is already two.
//
// Sorted by price so callers can draw them bottom-up.
func FindShelves(candles []market.Candle, strength, lookback int, tolPct float64, minTouches int) []Shelf {
	if minTouches < 2 {
		minTouches = 2
	}
	if tolPct <= 0 {
		return nil
	}
	pts := FindSwingPoints(candles, strength, lookback)
	if len(pts) < 2 {
		return nil
	}

	sorted := make([]SwingPoint, len(pts))
	copy(sorted, pts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Price < sorted[j].Price })

	var out []Shelf
	i := 0
	for i < len(sorted) {
		// Greedy over price-sorted pivots, anchored on the lowest unconsumed
		// one — same walk as clusterSwings, so the two layers agree about
		// where a cluster starts and stops.
		anchor := sorted[i].Price
		sh := Shelf{Lo: anchor, Hi: anchor, FirstIdx: sorted[i].Index, LastIdx: sorted[i].Index}
		sum := 0.0
		j := i
		for j < len(sorted) && (sorted[j].Price-anchor) <= anchor*tolPct {
			p := sorted[j]
			if p.Price < sh.Lo {
				sh.Lo = p.Price
			}
			if p.Price > sh.Hi {
				sh.Hi = p.Price
			}
			if p.Index < sh.FirstIdx {
				sh.FirstIdx = p.Index
			}
			if p.Index > sh.LastIdx {
				sh.LastIdx = p.Index
			}
			if p.IsTop {
				sh.Highs++
			} else {
				sh.Lows++
			}
			sum += p.Price
			j++
		}
		// BOTH sides required. Without this the output is just FindLiquidity
		// with the Kind field thrown away.
		if sh.Highs > 0 && sh.Lows > 0 && sh.Touches() >= minTouches {
			sh.Price = sum / float64(sh.Touches())
			out = append(out, sh)
		}
		i = j
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Price < out[b].Price })
	return out
}

// NearestShelves returns the closest shelf strictly above price and strictly
// below it, or nil. "Strictly" is measured against the band EDGE, not its
// midpoint: a shelf price sits inside is neither above nor below, and calling
// it either would put a target inside the level the price is already at.
func NearestShelves(shelves []Shelf, price float64) (above, below *Shelf) {
	for i := range shelves {
		s := &shelves[i]
		switch {
		case s.Lo > price:
			if above == nil || s.Lo < above.Lo {
				above = s
			}
		case s.Hi < price:
			if below == nil || s.Hi > below.Hi {
				below = s
			}
		}
	}
	return above, below
}

// TopShelves narrows a raw FindShelves result to the few worth drawing.
//
// Detection alone is not usable: on 500 bars of BTC 1h, minTouches=3 yields
// fifteen bands, including one at 63,060 (20% away, span 16 bars) and on ETH
// six clustered around 1,890 from months ago. Drawing all of them is worse
// than drawing none — the reader loses the two that matter.
//
// Deliberately NOT a weighted score. Invented weights are the thing that has
// to be A/B'd, and this layer is display-only, so selection is a hard
// proximity gate plus an explainable sort:
//
//  1. Drop anything further than maxDistPct from price. A band 20% away is
//     not support or resistance for the decision in front of you.
//  2. Rank by min(Highs, Lows) first. That is the "how two-sided is this"
//     measure, and two-sidedness is the whole reason a shelf is not a pool —
//     so H3/L3 outranks H5/L1 even on fewer total touches.
//  3. Then total touches, then span. Span breaks ties toward a band that has
//     mattered across time rather than through one stretch of chop.
//
// n <= 0 returns everything that survives the gate.
func TopShelves(shelves []Shelf, price, maxDistPct float64, n int) []Shelf {
	if price <= 0 || len(shelves) == 0 {
		return nil
	}
	var keep []Shelf
	for _, s := range shelves {
		// Distance is measured to the nearest EDGE, so a wide band the price
		// sits just outside is not penalised for its far side.
		d := 0.0
		switch {
		case price < s.Lo:
			d = (s.Lo - price) / price
		case price > s.Hi:
			d = (price - s.Hi) / price
		}
		if maxDistPct > 0 && d > maxDistPct {
			continue
		}
		keep = append(keep, s)
	}
	sort.SliceStable(keep, func(a, b int) bool {
		sa, sb := keep[a], keep[b]
		if x, y := minInt(sa.Highs, sa.Lows), minInt(sb.Highs, sb.Lows); x != y {
			return x > y
		}
		if sa.Touches() != sb.Touches() {
			return sa.Touches() > sb.Touches()
		}
		return sa.Span() > sb.Span()
	})
	if n > 0 && len(keep) > n {
		keep = keep[:n]
	}
	// Back to price order for drawing.
	sort.Slice(keep, func(a, b int) bool { return keep[a].Price < keep[b].Price })
	return keep
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
