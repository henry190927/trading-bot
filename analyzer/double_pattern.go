package analyzer

import (
	"math"

	"myFirstGo/trading-bot/market"
)

// DoubleSide tells whether a pattern is a double top (bearish) or bottom (bullish).
type DoubleSide int

const (
	DoubleTop DoubleSide = iota
	DoubleBottom
)

// DoublePattern is a matched double-top or double-bottom: two pivot extremes
// within a price tolerance, separated by enough bars, with an intermediate
// extreme (valley for tops, peak for bottoms) deep enough to count.
type DoublePattern struct {
	Side      DoubleSide
	Level     float64 // mean of the two pivot extremes
	PivotAIdx int     // earlier pivot
	PivotBIdx int     // later pivot
	ValleyExt float64 // intermediate extreme between the pivots
}

// DetectDoublePatterns scans the last `lookback` candles for double tops and
// bottoms. A pattern is included only if its second pivot is in the most
// recent `recency` bars — older patterns are usually already faded.
//
//	pivotWidth     bars on each side that must be lower (highs) / higher (lows)
//	               to confirm a pivot. Typical: 2.
//	minSep         minimum bars between the two pivots. Typical: 5.
//	recency        PivotB must be within this many bars of `len(cs)-1`. Typical: 5.
//	priceTolerance fractional max distance between the two pivot prices. Typical: 0.003.
//	valleyDepth    fractional minimum distance from the pivot level to the
//	               intermediate extreme. Typical: 0.01. Without this, "double"
//	               is just two adjacent bars at the same level.
func DetectDoublePatterns(
	cs []market.Candle,
	lookback, pivotWidth, minSep, recency int,
	priceTolerance, valleyDepth float64,
) []DoublePattern {
	if len(cs) < lookback || pivotWidth < 1 {
		return nil
	}
	start := len(cs) - lookback

	var highs, lows []int
	for i := start + pivotWidth; i < len(cs)-pivotWidth; i++ {
		isHigh, isLow := true, true
		for j := 1; j <= pivotWidth; j++ {
			if cs[i-j].High >= cs[i].High || cs[i+j].High >= cs[i].High {
				isHigh = false
			}
			if cs[i-j].Low <= cs[i].Low || cs[i+j].Low <= cs[i].Low {
				isLow = false
			}
			if !isHigh && !isLow {
				break
			}
		}
		if isHigh {
			highs = append(highs, i)
		}
		if isLow {
			lows = append(lows, i)
		}
	}

	var out []DoublePattern
	recencyStart := len(cs) - recency

	// Double tops
	for i := 0; i < len(highs); i++ {
		for j := i + 1; j < len(highs); j++ {
			a, b := highs[i], highs[j]
			if b-a < minSep || b < recencyStart {
				continue
			}
			pa, pb := cs[a].High, cs[b].High
			if pa == 0 || math.Abs(pa-pb)/pa > priceTolerance {
				continue
			}
			valley := cs[a+1].Low
			for k := a + 1; k < b; k++ {
				if cs[k].Low < valley {
					valley = cs[k].Low
				}
			}
			mid := (pa + pb) / 2
			if mid == 0 || (mid-valley)/mid < valleyDepth {
				continue
			}
			out = append(out, DoublePattern{
				Side: DoubleTop, Level: mid,
				PivotAIdx: a, PivotBIdx: b, ValleyExt: valley,
			})
		}
	}

	// Double bottoms
	for i := 0; i < len(lows); i++ {
		for j := i + 1; j < len(lows); j++ {
			a, b := lows[i], lows[j]
			if b-a < minSep || b < recencyStart {
				continue
			}
			pa, pb := cs[a].Low, cs[b].Low
			if pa == 0 || math.Abs(pa-pb)/pa > priceTolerance {
				continue
			}
			peak := cs[a+1].High
			for k := a + 1; k < b; k++ {
				if cs[k].High > peak {
					peak = cs[k].High
				}
			}
			mid := (pa + pb) / 2
			if mid == 0 || (peak-mid)/mid < valleyDepth {
				continue
			}
			out = append(out, DoublePattern{
				Side: DoubleBottom, Level: mid,
				PivotAIdx: a, PivotBIdx: b, ValleyExt: peak,
			})
		}
	}

	return out
}
