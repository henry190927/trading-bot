package market

import "time"

type Candle struct {
	OpenTime  time.Time
	CloseTime time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	// QuoteVolume in USDT; useful when comparing across symbols of different price magnitudes.
	QuoteVolume float64
}

type Trade struct {
	Time     time.Time
	Price    float64
	Quantity float64
	// BuyerMaker: true means the aggressor was a seller (taker sold into a bid).
	// CVD treats taker-buy as +volume and taker-sell as -volume.
	BuyerMaker bool
}

type DepthLevel struct {
	Price    float64
	Quantity float64
}

type Depth struct {
	Time time.Time
	Bids []DepthLevel
	Asks []DepthLevel
}

func Closes(cs []Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

func Highs(cs []Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.High
	}
	return out
}

func Lows(cs []Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Low
	}
	return out
}
