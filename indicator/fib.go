package indicator

import "myFirstGo/trading/market"

// FibLevels are the canonical retracement ratios.
var FibLevels = []float64{0.236, 0.382, 0.5, 0.618, 0.786}

type FibLevel struct {
	Ratio float64
	Price float64
}

type FibRetracement struct {
	SwingHigh      float64
	SwingHighIndex int
	SwingLow       float64
	SwingLowIndex  int
	// Uptrend means the leg moves low → high (retracements pull back from high toward low).
	Uptrend bool
	Levels  []FibLevel
}

// FindSwing scans `lookback` candles and returns the highest high / lowest low.
// The direction is decided by which extreme is more recent.
func FindSwing(cs []market.Candle, lookback int) FibRetracement {
	if lookback <= 0 || lookback > len(cs) {
		lookback = len(cs)
	}
	if lookback == 0 {
		return FibRetracement{}
	}
	start := len(cs) - lookback
	hi, lo := cs[start].High, cs[start].Low
	hiIdx, loIdx := start, start
	for i := start + 1; i < len(cs); i++ {
		if cs[i].High > hi {
			hi, hiIdx = cs[i].High, i
		}
		if cs[i].Low < lo {
			lo, loIdx = cs[i].Low, i
		}
	}
	uptrend := hiIdx > loIdx
	return FibRetracement{
		SwingHigh:      hi,
		SwingHighIndex: hiIdx,
		SwingLow:       lo,
		SwingLowIndex:  loIdx,
		Uptrend:        uptrend,
		Levels:         buildLevels(hi, lo, uptrend),
	}
}

func buildLevels(hi, lo float64, uptrend bool) []FibLevel {
	span := hi - lo
	out := make([]FibLevel, len(FibLevels))
	for i, r := range FibLevels {
		var price float64
		if uptrend {
			price = hi - span*r
		} else {
			price = lo + span*r
		}
		out[i] = FibLevel{Ratio: r, Price: price}
	}
	return out
}
