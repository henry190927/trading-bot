package signal

import (
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// Bias returns the directional bias implied by the MACD(12,26,9) histogram
// at the most recent candle. Positive histogram = Long bias, negative = Short.
// Returns Flat if there's insufficient data.
//
// Intended use: compute Bias from a higher-timeframe candle series, then
// pass it through Inputs.Bias on the base timeframe. The engine suppresses
// signals that fight the bias.
func Bias(candles []market.Candle) Side {
	if len(candles) < 35 {
		return Flat
	}
	closes := market.Closes(candles)
	m := indicator.MACD(closes, 12, 26, 9)
	h := m[len(m)-1].Histogram
	switch {
	case h > 0:
		return Long
	case h < 0:
		return Short
	}
	return Flat
}

// DefaultBiasTF picks a sensible higher timeframe for MTF filtering given
// the base trading-bot timeframe.
func DefaultBiasTF(base market.Timeframe) market.Timeframe {
	switch base {
	case market.TF1m:
		return market.TF5m
	case market.TF5m, market.TF15m:
		return market.TF1h
	case market.TF30m:
		return market.TF4h
	case market.TF1h:
		return market.TF4h
	case market.TF2h:
		return market.TF1d
	case market.TF4h:
		return market.TF1d
	}
	return market.TF4h
}
