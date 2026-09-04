package session

import (
	"math"
	"strings"
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// bar builds a candle spanning [startUTC, startUTC+d) with a range expressed
// as a percent of the open, so range assertions read directly.
func bar(startUTC time.Time, d time.Duration, open, rangePct float64) market.Candle {
	half := open * rangePct / 100 / 2
	return market.Candle{
		OpenTime:  startUTC,
		CloseTime: startUTC.Add(d),
		Open:      open,
		High:      open + half,
		Low:       open - half,
		Close:     open,
	}
}

// ---------------------------------------------------------------------
// The regression this package exists to prevent.
//
// 9:30 America/New_York is 13:30 UTC under EDT and 14:30 UTC under EST. Any
// implementation that keys the open bar on a fixed UTC or Taipei hour is right
// for eight months and wrong for four, starting on the first Sunday of
// November, with no error and no log line — the volume/range profile just
// quietly points at the wrong bar.
//
//	2026-09-04 (Fri, EDT)  9:30 ET = 13:30 UTC = 21:30 Taipei
//	2026-11-02 (Mon, EST)  9:30 ET = 14:30 UTC = 22:30 Taipei
// ---------------------------------------------------------------------

func TestContainsCashOpenSurvivesDST(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startUTC time.Time
		want     bool
	}{
		{"EDT: 13:00-14:00 UTC holds 13:30", time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC), true},
		{"EDT: 14:00-15:00 UTC does not", time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC), false},
		{"EST: 13:00-14:00 UTC does NOT (open moved to 14:30)", time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC), false},
		{"EST: 14:00-15:00 UTC holds 14:30", time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC), true},
	} {
		got := ContainsCashOpen(bar(tc.startUTC, time.Hour, 100, 1))
		if got != tc.want {
			t.Errorf("%s: ContainsCashOpen = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Stated as the invariant rather than the arithmetic: the SAME wall-clock
	// UTC hour changes membership across the boundary. A fixed-hour
	// implementation cannot satisfy both rows above at once.
	edt := ContainsCashOpen(bar(time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC), time.Hour, 100, 1))
	est := ContainsCashOpen(bar(time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC), time.Hour, 100, 1))
	if edt == est {
		t.Error("13:00 UTC must be the open bar under EDT and not under EST — a fixed-hour rule would return the same for both")
	}
}

// Half-open [OpenTime, CloseTime): a bar starting exactly at the open owns it,
// a bar ending exactly at the open does not. Otherwise two adjacent bars both
// claim the same open and the median is computed over double-counted samples.
func TestContainsCashOpenBoundaryIsHalfOpen(t *testing.T) {
	openUTC := time.Date(2026, 9, 4, 13, 30, 0, 0, time.UTC) // 9:30 EDT
	if !ContainsCashOpen(bar(openUTC, 15*time.Minute, 100, 1)) {
		t.Error("a bar starting exactly at the cash open must contain it")
	}
	if ContainsCashOpen(bar(openUTC.Add(-15*time.Minute), 15*time.Minute, 100, 1)) {
		t.Error("a bar ending exactly at the cash open must NOT contain it (half-open interval)")
	}
}

func TestContainsCashOpenRejectsDegenerateBars(t *testing.T) {
	openUTC := time.Date(2026, 9, 4, 13, 30, 0, 0, time.UTC)
	if ContainsCashOpen(market.Candle{CloseTime: openUTC.Add(time.Hour)}) {
		t.Error("zero OpenTime must not match")
	}
	if ContainsCashOpen(market.Candle{OpenTime: openUTC, CloseTime: openUTC}) {
		t.Error("zero-width bar must not match")
	}
}

// ---------------------------------------------------------------------
// MedianOpenBarRangePct
// ---------------------------------------------------------------------

// Five open bars with ranges 1..5% → median 3%. The non-open bars carry a 50%
// range specifically so that leaking one into the sample would be unmissable.
func TestMedianOpenBarRangePctIgnoresNonOpenBars(t *testing.T) {
	var cs []market.Candle
	for i, pct := range []float64{1, 2, 3, 4, 5} {
		day := time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC) // 9:00 EDT, holds 9:30
		cs = append(cs, bar(day, time.Hour, 100, pct))
		// same day, one hour later: not the open bar, wildly wider
		cs = append(cs, bar(day.Add(time.Hour), time.Hour, 100, 50))
	}
	pct, n := MedianOpenBarRangePct(cs)
	if n != 5 {
		t.Fatalf("sample count = %d, want 5 (the 50%%-range bars must be excluded)", n)
	}
	if math.Abs(pct-3.0) > 1e-9 {
		t.Errorf("median = %.6f, want 3.0", pct)
	}
}

func TestMedianOpenBarRangePctEvenSample(t *testing.T) {
	var cs []market.Candle
	for i, pct := range []float64{1, 2, 3, 4, 5, 6} {
		cs = append(cs, bar(time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC), time.Hour, 100, pct))
	}
	pct, n := MedianOpenBarRangePct(cs)
	if n != 6 {
		t.Fatalf("n = %d, want 6", n)
	}
	if math.Abs(pct-3.5) > 1e-9 {
		t.Errorf("median = %.6f, want 3.5 (mean of the two middles)", pct)
	}
}

// Below MinSamples the median is reported as 0 but the COUNT is still
// returned, so a caller can distinguish "no history yet" from "no risk". The
// warning path keys on the zero and stays silent.
func TestMedianOpenBarRangePctThinSampleReportsZeroButKeepsCount(t *testing.T) {
	var cs []market.Candle
	for i := range MinSamples - 1 {
		cs = append(cs, bar(time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC), time.Hour, 100, 3))
	}
	pct, n := MedianOpenBarRangePct(cs)
	if n != MinSamples-1 {
		t.Errorf("n = %d, want %d", n, MinSamples-1)
	}
	if pct != 0 {
		t.Errorf("median = %v, want 0 below MinSamples", pct)
	}
	if w := StopWarning(1000, 998, pct, n, time.Hour); w != "" {
		t.Errorf("must stay silent on a thin sample, got %q", w)
	}
}

