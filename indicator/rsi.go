package indicator

// RSI computes Wilder's Relative Strength Index over `period` (typically 14).
// Returns a slice of the same length as `closes`; values before index `period`
// are 0 (insufficient data).
func RSI(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	if len(closes) <= period || period <= 0 {
		return out
	}

	var gainSum, lossSum float64
	for i := 1; i <= period; i++ {
		ch := closes[i] - closes[i-1]
		if ch >= 0 {
			gainSum += ch
		} else {
			lossSum -= ch
		}
	}
	avgGain := gainSum / float64(period)
	avgLoss := lossSum / float64(period)
	out[period] = rsiFromAvgs(avgGain, avgLoss)

	for i := period + 1; i < len(closes); i++ {
		ch := closes[i] - closes[i-1]
		gain, loss := 0.0, 0.0
		if ch >= 0 {
			gain = ch
		} else {
			loss = -ch
		}
		// Wilder's smoothing
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		out[i] = rsiFromAvgs(avgGain, avgLoss)
	}
	return out
}

func rsiFromAvgs(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}
