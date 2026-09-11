package bls

import (
	"strings"
	"time"
)

// Metric says which BLS series answers a calendar row, and how to turn the
// index into the figure that row quotes.
type Metric struct {
	SeriesID  string
	Transform string // "mom" | "yoy" | "diff"
	Unit      string // "%" for percent changes, "k" for a level change in thousands
}

// metrics maps a ForexFactory row title to the series that settles it.
//
// EXACT match on the normalised title, not a substring test. "Core CPI m/m"
// contains "CPI m/m", so a contains-based lookup would answer the core row
// with the headline series — a wrong number in the exact place a wrong number
// does damage. An unrecognised title returns not-found and renders blank,
// which is the failure worth having: if the feed renames a row, the column
// goes empty instead of quietly reporting the wrong series.
//
// Seasonal adjustment follows the convention each figure is published under:
// m/m uses the SA series (an unadjusted month-over-month is dominated by
// seasonality), y/y uses NSA (the twelve-month comparison removes it, and NSA
// is what the headline y/y is quoted from).
//
// Core PPI is absent deliberately — see the note on Series. Four candidate
// IDs return data and disagree by a factor of seven for 2026-08; naming one
// without the catalog to confirm it would be a guess.
var metrics = map[string]Metric{
	"CPI m/m":                    {"CUSR0000SA0", "mom", "%"},
	"CPI y/y":                    {"CUUR0000SA0", "yoy", "%"},
	"Core CPI m/m":               {"CUSR0000SA0L1E", "mom", "%"},
	"Core CPI y/y":               {"CUUR0000SA0L1E", "yoy", "%"},
	"PPI m/m":                    {"WPSFD4", "mom", "%"},
	"PPI y/y":                    {"WPSFD4", "yoy", "%"},
	"Non-Farm Employment Change": {"CES0000000001", "diff", "k"},
}

// MetricFor resolves a calendar row title to its series, or reports that this
// package will not speak for it.
func MetricFor(title string) (Metric, bool) {
	m, ok := metrics[strings.Join(strings.Fields(title), " ")]
	return m, ok
}

// DataMonthFor returns the month a release REPORTS, given when it was
// released. CPI, PPI and the Employment Situation all publish the prior
// month: August CPI came out 2026-09-11, and WPSFD4's newest observation
// after the 2026-09-10 PPI release is 2026-M08.
func DataMonthFor(release time.Time) (year, month int) {
	r := release.UTC()
	y, m := r.Year(), int(r.Month())
	if m == 1 {
		return y - 1, 12
	}
	return y, m - 1
}

// Actual returns the released figure for a calendar row, in the unit that row
// quotes it in.
//
// Returns ok=false when the series is unmapped, when the reporting month is
// not in the cache yet (the normal state before a release), or when the
// previous period needed for the comparison is missing. All three are "no
// answer", never a zero — a 0.0% printed next to a 0.4% forecast reads as a
// miss rather than as silence.
func (s Store) Actual(title string, release time.Time) (value float64, unit string, ok bool) {
	m, ok := MetricFor(title)
	if !ok {
		return 0, "", false
	}
	y, mo := DataMonthFor(release)
	switch m.Transform {
	case "mom":
		v, ok := s.MoM(m.SeriesID, y, mo)
		return v, m.Unit, ok
	case "yoy":
		v, ok := s.YoY(m.SeriesID, y, mo)
		return v, m.Unit, ok
	case "diff":
		v, ok := s.Diff(m.SeriesID, y, mo)
		return v, m.Unit, ok
	}
	return 0, "", false
}
