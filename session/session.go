// Package session models the US cash-session calendar for the stock-synthetic
// perps (NCSK*), whose risk is concentrated in one bar per day.
//
// WHY THIS EXISTS: measured on 2026-09-04 over ~148 days of BingX 1h history,
// the bar containing the 9:30 ET cash open carries 2.5-7.25x the symbol's
// median hourly range, and sets the day's high or low on 25-58% of days —
// from one bar out of twenty-four. Everything else about a stock synthetic
// trades like a quiet crypto pair; that bar does not.
//
//	median cash-open-bar range   SNDK 3.75%  MSTR 2.60%  APP 2.38%  SPCX 2.26%  NVDA 1.69%
//	day's extreme in that bar    APP 58%  SPCX 50%  SNDK 42%  NVDA 25%  MSTR 25%
//
// The immediate consequence is a sizing fact, not a strategy opinion: a stop
// narrower than that bar's ordinary range is not a stop, because ordinary
// noise reaches it. SNDK on 2026-09-03 was stopped at 1,526.91 by a bar that
// wicked to 1,515.04 — 11.87 points, while SNDK's cash-open bar routinely
// travels ~58. At 100x that account could only afford a 0.224% stop, one
// SEVENTEENTH of the bar's median range.
package session

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	// Embed the IANA database rather than trusting the host's. The whole
	// point of this package is that the cash open is 9:30 *America/New_York*,
	// which is 21:30 Taipei under EDT and 22:30 under EST — a hardcoded hour,
	// or a missing tzdata falling back to UTC, breaks silently on the first
	// Sunday of November and stays broken until March.
	_ "time/tzdata"

	"myFirstGo/trading-bot/market"
)

// NYSE regular-hours open, in exchange-local time.
const (
	cashOpenHour = 9
	cashOpenMin  = 30
)

// OpenBarLookbackBars is how many 1h bars a caller should fetch before asking
// for MedianOpenBarRangePct. ~400 bars is ~17 calendar days, which after
// weekends leaves ~12 cash-open bars — comfortably above MinSamples without
// asking the exchange for a long history on every render.
//
// Exported so every surface that shows this warning samples the SAME window.
// The web guard and cmd/validate both compute their own baseline; if they
// disagreed on the lookback they would report different medians for the same
// symbol at the same moment, which is the display-vs-reality shape this
// package's warning exists to remove.
const OpenBarLookbackBars = 400

// MinSamples is the smallest cash-open-bar sample a median is reported for.
// Below this the median is one or two prints and warning on it would be
// noise dressed as a measurement.
const MinSamples = 5

var (
	nyOnce sync.Once
	nyZone *time.Location
)

// NY returns the exchange location. Falls back to a fixed -05:00 only if the
// embedded database somehow fails to load, which would make the open-bar
// detection wrong by an hour for half the year — so the fallback is a
// last-resort that keeps callers running, not a supported mode.
func NY() *time.Location {
	nyOnce.Do(func() {
		if loc, err := time.LoadLocation("America/New_York"); err == nil {
			nyZone = loc
			return
		}
		nyZone = time.FixedZone("EST", -5*60*60)
	})
	return nyZone
}

// CashOpen returns 9:30 ET on the exchange-local calendar date of ref.
func CashOpen(ref time.Time) time.Time {
	n := ref.In(NY())
	return time.Date(n.Year(), n.Month(), n.Day(), cashOpenHour, cashOpenMin, 0, 0, NY())
}

// IsWeekend reports whether t falls on a Saturday or Sunday in exchange-local
// time. Market holidays are deliberately NOT modelled: an unmodelled holiday
// makes a warning conservative (it names an open that will not happen), never
// permissive, and a holiday calendar is one more thing to go stale.
func IsWeekend(t time.Time) bool {
	switch t.In(NY()).Weekday() {
	case time.Saturday, time.Sunday:
		return true
	}
	return false
}

// NextCashOpen returns the first 9:30 ET strictly after now, skipping weekends.
func NextCashOpen(now time.Time) time.Time {
	o := CashOpen(now)
	for !o.After(now) || IsWeekend(o) {
		o = CashOpen(o.AddDate(0, 0, 1))
	}
	return o
}

// ContainsCashOpen reports whether the bar's [OpenTime, CloseTime) window
// contains that day's cash open.
//
// Derived from the bar's OWN timestamps instead of a hardcoded hour, which
// makes it correct for any timeframe the exchange serves and for any bar
// alignment, and immune to DST: the 1h bar holding 9:30 ET is 21:00 Taipei in
// summer and 22:00 in winter, and this function does not need to know that.
// A bar straddling exchange-local midnight is tested against both dates it
// touches.
func ContainsCashOpen(c market.Candle) bool {
	if c.OpenTime.IsZero() || !c.CloseTime.After(c.OpenTime) {
		return false
	}
	for _, ref := range []time.Time{c.OpenTime, c.CloseTime.Add(-time.Nanosecond)} {
		o := CashOpen(ref)
		if !o.Before(c.OpenTime) && o.Before(c.CloseTime) {
			return true
		}
	}
	return false
}

// MedianOpenBarRangePct returns the median (High-Low)/Open as a percent over
// the cash-open bars in candles, and how many there were.
//
// Median, not mean: one earnings gap should not redefine what an ordinary open
// looks like. Returns (0, n) when n < MinSamples so callers cannot accidentally
// act on a one-sample "median" — the count is still reported so a UI can say
// "not enough history" rather than "no risk".
func MedianOpenBarRangePct(candles []market.Candle) (pct float64, n int) {
	var xs []float64
	for _, c := range candles {
		if !ContainsCashOpen(c) || c.Open <= 0 || c.High < c.Low {
			continue
		}
		xs = append(xs, (c.High-c.Low)/c.Open*100)
	}
	if len(xs) < MinSamples {
		return 0, len(xs)
	}
	sort.Float64s(xs)
	return medianSorted(xs), len(xs)
}

