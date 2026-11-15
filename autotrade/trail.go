package autotrade

import (
	"math"

	"github.com/henry190927/trading-bot/market"
)

// The trailing-stop primitive, ONE implementation for the backtest
// (autostrat.SimulateHTFTrend → cmd/trendbt), the paper resolver
// (EvaluateTrailFire → executor + /ops panel) and, later, the live trail
// manager. It lives here rather than in autostrat because autostrat imports
// autotrade; a second copy on this side would let paper and backtest drift
// with no error. docs/htf_trend_executor_design.md §2.2.
//
// Pure functions of closed candles: nothing is persisted, so the stop of any
// fire at any bar is recomputed from its entry, its initial stop and the bars
// since (§2.4).

// TrailStop is the stop that applies to bar j+1 of a position whose stop
// during bar j was cur: the opposite M-bar channel ending at closed bar j,
// ratcheted so it never loosens. Reads cs[j-M+1 .. j] only.
//
// With fewer than m bars up to j it returns cur unchanged — a looser stop
// than the strategy's. Callers that score a real fire must refuse that case
// (EvaluateTrailFire returns OutUnscored) rather than rely on this fallback.
func TrailStop(cs []market.Candle, j int, long bool, cur float64, m int) float64 {
	if m < 1 || j < m-1 || j >= len(cs) {
		return cur
	}
	if long {
		lo := math.Inf(1)
		for _, c := range cs[j-m+1 : j+1] {
			lo = math.Min(lo, c.Low)
		}
		return math.Max(cur, lo)
	}
	hi := math.Inf(-1)
	for _, c := range cs[j-m+1 : j+1] {
		hi = math.Max(hi, c.High)
	}
	return math.Min(cur, hi)
}

// TrailWalk is one position walked forward from its signal bar. Gross: no fee
// (the backtest adds its own; the book keeps R gross, B5).
type TrailWalk struct {
	ExitIdx int     // bar the stop was hit on; last bar when Open
	Exit    float64 // exit price (the stop, or the bar's open on a gap through it); last close when Open
	Open    bool    // no bar after the signal bar touched the stop
	// ExitStop is the stop level in force on the exit bar (= Exit unless the
	// bar gapped through it). 0 when Open.
	ExitStop float64
	// CurStop is the stop in force for the bar AFTER the last one in cs —
	// the forming bar — when Open: the trail ratcheted through the last
	// closed bar. 0 when not Open.
	CurStop float64
}

// WalkTrail walks a position entered at the close of bar i with initial stop
// `stop` through every later bar of cs (the entry price plays no part in the
// walk; R is the caller's arithmetic):
//
//   - the stop for bar j is fixed before bar j opens: the initial stop, then
//     TrailStop over the bars <= j-1 (for j = i+1 that includes the signal
//     bar itself);
//   - a long exits when Low[j] <= stop at the stop, or at Open[j] when the
//     bar opens through it; a short mirrors.
//
// There is no favourable exit, so intrabar ordering cannot help a trade.
func WalkTrail(cs []market.Candle, i int, side string, stop float64, m int) TrailWalk {
	long := side == "long"
	cur := stop
	w := TrailWalk{ExitIdx: len(cs) - 1, Exit: cs[len(cs)-1].Close, Open: true}
	for j := i + 1; j < len(cs); j++ {
		// Trail from the bars that have CLOSED before bar j opens (≤ j-1).
		cur = TrailStop(cs, j-1, long, cur, m)
		b := cs[j]
		if long && b.Low <= cur {
			w.ExitIdx, w.Exit, w.Open, w.ExitStop = j, math.Min(cur, b.Open), false, cur
			return w
		}
		if !long && b.High >= cur {
			w.ExitIdx, w.Exit, w.Open, w.ExitStop = j, math.Max(cur, b.Open), false, cur
			return w
		}
	}
	w.CurStop = TrailStop(cs, len(cs)-1, long, cur, m)
	return w
}
