package indicator

import (
	"math"

	"github.com/henry190927/trading-bot/market"
)

// ATR returns Wilder's Average True Range. Standard period is 14.
// True Range = max(high-low, |high-prevClose|, |low-prevClose|).
// Values before index `period` are 0.
func ATR(cs []market.Candle, period int) []float64 {
	out := make([]float64, len(cs))
	if period <= 0 || len(cs) <= period {
		return out
	}
	tr := make([]float64, len(cs))
	tr[0] = cs[0].High - cs[0].Low
	for i := 1; i < len(cs); i++ {
		hl := cs[i].High - cs[i].Low
		hc := math.Abs(cs[i].High - cs[i-1].Close)
		lc := math.Abs(cs[i].Low - cs[i-1].Close)
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}
	// Seed with simple average of the first `period` TRs.
	var seed float64
	for i := 1; i <= period; i++ {
		seed += tr[i]
	}
	out[period] = seed / float64(period)
	for i := period + 1; i < len(cs); i++ {
		out[i] = (out[i-1]*float64(period-1) + tr[i]) / float64(period)
	}
	return out
}