func medianSorted(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// MaxLeverageForOpenBar returns the largest leverage at which a stop placed at
// the cash-open bar's median range still costs at most riskFrac of equity, for
// a position opened with the given margin.
//
// This is the number that decides whether holding a stock synthetic through
// the open is possible at all: at 336u equity, 75u margin and a 5% risk unit,
// it comes out 6.0x for SNDK and 13.3x for NVDA. Not 125x.
func MaxLeverageForOpenBar(equity, margin, medianOpenPct, riskFrac float64) float64 {
	if equity <= 0 || margin <= 0 || medianOpenPct <= 0 || riskFrac <= 0 {
		return 0
	}
	notional := equity * riskFrac / (medianOpenPct / 100)
	return notional / margin
}

// StopWarning reports that a stop sits inside the cash-open bar's ordinary
// range, so that bar is expected to reach it regardless of direction. Returns
// "" when there is nothing to say.
//
// A WARNING, not a refusal, and deliberately so. A stop tighter than the open
// bar is a perfectly good choice for a position that will be closed before the
// open; what is not good is carrying it through the open without knowing the
// stop is inside the noise. Refusing here would repeat the mistake of the
// entry-based stop check, which forbade every legitimate trailing stop.
//
// untilOpen <= 0 means the open bar is already in progress or unknown, which
// is the MOST dangerous case, not a reason to stay quiet.
func StopWarning(entry, stop, medianOpenPct float64, samples int, untilOpen time.Duration) string {
	if entry <= 0 || stop <= 0 || medianOpenPct <= 0 || samples < MinSamples {
		return ""
	}
	stopPct := math.Abs(entry-stop) / entry * 100
	if stopPct <= 0 || stopPct >= medianOpenPct {
		return ""
	}
	when := "the cash open is already in progress"
	if untilOpen > 0 {
		when = "next cash open in " + humanDur(untilOpen)
	}
	return fmt.Sprintf(
		"stop is %.3f%% from entry, inside the cash-open bar's median range of %.2f%% (1/%.1f of it, n=%d) — %s, and that bar's ordinary travel reaches this stop in either direction. Close before the open, or widen beyond %.2f%%.",
		stopPct, medianOpenPct, medianOpenPct/stopPct, samples, when, medianOpenPct)
}

// humanDur renders a wait the way the warning reads best: coarse, because the
// decision it feeds ("do I still hold this at the open?") does not turn on
// seconds.
func humanDur(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// ---------------------------------------------------------------------
// Session-relative volume baseline (Layer 1).
//
// The engine's three volume gates all divide by a trailing 19/20-bar mean
// (signal/engine.go). On a 1h series that window is nearly a whole day, so it
// always contains the cash-open bar — and which side of the contamination a
// given bar falls on is decided purely by its hour:
//
//	SNDK relVol median   09:00 ET 6.32x  ...  02:00 ET 0.22x   (29x apart)
//	SPCX relVol median   09:00 ET 4.94x  ...  09:00 ET 0.37x   (13x apart)
//	BTC                                                        ( 7.2x apart)
//
// Measured at the VOTE, not just the formula: the cash-open bar is 4.1% of
// bars but produces 24-36% of every range-expansion-plus-volume fire on the
// stock synthetics, 17% on BTC, 21% on XAG.
//
// Whether normalising that IMPROVES anything is an open question, not a
// given — the open bar genuinely is a range-expansion-on-volume bar, so the
// contaminated denominator may be accidentally selecting the most informative
// bar of the day. That is what the A/B is for; this function only makes the
// alternative expressible.
// ---------------------------------------------------------------------

// TimeOfDayBucket keys a bar by its start offset within the exchange-local
// day, in minutes. For a regular timeframe this yields exactly
// 1440/tfMinutes buckets, and it tracks DST for free because the offset is
// computed in exchange-local time.
func TimeOfDayBucket(c market.Candle) int {
	n := c.OpenTime.In(NY())
	return n.Hour()*60 + n.Minute()
}

// SessionRelVol returns candles[i].Volume divided by the MEDIAN volume of
// earlier bars in the same time-of-day bucket, and whether the bucket held
// enough history to mean anything.
//
// ok=false is the important half of the contract: on a 5m series there are 288
// buckets a day, so a 300-bar window leaves ~1 sample each and the honest
// answer is "no baseline". Callers must fall back to the trailing mean rather
// than treat a one-sample median as a measurement — which also makes the whole
// change a no-op on short timeframes instead of a silent randomiser.
//
// Only bars strictly BEFORE i are used, so this stays usable inside a
// backtest without leaking the future.
func SessionRelVol(candles []market.Candle, i, minSamples int) (float64, bool) {
	if i <= 0 || i >= len(candles) || minSamples < 1 {
		return 0, false
	}
	want := TimeOfDayBucket(candles[i])
	var xs []float64
	for j := 0; j < i; j++ {
		if TimeOfDayBucket(candles[j]) == want && candles[j].Volume > 0 {
			xs = append(xs, candles[j].Volume)
		}
	}
	if len(xs) < minSamples {
		return 0, false
	}
	sort.Float64s(xs)
	base := medianSorted(xs)
	if base <= 0 {
		return 0, false
	}
	return candles[i].Volume / base, true
}
