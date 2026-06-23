package signal

import (
	"myFirstGo/trading-bot/market"
)

// TrendStructure classifies the recent price series by swing-point
// sequence. Sequence is "strong" when the last 3 swing highs AND the
// last 3 swing lows are both monotonic in the same direction (all
// strictly descending = downtrend, all strictly ascending = uptrend).
// Anything else — mixed, only 2 points, flat — is Neutral.
type TrendStructure int

const (
	StructNeutral TrendStructure = iota
	StructUptrend
	StructDowntrend
)

func (t TrendStructure) String() string {
	switch t {
	case StructUptrend:
		return "HH-HL uptrend"
	case StructDowntrend:
		return "LH-LL downtrend"
	}
	return "neutral"
}

// SwingPoint is a local high or low identified by the fractal rule:
// strictly more extreme than every bar in [i-strength, i+strength],
// excluding the candidate itself.
type SwingPoint struct {
	Index int     // candle index
	Price float64 // High for top, Low for bottom
	IsTop bool
}

// FindSwingPoints walks the candle slice and returns a chronological
// list of swing highs + lows using a fractal of `strength` bars on
// each side (default callers pass 2 — matches what the analyzer's
// double-pattern detector uses). Returns at most the last `cap` points
// each side merged in time order; pass 0 for no cap.
//
// Note: the last `strength` bars can never qualify as swings (no
// future bars to compare against). For live use this is fine — the
// most recent swing is always at least `strength` bars old.
func FindSwingPoints(candles []market.Candle, strength int, cap int) []SwingPoint {
	if strength < 1 {
		strength = 2
	}
	if len(candles) < 2*strength+1 {
		return nil
	}
	var pts []SwingPoint
	for i := strength; i < len(candles)-strength; i++ {
		hi, lo := candles[i].High, candles[i].Low
		isTop, isBot := true, true
		for j := i - strength; j <= i+strength; j++ {
			if j == i {
				continue
			}
			if candles[j].High >= hi {
				isTop = false
			}
			if candles[j].Low <= lo {
				isBot = false
			}
		}
		if isTop {
			pts = append(pts, SwingPoint{Index: i, Price: hi, IsTop: true})
		}
		if isBot {
			pts = append(pts, SwingPoint{Index: i, Price: lo, IsTop: false})
		}
	}
	if cap > 0 && len(pts) > 2*cap {
		// Keep last `cap` tops + last `cap` bottoms (interleaved by index).
		var tops, bots []SwingPoint
		for _, p := range pts {
			if p.IsTop {
				tops = append(tops, p)
			} else {
				bots = append(bots, p)
			}
		}
		if len(tops) > cap {
			tops = tops[len(tops)-cap:]
		}
		if len(bots) > cap {
			bots = bots[len(bots)-cap:]
		}
		merged := append(tops, bots...)
		// Sort by index ascending.
		for i := 1; i < len(merged); i++ {
			for j := i; j > 0 && merged[j].Index < merged[j-1].Index; j-- {
				merged[j], merged[j-1] = merged[j-1], merged[j]
			}
		}
		return merged
	}
	return pts
}

// ClassifyTrendStructure inspects the most recent swing series and
// returns Uptrend / Downtrend / Neutral.
//
// Rule: take the last 3 swing highs and last 3 swing lows (by index).
//   - All 3 highs strictly ascending AND all 3 lows strictly ascending
//     → Uptrend (HH-HL).
//   - All 3 highs strictly descending AND all 3 lows strictly
//     descending → Downtrend (LH-LL).
//   - Anything else → Neutral.
//
// Also returns the last 3 highs and lows for caller diagnostics
// (Reason annotation, etc.). Lengths < 3 → Neutral with empty slices.
func ClassifyTrendStructure(candles []market.Candle) (TrendStructure, []SwingPoint, []SwingPoint) {
	pts := FindSwingPoints(candles, 2, 0)
	var tops, bots []SwingPoint
	for _, p := range pts {
		if p.IsTop {
			tops = append(tops, p)
		} else {
			bots = append(bots, p)
		}
	}
	if len(tops) < 3 || len(bots) < 3 {
		return StructNeutral, tops, bots
	}
	t := tops[len(tops)-3:]
	b := bots[len(bots)-3:]

	ascTops := t[0].Price < t[1].Price && t[1].Price < t[2].Price
	descTops := t[0].Price > t[1].Price && t[1].Price > t[2].Price
	ascBots := b[0].Price < b[1].Price && b[1].Price < b[2].Price
	descBots := b[0].Price > b[1].Price && b[1].Price > b[2].Price

	if ascTops && ascBots {
		return StructUptrend, t, b
	}
	if descTops && descBots {
		return StructDowntrend, t, b
	}
	return StructNeutral, t, b
}
