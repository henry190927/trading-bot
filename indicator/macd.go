package indicator

type MACDPoint struct {
	MACD      float64
	Signal    float64
	Histogram float64
}

// MACD with default params 12/26/9. Returns slice aligned to closes;
// values before the slow EMA warms up are zero.
func MACD(closes []float64, fast, slow, signal int) []MACDPoint {
	out := make([]MACDPoint, len(closes))
	if slow <= 0 || fast <= 0 || signal <= 0 || len(closes) < slow {
		return out
	}
	emaFast := EMA(closes, fast)
	emaSlow := EMA(closes, slow)
	macdLine := make([]float64, len(closes))
	for i := range closes {
		macdLine[i] = emaFast[i] - emaSlow[i]
	}
	// Signal EMA starts from index slow-1 (where MACD line is meaningful).
	start := slow - 1
	if start >= len(closes) {
		return out
	}
	sig := EMA(macdLine[start:], signal)
	for i := range sig {
		idx := start + i
		out[idx] = MACDPoint{
			MACD:      macdLine[idx],
			Signal:    sig[i],
			Histogram: macdLine[idx] - sig[i],
		}
	}
	return out
}

// EMA seeded with SMA of the first `period` values.
func EMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 0 || len(values) < period {
		return out
	}
	k := 2.0 / float64(period+1)
	var seed float64
	for i := 0; i < period; i++ {
		seed += values[i]
	}
	seed /= float64(period)
	out[period-1] = seed
	for i := period; i < len(values); i++ {
		out[i] = values[i]*k + out[i-1]*(1-k)
	}
	return out
}
