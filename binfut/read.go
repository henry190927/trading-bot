package binfut

import "time"

// LatestOI returns the newest open-interest bucket for a short name.
func (s Store) LatestOI(short string) (OIPoint, bool) {
	pts := s.Symbols[short].OI
	if len(pts) == 0 {
		return OIPoint{}, false
	}
	return pts[len(pts)-1], true
}

// OIChange returns the fractional change in open-interest NOTIONAL across
// `window`, ending at the newest bucket.
//
// Anchored to the newest bucket rather than to wall-clock now: Binance stamps
// each bucket and the newest may be a few minutes old, so measuring back from
// the clock would shorten the window by however stale the cache is.
//
// The comparison point is the newest bucket at or before (newest - window).
// Returns false when the series does not reach back that far — never a zero,
// which beside a real reading would look like "flat" rather than "unknown".
func (s Store) OIChange(short string, window time.Duration) (float64, bool) {
	pts := s.Symbols[short].OI
	if len(pts) < 2 || window <= 0 {
		return 0, false
	}
	cur := pts[len(pts)-1]
	target := cur.Time.Add(-window)
	var prev *OIPoint
	for i := len(pts) - 2; i >= 0; i-- {
		if !pts[i].Time.After(target) {
			prev = &pts[i]
			break
		}
	}
	if prev == nil || prev.Value <= 0 {
		return 0, false
	}
	return (cur.Value - prev.Value) / prev.Value, true
}

// Whale is the positioning split: top traders against all accounts, both
// measured the same way.
type Whale struct {
	Top   float64 // top traders, long/short by ACCOUNT COUNT
	All   float64 // all accounts, long/short by ACCOUNT COUNT
	Gap   float64 // Top - All; negative = the crowd is longer than the whales
	TopSz float64 // top traders by POSITION SIZE — a third reading, see below
}

// Whales returns the split for a short name.
//
// Top and All are BOTH by account count, which is the whole point: Binance
// also publishes a size-weighted top-trader ratio, and differencing THAT
// against the account-count crowd ratio would blend two different questions
// (whale versus retail, and size versus headcount) into one number that
// answers neither. TopSz is carried alongside for reference — when it sits
// below Top, the bigger whale positions lean shorter than the whale headcount
// does, which is a real signal but a separate one.
//
// Reading the gap: negative means retail accounts are longer than top-trader
// accounts. That is the classic "the crowd is on this side" configuration. It
// is NOT a trade on its own — the crowd is right during a trend, which is most
// of what a trend is.
func (s Store) Whales(short string) (Whale, bool) {
	d := s.Symbols[short]
	if len(d.TopAccounts) == 0 || len(d.AllAccounts) == 0 {
		return Whale{}, false
	}
	w := Whale{
		Top: d.TopAccounts[len(d.TopAccounts)-1].Ratio,
		All: d.AllAccounts[len(d.AllAccounts)-1].Ratio,
	}
	if w.Top <= 0 || w.All <= 0 {
		return Whale{}, false
	}
	w.Gap = w.Top - w.All
	if len(d.TopPositions) > 0 {
		w.TopSz = d.TopPositions[len(d.TopPositions)-1].Ratio
	}
	return w, true
}

// WhaleGapMeaningful is the smallest Top-minus-All difference worth naming.
//
// The two series are structurally different populations, so they never sit
// exactly equal; a small gap is the baseline, not a signal. 0.25 is a quarter
// of a whole ratio point — on 2026-09-11 ETH read Top 1.40 against All 2.49,
// a gap of -1.09, while BTC read +0.51. Both are far outside this band, and
// the everyday noise between them is far inside it.
const WhaleGapMeaningful = 0.25

// Crowd names which side the retail book is on relative to the whales, or ""
// when the gap is inside the noise band.
func (w Whale) Crowd() string {
	switch {
	case w.Gap <= -WhaleGapMeaningful:
		return "retail-longer"
	case w.Gap >= WhaleGapMeaningful:
		return "retail-shorter"
	}
	return ""
}
