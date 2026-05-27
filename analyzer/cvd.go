package analyzer

import "myFirstGo/trading-bot/market"

type CVDPoint struct {
	Time  int64
	Value float64
}

// CVD returns cumulative volume delta over a stream of trades.
// Taker buy (BuyerMaker=false) adds quantity; taker sell subtracts.
func CVD(trades []market.Trade) []CVDPoint {
	out := make([]CVDPoint, len(trades))
	var cum float64
	for i, t := range trades {
		if t.BuyerMaker {
			cum -= t.Quantity
		} else {
			cum += t.Quantity
		}
		out[i] = CVDPoint{Time: t.Time.UnixMilli(), Value: cum}
	}
	return out
}

// CVDByCandle buckets trades into the candle window they fall in and returns
// one CVD value per candle (close-of-bar). Trades outside any candle are dropped.
func CVDByCandle(candles []market.Candle, trades []market.Trade) []float64 {
	out := make([]float64, len(candles))
	if len(candles) == 0 || len(trades) == 0 {
		return out
	}
	var cum float64
	ti := 0
	for ci, c := range candles {
		for ti < len(trades) && !trades[ti].Time.After(c.CloseTime) {
			if trades[ti].Time.Before(c.OpenTime) {
				ti++
				continue
			}
			if trades[ti].BuyerMaker {
				cum -= trades[ti].Quantity
			} else {
				cum += trades[ti].Quantity
			}
			ti++
		}
		out[ci] = cum
	}
	return out
}
