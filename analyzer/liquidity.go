package analyzer

import (
	"math"

	"myFirstGo/trading/market"
)

type SweepSide int

const (
	SweepHigh SweepSide = iota // liquidity above equal highs taken (bearish reversal candidate)
	SweepLow                   // liquidity below equal lows taken (bullish reversal candidate)
)

type LiquiditySweep struct {
	Side      SweepSide
	Level     float64 // the equal high/low price that was swept
	SweepIdx  int     // candle index where wick pierced the level
	ReclaimOK bool    // close returned back through the level (confirmed sweep)
}

// FindEqualLevels groups recent swing highs/lows that cluster within `tolerance`
// (fractional, e.g. 0.0005 = 5bps). Returns the cluster level (mean) for each
// group with at least `minTouches` touches.
func FindEqualLevels(values []float64, tolerance float64, minTouches int) []float64 {
	if len(values) == 0 || minTouches < 2 {
		return nil
	}
	used := make([]bool, len(values))
	var levels []float64
	for i := 0; i < len(values); i++ {
		if used[i] {
			continue
		}
		group := []float64{values[i]}
		used[i] = true
		for j := i + 1; j < len(values); j++ {
			if used[j] {
				continue
			}
			if math.Abs(values[j]-values[i])/values[i] <= tolerance {
				group = append(group, values[j])
				used[j] = true
			}
		}
		if len(group) >= minTouches {
			var sum float64
			for _, v := range group {
				sum += v
			}
			levels = append(levels, sum/float64(len(group)))
		}
	}
	return levels
}

// DetectSweeps walks `cs` and flags candles whose wick pierced an equal-highs
// or equal-lows level and then closed back inside (the classic liquidity grab).
//
// A sweep is only returned if it remains "valid" — meaning no later candle
// closed back through the swept level in the wrong direction. A bullish
// sweep-low at L is invalidated by any later close < L (price ultimately
// broke down); a bearish sweep-high at L is invalidated by any later close > L
// (price ultimately broke up). This prevents stale sweeps from continuing to
// fire confluence votes after structure has already changed.
func DetectSweeps(cs []market.Candle, tolerance float64, minTouches int) []LiquiditySweep {
	if len(cs) < minTouches+1 {
		return nil
	}
	highs := market.Highs(cs[:len(cs)-1])
	lows := market.Lows(cs[:len(cs)-1])
	eqHighs := FindEqualLevels(highs, tolerance, minTouches)
	eqLows := FindEqualLevels(lows, tolerance, minTouches)

	var out []LiquiditySweep
	for i, c := range cs {
		for _, lvl := range eqHighs {
			if c.High > lvl && c.Close < lvl {
				out = append(out, LiquiditySweep{Side: SweepHigh, Level: lvl, SweepIdx: i, ReclaimOK: true})
			}
		}
		for _, lvl := range eqLows {
			if c.Low < lvl && c.Close > lvl {
				out = append(out, LiquiditySweep{Side: SweepLow, Level: lvl, SweepIdx: i, ReclaimOK: true})
			}
		}
	}

	// Post-detection invalidation: a sweep is dead the moment a later candle
	// closes through it in the wrong direction.
	for i := range out {
		sw := &out[i]
		for j := sw.SweepIdx + 1; j < len(cs); j++ {
			if (sw.Side == SweepLow && cs[j].Close < sw.Level) ||
				(sw.Side == SweepHigh && cs[j].Close > sw.Level) {
				sw.ReclaimOK = false
				break
			}
		}
	}
	valid := out[:0]
	for _, sw := range out {
		if sw.ReclaimOK {
			valid = append(valid, sw)
		}
	}
	return valid
}