// ---------------------------------------------------------------------
// NextCashOpen
// ---------------------------------------------------------------------

func TestNextCashOpen(t *testing.T) {
	// 2026-09-04 is a Friday in EDT; its open is 13:30 UTC.
	friBefore := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if got, want := NextCashOpen(friBefore).UTC(), time.Date(2026, 9, 4, 13, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("before Friday's open: got %s, want %s", got, want)
	}

	// After it, the next open skips Sat+Sun to Monday 2026-09-07 (still EDT).
	friAfter := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)
	if got, want := NextCashOpen(friAfter).UTC(), time.Date(2026, 9, 7, 13, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("after Friday's open: got %s, want %s (must skip the weekend)", got, want)
	}

	// Strictly after: standing exactly on the open returns the NEXT one, so a
	// caller polling at the open does not read untilOpen == 0 forever.
	at := time.Date(2026, 9, 4, 13, 30, 0, 0, time.UTC)
	if got := NextCashOpen(at); !got.After(at) {
		t.Errorf("NextCashOpen(%s) = %s, must be strictly after", at, got)
	}
}

// ---------------------------------------------------------------------
// StopWarning
// ---------------------------------------------------------------------

func TestStopWarningFiresInsideTheOpenBarRange(t *testing.T) {
	// entry 1000 / stop 998 → 0.2%; SNDK's measured open bar is 3.75%.
	w := StopWarning(1000, 998, 3.75, 12, 3*time.Hour)
	if w == "" {
		t.Fatal("a 0.2% stop against a 3.75% open bar must warn")
	}
	for _, want := range []string{"0.200%", "3.75%", "1/18.8", "n=12", "3h 0m"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}
}

// Long and short must read identically — the open bar travels both ways, and
// the guard is about distance, not direction.
func TestStopWarningIsDirectionSymmetric(t *testing.T) {
	below := StopWarning(1000, 998, 3.75, 12, time.Hour)
	above := StopWarning(1000, 1002, 3.75, 12, time.Hour)
	if below != above {
		t.Errorf("asymmetric:\n long: %s\nshort: %s", below, above)
	}
}

func TestStopWarningSilentWhenStopClearsTheOpenBar(t *testing.T) {
	// 4% stop vs a 3.75% open bar: wider than ordinary travel, nothing to say.
	if w := StopWarning(1000, 960, 3.75, 12, time.Hour); w != "" {
		t.Errorf("want silence, got %q", w)
	}
	// Exactly equal counts as clearing it — the guard flags strictly-inside.
	if w := StopWarning(1000, 962.5, 3.75, 12, time.Hour); w != "" {
		t.Errorf("a stop exactly at the median must not warn, got %q", w)
	}
}

func TestStopWarningSilentOnMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		entry, stop, medianPct float64
		samples                int
	}{
		{"no entry", 0, 998, 3.75, 12},
		{"no stop", 1000, 0, 3.75, 12},
		{"no baseline", 1000, 998, 0, 12},
		{"thin sample", 1000, 998, 3.75, MinSamples - 1},
	} {
		if w := StopWarning(tc.entry, tc.stop, tc.medianPct, tc.samples, time.Hour); w != "" {
			t.Errorf("%s: want silence, got %q", tc.name, w)
		}
	}
}

// An open already in progress is the most dangerous moment, so it must warn
// louder rather than fall through the "untilOpen > 0" branch into silence.
func TestStopWarningHandlesOpenAlreadyInProgress(t *testing.T) {
	w := StopWarning(1000, 998, 3.75, 12, 0)
	if w == "" {
		t.Fatal("must still warn when the open bar is in progress")
	}
	if !strings.Contains(w, "already in progress") {
		t.Errorf("should say the open is under way:\n%s", w)
	}
}

// ---------------------------------------------------------------------
// MaxLeverageForOpenBar — the number that decides whether holding through the
// open is possible at all.
// ---------------------------------------------------------------------

func TestMaxLeverageForOpenBar(t *testing.T) {
	const equity, margin, risk = 336.30, 75.0, 0.05
	for _, tc := range []struct {
		name      string
		medianPct float64
		want      float64
	}{
		// notional = 336.30*0.05 / (pct/100); lev = notional/75
		{"SNDK 3.75%", 3.75, 5.978667},
		{"NVDA 1.69%", 1.69, 13.266272},
	} {
		got := MaxLeverageForOpenBar(equity, margin, tc.medianPct, risk)
		if math.Abs(got-tc.want) > 1e-5 {
			t.Errorf("%s: got %.6f, want %.6f", tc.name, got, tc.want)
		}
	}

	// The point of the function, stated as an assertion: the leverage this
	// desk habitually uses is nowhere near admissible for a stock synthetic
	// held through the open.
	if lev := MaxLeverageForOpenBar(equity, margin, 3.75, risk); lev >= 20 {
		t.Errorf("SNDK cap came out %.1fx — expected well under 20x, or the arithmetic changed", lev)
	}

	for _, bad := range [][4]float64{
		{0, margin, 3.75, risk}, {equity, 0, 3.75, risk},
		{equity, margin, 0, risk}, {equity, margin, 3.75, 0},
	} {
		if got := MaxLeverageForOpenBar(bad[0], bad[1], bad[2], bad[3]); got != 0 {
			t.Errorf("MaxLeverageForOpenBar%v = %v, want 0", bad, got)
		}
	}
}
