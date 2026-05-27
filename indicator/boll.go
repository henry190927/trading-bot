package indicator

import "math"

type BollBand struct {
	Mid   float64
	Upper float64
	Lower float64
}

// Bollinger returns SMA(period) ± k*stdev. Default params: period=20, k=2.
func Bollinger(closes []float64, period int, k float64) []BollBand {
	out := make([]BollBand, len(closes))
	if period <= 0 || len(closes) < period {
		return out
	}
	for i := period - 1; i < len(closes); i++ {
		window := closes[i-period+1 : i+1]
		mean := avg(window)
		sd := stdev(window, mean)
		out[i] = BollBand{
			Mid:   mean,
			Upper: mean + k*sd,
			Lower: mean - k*sd,
		}
	}
	return out
}

func avg(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stdev(xs []float64, mean float64) float64 {
	var s float64
	for _, x := range xs {
		d := x - mean
		s += d * d
	}
	return math.Sqrt(s / float64(len(xs)))
}
