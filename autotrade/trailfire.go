package autotrade

import (
	"sort"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// Scoring for fires whose exit is a trailing stop rather than a fixed
// bracket (htf-trend). docs/htf_trend_executor_design.md §2.3-§2.5.
//
// The trail is RECOMPUTED from the fire and the closed candles on every call;
// nothing is persisted. The trail reads only High/Low of closed bars and the
// fire carries the entry and the initial stop, so the recomputation is exact,
// and the append-only log stays the only state (§2.4).

// IsTrail reports whether f exits on a trailing stop. Every row written
// before the trail path existed decodes TrailM as 0 and takes the
// fixed-bracket path byte for byte.
func (f PaperFire) IsTrail() bool { return f.TrailM > 0 }

// Resolve scores a fire from closed candles: EvaluateTrailFire for a trail
// fire, EvaluateFire otherwise. Every caller that used to call EvaluateFire on
// a logged fire goes through here, so a trail fire can never reach the
// fixed-bracket scorer by a forgotten call site.
func Resolve(f PaperFire, cs []market.Candle, expiryBars int) Outcome {
	if f.IsTrail() {
		return EvaluateTrailFire(f, cs)
	}
	return EvaluateFire(f, cs, expiryBars)
}

// ResolveLive is Resolve plus the live-mark overlay (EvaluateFireLive /
// EvaluateTrailFireLive).
func ResolveLive(f PaperFire, cs []market.Candle, expiryBars int, livePrice float64) Outcome {
	if f.IsTrail() {
		return EvaluateTrailFireLive(f, cs, livePrice)
	}
	return EvaluateFireLive(f, cs, expiryBars, livePrice)
}

// HoldsSlot reports whether a rule's newest fire still occupies its slot (and
// counts in the open book). Open and pending always do. A trail fire that
// cannot be scored does too — the opposite of a fixed-bracket fire, whose
// Unscored frees the slot (OutUnscored). A trail position can run for weeks,
// past any kline window, and freeing its slot would re-enter a trend the book
// is already in; holding it fails toward refusing an entry, the same posture
// BuildBook takes for missing candles (§2.4).
func HoldsSlot(f PaperFire, oc Outcome) bool {
	switch oc.Status {
	case OutOpen, OutPending:
		return true
	case OutUnscored:
		return f.IsTrail()
	}
	return false
}

// stopHold is how long after a stop exit the rule's slot stays taken. A trail
// fire carries its own rule's cooldown and TF (B6): the replays' single
// max-across-rules × 1h convention would absorb re-entries the executor
// really took (htf-trend runs cooldown 0, like its backtest). Fixed-bracket
// fires keep the caller's value so their totals do not move.
func stopHold(f PaperFire, exitAt time.Time, cooldownBars int, barDur time.Duration) time.Time {
	if f.IsTrail() {
		return exitAt.Add(time.Duration(f.CooldownBars) * market.BarDuration(market.Timeframe(f.TF)))
	}
	return exitAt.Add(time.Duration(cooldownBars) * barDur)
}

// openForever is the hold for a position that has not left the book.
const openForever = 1_000_000 * time.Hour

// trailSignalIdx finds the fire's signal bar in cs: the bar whose CloseTime is
// f.BarTime, or for a row without one, the newest bar closed at f.Time —
// accepted only when it closed less than one bar before the fire, so a stale
// window whose last bar is days old is not mistaken for the signal bar.
// -1 = not in cs.
func trailSignalIdx(f PaperFire, cs []market.Candle) int {
	if !f.BarTime.IsZero() {
		i := sort.Search(len(cs), func(k int) bool { return !cs[k].CloseTime.Before(f.BarTime) })
		if i < len(cs) && cs[i].CloseTime.Equal(f.BarTime) {
			return i
		}
		return -1
	}
	i := sort.Search(len(cs), func(k int) bool { return cs[k].CloseTime.After(f.Time) }) - 1
	if i < 0 {
		return -1
	}
	if d := market.BarDuration(market.Timeframe(f.TF)); d > 0 && f.Time.Sub(cs[i].CloseTime) >= d {
		return -1
	}
	return i
}

// EvaluateTrailFire replays a trail fire over closed candles (ascending):
//
//  1. the signal bar i is located (trailSignalIdx); absent → OutUnscored;
//  2. i must have the first trail's lookback behind it (i >= TrailM-1);
//     otherwise OutUnscored — never a looser trail computed from fewer bars
//     (B7; TrailStop would silently keep the initial stop);
//  3. WalkTrail from i. An exit is OutStop at the walk's exit price (the stop,
//     or the bar's open on a gap through it), ExitAt = that bar's close, NetR
//     gross against the INITIAL stop. No exit is OutOpen, UnrealR marked to
//     the last close, CurStop = the stop for the forming bar.
//
// The fill is at f.Entry at the fire, as for every marketable fire. Status
// stays OutStop for every exit (it is a stop order), so everything that
// settles TP|Stop settles it unchanged; Trailed says whether the stop had
// moved off its initial level.
func EvaluateTrailFire(f PaperFire, cs []market.Candle) Outcome {
	long := f.Side == "long"
	risk, dir := f.Entry-f.Stop, 1.0
	if !long {
		risk, dir = f.Stop-f.Entry, -1.0
	}
	if risk <= 0 || f.TrailM < 1 {
		return Outcome{Status: OutOpen} // malformed — can't score; holds its slot like EvaluateFire's
	}
	i := trailSignalIdx(f, cs)
	if i < 0 || i < f.TrailM-1 {
		return Outcome{Status: OutUnscored}
	}
	// A touch fire filled inside bar i; if bar i also reached the initial
	// stop it is a stop on bar i at the stop price — conservative, and what
	// the touch backtest scores (SimulateHTFTrendTouch).
	if f.Touch && ((long && cs[i].Low <= f.Stop) || (!long && cs[i].High >= f.Stop)) {
		return Outcome{Status: OutStop, FillPrice: f.Entry, FilledAt: f.Time, ExitPrice: f.Stop,
			NetR: dir * (f.Stop - f.Entry) / risk, ExitAt: cs[i].CloseTime}
	}
	w := WalkTrail(cs, i, f.Side, f.Stop, f.TrailM)
	out := Outcome{FillPrice: f.Entry, FilledAt: f.Time, ExitPrice: w.Exit, BarsHeld: w.ExitIdx - i}
	if !w.Open {
		out.Status = OutStop
		out.NetR = dir * (w.Exit - f.Entry) / risk
		out.ExitAt = cs[w.ExitIdx].CloseTime
		out.Trailed = w.ExitStop != f.Stop
		return out
	}
	out.Status = OutOpen
	out.UnrealR = dir * (w.Exit - f.Entry) / risk
	out.CurStop = w.CurStop
	out.Trailed = w.CurStop != f.Stop
	return out
}

// EvaluateTrailFireLive overlays the live mark on EvaluateTrailFire, the same
// rule EvaluateFireLive applies, against the TRAILED level: a mark at or
// through CurStop resolves now at CurStop; otherwise UnrealR is marked to it.
// Display only (the panel): once the forming bar closes, the closed-bar walk
// takes over with the exact gap rule. Like EvaluateFireLive, a live
// resolution has no ExitAt — the bar it happened in has not closed.
func EvaluateTrailFireLive(f PaperFire, cs []market.Candle, livePrice float64) Outcome {
	out := EvaluateTrailFire(f, cs)
	if livePrice <= 0 || out.Status != OutOpen || out.CurStop <= 0 {
		return out
	}
	long := f.Side == "long"
	risk, dir := f.Entry-f.Stop, 1.0
	if !long {
		risk, dir = f.Stop-f.Entry, -1.0
	}
	if (long && livePrice <= out.CurStop) || (!long && livePrice >= out.CurStop) {
		out.Status, out.ExitPrice, out.UnrealR = OutStop, out.CurStop, 0
		out.NetR = dir * (out.CurStop - f.Entry) / risk
		return out
	}
	out.ExitPrice, out.UnrealR = livePrice, dir*(livePrice-f.Entry)/risk
	return out
}

// absorbedBy reports whether fire f falls inside a slot held until u. A trail
// fire is judged by its SIGNAL BAR, not the minute it was logged: a signal on
// the very bar its predecessor stopped out on is one the backtest never takes
// ("after an exit on bar j, the next signal must come on bar >= j+1"), yet it
// is logged a minute after that bar's close, i.e. after u. Fixed-bracket
// fires keep the Time comparison every existing number was computed with.
func absorbedBy(f PaperFire, u time.Time) bool {
	if f.IsTrail() && !f.BarTime.IsZero() {
		return !f.BarTime.After(u)
	}
	return f.Time.Before(u)
}

// SlotBlocked is the executor's paper one-position-per-rule decision
// (cmd/monitor paperBlocked) as a pure function, so cmd/trendbt can replay the
// executor's gating offline with the same code. latest is the rule's newest
// fire, cs the executor's candles for it, barTime the candidate's signal-bar
// close, ruleCooldown the rule's cooldown (fixed bracket) and barDur its TF.
//
//   - open / pending (or a trail fire that cannot be scored) → blocked;
//   - no candles → blocked (fail-safe);
//   - stopped → blocked through the cooldown: for a fixed bracket while
//     barTime < exit + cooldown (unchanged); for a trail fire while barTime
//     <= exit + its own CooldownBars — so with cooldown 0 a signal on the exit
//     bar itself is refused, as in the backtest;
//   - tp / no-fill → free.
func SlotBlocked(latest PaperFire, cs []market.Candle, barTime time.Time, ruleCooldown int, barDur time.Duration) bool {
	if len(cs) == 0 {
		return true
	}
	oc := Resolve(latest, cs, 6)
	if HoldsSlot(latest, oc) {
		return true
	}
	if oc.Status != OutStop {
		return false
	}
	if latest.IsTrail() {
		return !barTime.After(oc.ExitAt.Add(time.Duration(latest.CooldownBars) * barDur))
	}
	return barTime.Before(oc.ExitAt.Add(time.Duration(ruleCooldown) * barDur))
}
