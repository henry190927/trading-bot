package signal

import (
	"time"

	"github.com/henry190927/trading-bot/market"
)

// Opens reports the most recent daily, weekly, and monthly opening prices
// derived from the candle series. Returns zero values for any timeframe
// where insufficient history is available.
//
// "Open" means the open price of the first candle that started at or after
// the most recent UTC boundary (midnight for daily, Monday 00:00 for weekly,
// 1st-of-month 00:00 for monthly). Institutional desks reference these as
// intraday support/resistance.
type Opens struct {
	Daily   float64
	Weekly  float64
	Monthly float64
}

// ComputeOpens scans candles (must be sorted ascending by OpenTime) and
// finds the open of the most recent day/week/month start.
//
// Note: candles must span at least the relevant period — e.g. weekly open
// requires history back to the most recent Monday 00:00 UTC.
func ComputeOpens(candles []market.Candle, now time.Time) Opens {
	// Delegates to ComputePeriodLevels so the day/week/month boundary
	// arithmetic exists once (signal/periods.go). Behaviour is unchanged:
	// Period.Open is the open of the first candle at or after the boundary,
	// and zero when history starts mid-period — same as findOpenAtOrAfter,
	// which several backtests still call directly.
	pl := ComputePeriodLevels(candles, now)
	return Opens{Daily: pl.Day.Open, Weekly: pl.Week.Open, Monthly: pl.Month.Open}
}

// findOpenAtOrAfter returns the Open of the first candle at/after the
// boundary. Returns 0 if our data starts after the boundary (we'd be
// mistakenly treating our slice's earliest candle as the boundary open).
func findOpenAtOrAfter(candles []market.Candle, boundary time.Time) float64 {
	if len(candles) == 0 || candles[0].OpenTime.After(boundary) {
		return 0
	}
	for _, c := range candles {
		if !c.OpenTime.Before(boundary) {
			return c.Open
		}
	}
	return 0
}
