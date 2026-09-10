package signal

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// 2026-09-10 is a Thursday, so the week boundary is Monday 2026-09-07 and the
// month boundary is 2026-09-01. Every expectation below is folded by hand from
// the fixture rather than read off an implementation run.
var periodNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func bar(y int, mo time.Month, d, h int, o, hi, lo float64) market.Candle {
	t := time.Date(y, mo, d, h, 0, 0, 0, time.UTC)
	return market.Candle{OpenTime: t, CloseTime: t.Add(time.Hour), Open: o, High: hi, Low: lo, Close: o}
}

func periodFixture() []market.Candle {
	return []market.Candle{
		bar(2026, 8, 31, 0, 100, 110, 90),  // previous month
		bar(2026, 9, 1, 0, 200, 260, 190),  // month opens here
		bar(2026, 9, 5, 0, 210, 300, 150),  // month extremes live here
		bar(2026, 9, 7, 0, 220, 240, 205),  // week opens here (Monday)
		bar(2026, 9, 9, 0, 230, 250, 180),  // week low
		bar(2026, 9, 10, 0, 240, 270, 235), // day opens here
		bar(2026, 9, 10, 6, 245, 280, 220), // day + week high, day low
	}
}

func TestComputePeriodLevels(t *testing.T) {
	got := ComputePeriodLevels(periodFixture(), periodNow)

	for _, c := range []struct {
		name string
		got  Period
		want Period
	}{
		// 09-10 00:00 and 06:00: open 240, high max(270,280), low min(235,220)
		{"Day", got.Day, Period{Open: 240, High: 280, Low: 220}},
		// from Monday 09-07: open 220, high max(240,250,270,280),
		// low min(205,180,235,220)
		{"Week", got.Week, Period{Open: 220, High: 280, Low: 180}},
		// from 09-01: open 200, high max(260,300,240,250,270,280),
		// low min(190,150,205,180,235,220) — the 08-31 bar must not count
		{"Month", got.Month, Period{Open: 200, High: 300, Low: 150}},
	} {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

// ComputeOpens now delegates here, so its three numbers must still be the
// three period opens and nothing else.
func TestComputeOpensStillMatchesPeriodOpens(t *testing.T) {
	cs := periodFixture()
	o := ComputeOpens(cs, periodNow)
	pl := ComputePeriodLevels(cs, periodNow)

	if o.Daily != pl.Day.Open || o.Weekly != pl.Week.Open || o.Monthly != pl.Month.Open {
		t.Fatalf("ComputeOpens = %+v, want opens from %+v", o, pl)
	}
	if o.Daily != 240 || o.Weekly != 220 || o.Monthly != 200 {
		t.Errorf("ComputeOpens = %+v, want {240 220 200}", o)
	}
}

// History that starts mid-period cannot report that period: the earliest bar
// held is not its open, and its extremes are not its extremes.
func TestComputePeriodLevelsRefusesPartialPeriods(t *testing.T) {
	cs := []market.Candle{
		bar(2026, 9, 9, 0, 230, 250, 180), // starts after both the week and month boundaries
		bar(2026, 9, 10, 0, 240, 270, 235),
		bar(2026, 9, 10, 6, 245, 280, 220),
	}
	got := ComputePeriodLevels(cs, periodNow)

	// The day is fully covered, so it still reports.
	if want := (Period{Open: 240, High: 280, Low: 220}); got.Day != want {
		t.Errorf("Day = %+v, want %+v", got.Day, want)
	}
	if got.Week != (Period{}) {
		t.Errorf("Week = %+v, want zero — history starts after Monday 09-07", got.Week)
	}
	if got.Month != (Period{}) {
		t.Errorf("Month = %+v, want zero — history starts after 09-01", got.Month)
	}
}

func TestComputePeriodLevelsEmpty(t *testing.T) {
	if got := ComputePeriodLevels(nil, periodNow); got != (PeriodLevels{}) {
		t.Errorf("ComputePeriodLevels(nil) = %+v, want zero", got)
	}
}

func TestPeriodBoundsWeekStartsMonday(t *testing.T) {
	// One case per weekday across a Monday-to-Sunday run, so an off-by-one
	// in the Sunday=0 shift cannot hide.
	for d, wantMonday := range map[int]int{7: 7, 8: 7, 9: 7, 10: 7, 11: 7, 12: 7, 13: 7, 14: 14} {
		now := time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC)
		day, week, month := periodBounds(now)
		if day.Day() != d || day.Hour() != 0 {
			t.Errorf("d=%d: day = %v, want %d 00:00", d, day, d)
		}
		if week.Day() != wantMonday || week.Weekday() != time.Monday {
			t.Errorf("d=%d (%s): week = %v, want 09-%02d (Monday)",
				d, now.Weekday(), week, wantMonday)
		}
		if month.Day() != 1 || month.Month() != time.September {
			t.Errorf("d=%d: month = %v, want 2026-09-01", d, month)
		}
	}
}
