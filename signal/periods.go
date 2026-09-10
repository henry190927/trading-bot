package signal

import (
	"time"

	"myFirstGo/trading-bot/market"
)

// Period is one calendar period's open, high and low.
type Period struct {
	Open float64
	High float64
	Low  float64
}

// PeriodLevels carries the current day's, week's and month's open/high/low.
//
// The opens were already here (see Opens); the highs and lows were not, and
// nothing in the tree computed them — a grep for DailyHigh / PrevDayHigh /
// PDH / PDL returned nothing. They are the other half of the reference set
// desks actually quote: "today's high" and "this week's low" mark where the
// period's extreme buyers and sellers were, which an open cannot say.
//
// Not to be confused with the exchange's rolling 24h high/low (BingX
// /quote/ticker highPrice / lowPrice). That window slides with the clock;
// these snap to UTC calendar boundaries, the same ones the opens use, so the
// numbers move only when a new period starts.
type PeriodLevels struct {
	Day   Period
	Week  Period
	Month Period
}

// periodBounds returns the UTC start of the current day, week (Monday) and
// month. Extracted so ComputeOpens and ComputePeriodLevels cannot drift: two
// copies of a week-boundary calculation is one copy too many.
func periodBounds(now time.Time) (day, week, month time.Time) {
	n := now.UTC()
	day = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	// time.Weekday is Sunday=0..Saturday=6; shift so Monday is 0.
	daysSinceMonday := (int(n.Weekday()) + 6) % 7
	week = day.AddDate(0, 0, -daysSinceMonday)
	month = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	return day, week, month
}

// ComputePeriodLevels scans candles (sorted ascending by OpenTime) and
// aggregates each period from its UTC boundary to the end of the slice.
//
// Whether "today's high" includes the bar still forming is the caller's
// choice, because it depends on what the number is for: a displayed level
// should include it (that IS the day's high as a chart shows it), while
// anything feeding a backtest must not, or it reads the future. This function
// aggregates exactly the slice it is given and makes no such decision itself.
func ComputePeriodLevels(candles []market.Candle, now time.Time) PeriodLevels {
	if len(candles) == 0 {
		return PeriodLevels{}
	}
	day, week, month := periodBounds(now)
	return PeriodLevels{
		Day:   aggregateFrom(candles, day),
		Week:  aggregateFrom(candles, week),
		Month: aggregateFrom(candles, month),
	}
}

// aggregateFrom folds every candle at or after boundary into one Period.
//
// Returns a zero Period when the slice begins after the boundary, matching
// findOpenAtOrAfter: if history starts mid-period, the earliest candle we
// hold is not the period's open and its extremes are not the period's
// extremes. Reporting a partial period as a whole one would put a level on
// the chart that never existed.
func aggregateFrom(candles []market.Candle, boundary time.Time) Period {
	if len(candles) == 0 || candles[0].OpenTime.After(boundary) {
		return Period{}
	}
	var p Period
	seen := false
	for _, c := range candles {
		if c.OpenTime.Before(boundary) {
			continue
		}
		if !seen {
			seen = true
			p.Open, p.High, p.Low = c.Open, c.High, c.Low
			continue
		}
		if c.High > p.High {
			p.High = c.High
		}
		if c.Low < p.Low {
			p.Low = c.Low
		}
	}
	return p
}
