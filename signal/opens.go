package signal

import (
	"time"

	"myFirstGo/trading-bot/market"
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
	if len(candles) == 0 {
		return Opens{}
	}
	nowUTC := now.UTC()

	dayStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
	// Week boundary: most recent Monday 00:00 UTC. time.Weekday: Sunday=0, Monday=1, …
	weekday := int(nowUTC.Weekday()) // 0..6, Sun..Sat
	daysSinceMonday := (weekday + 6) % 7
	weekStart := dayStart.AddDate(0, 0, -daysSinceMonday)
	monthStart := time.Date(nowUTC.Year(), nowUTC.Month(), 1, 0, 0, 0, 0, time.UTC)

	out := Opens{}
	out.Daily = findOpenAtOrAfter(candles, dayStart)
	out.Weekly = findOpenAtOrAfter(candles, weekStart)
	out.Monthly = findOpenAtOrAfter(candles, monthStart)
	return out
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
