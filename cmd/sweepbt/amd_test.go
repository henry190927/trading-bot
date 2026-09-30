package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// hourly builds one bar per hour from a UTC start, with the shape given.
func hourly(start time.Time, n int, shape func(i int) (h, l float64)) []market.Candle {
	var cs []market.Candle
	for i := 0; i < n; i++ {
		o := start.Add(time.Duration(i) * time.Hour)
		h, l := shape(i)
		cs = append(cs, market.Candle{OpenTime: o, CloseTime: o.Add(time.Hour),
			Open: (h + l) / 2, High: h, Low: l, Close: (h + l) / 2})
	}
	return cs
}

func amdDay() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) }

// A range that is still forming is not a range. Judging a "sweep" against a
// boundary that is still moving is look-ahead's quieter cousin: the bar that
// extends the range would score as the bar that broke it.
func TestAsianRangeIsNotReadableUntilTheSessionCloses(t *testing.T) {
	cs := hourly(amdDay(), 24, func(i int) (float64, float64) { return 100 + float64(i), 90 + float64(i) })
	for i := 0; i < 7; i++ { // 00:00–06:00 UTC, inside the session
		if _, _, ok := asianRange(cs, i); ok {
			t.Errorf("bar %d (%02d:00 UTC) is inside the Asian session and must not yield a range",
				i, cs[i].OpenTime.UTC().Hour())
		}
	}
	if _, _, ok := asianRange(cs, 7); !ok { // 07:00, the session has closed
		t.Error("the range must be readable once 07:00 UTC has arrived")
	}
}

// The range is the session's own extremes and nothing else — not the day's,
// not a rolling window's.
func TestAsianRangeUsesOnlyItsOwnSessionBars(t *testing.T) {
	// Asia (00–06) spans 90..106. London onward spikes far outside it.
	cs := hourly(amdDay(), 24, func(i int) (float64, float64) {
		if i < 7 {
			return 100 + float64(i), 90 + float64(i)
		}
		return 500, 10 // a spike that must NOT widen the range
	})
	hi, lo, ok := asianRange(cs, 12)
	if !ok {
		t.Fatal("range should be readable at 12:00 UTC")
	}
	if hi != 106 || lo != 90 {
		t.Errorf("range = %v–%v, want 90–106 (the 00:00–06:00 bars only)", lo, hi)
	}
}

// Yesterday's session must not leak into today's range.
func TestAsianRangeDoesNotCrossTheDayBoundary(t *testing.T) {
	// Start at 00:00 on day 1 with a WIDE session, then a narrow one on day 2.
	cs := hourly(amdDay(), 48, func(i int) (float64, float64) {
		day2 := i >= 24
		switch {
		case !day2 && i < 7:
			return 200, 50 // day 1 Asia: very wide
		case day2 && i-24 < 7:
			return 101, 99 // day 2 Asia: narrow
		}
		return 100.5, 99.5
	})
	hi, lo, ok := asianRange(cs, 24+8) // day 2, 08:00 UTC
	if !ok {
		t.Fatal("day 2 range should be readable")
	}
	if hi != 101 || lo != 99 {
		t.Errorf("range = %v–%v, want 99–101 — day 1's session leaked in", lo, hi)
	}
}

func TestSweepWindowExcludesAsiaAndLateNY(t *testing.T) {
	cs := hourly(amdDay(), 24, func(i int) (float64, float64) { return 100, 99 })
	for _, h := range []int{0, 3, 6} {
		if inSweepWindow(cs[h]) {
			t.Errorf("%02d:00 UTC is inside the Asian session and must not accept a sweep", h)
		}
	}
	for _, h := range []int{7, 12, 19} {
		if !inSweepWindow(cs[h]) {
			t.Errorf("%02d:00 UTC is inside London→NY and must accept a sweep", h)
		}
	}
	for _, h := range []int{20, 23} {
		if inSweepWindow(cs[h]) {
			t.Errorf("%02d:00 UTC is past the window and must not accept a sweep", h)
		}
	}
}

// A fire must be a sweep-and-RECLAIM, not merely a break. A bar that closes
// beyond the boundary is an expansion, and taking it would invert the strategy.
func TestAMDFiresOnReclaimNotOnBreak(t *testing.T) {
	mk := func(londonHigh, londonClose float64) []market.Candle {
		return hourly(amdDay().Add(-72*time.Hour), 96, func(i int) (float64, float64) {
			h := (i % 24)
			switch {
			case h < 7:
				return 101, 99 // Asia: 99–101 every day
			case h == 9:
				return londonHigh, 99.5
			}
			return 100.5, 99.5
		})
	}
	atr := make([]float64, 96)
	for i := range atr {
		atr[i] = 1
	}

	// Pokes 103 and closes back at 99.5 → reclaim → short.
	cs := mk(103, 99.5)
	for i := range cs {
		if cs[i].OpenTime.UTC().Hour() == 9 {
			cs[i].Close = 99.5
		}
	}
	if got := genAMDFires(cs, atr, "T", 0.15, 2.0, false); len(got) == 0 {
		t.Fatal("a sweep of the range high that closes back inside must fire")
	} else if got[0].Side != "short" {
		t.Errorf("side = %s, want short", got[0].Side)
	}

	// Same poke but the bar CLOSES above the boundary → a break, not a trap.
	cs2 := mk(103, 102.5)
	for i := range cs2 {
		if cs2[i].OpenTime.UTC().Hour() == 9 {
			cs2[i].Close = 102.5
		}
	}
	if got := genAMDFires(cs2, atr, "T", 0.15, 2.0, false); len(got) != 0 {
		t.Errorf("closing beyond the boundary is an expansion, not a manipulation — fired %d time(s)", len(got))
	}
}
