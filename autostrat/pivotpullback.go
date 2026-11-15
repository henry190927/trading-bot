package autostrat

import (
	"fmt"
	"sort"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// PivotPullbackParams configure the "pivot-pullback" trend-following trigger
// (docs/pivot_pullback_design.md). DefaultPivotPullbackParams is the
// pre-registered P0 arm; the two pre-registered variants change exactly one
// field each (Trigger, Stop).
//
// NOT wired into EvalAutoTrigger: cmd/pivotbt is the only caller until the
// gate says otherwise.
type PivotPullbackParams struct {
	HTFLookback   int     // closed HTF bars fed to AnalyzeStructure
	Strength      int     // HTF swing strength for the leg / zone
	MicroStrength int     // base-TF swing strength for the pullback's lower high
	SegmentCap    int     // at most this many base bars in the pullback segment
	MinR          float64 // skip a bracket whose reward:risk is below this
	Trigger       string  // "bos" (P0, micro-BOS) | "reclaim" (V1)
	Stop          string  // "invalidate" (P0, 4h leg origin) | "pullback" (V2, pullback extreme)
}

// DefaultPivotPullbackParams is P0 as fixed in the design doc on 2026-11-15.
func DefaultPivotPullbackParams() PivotPullbackParams {
	return PivotPullbackParams{
		HTFLookback: 200, Strength: 2, MicroStrength: 2, SegmentCap: 96,
		MinR: 1.5, Trigger: "bos", Stop: "invalidate",
	}
}

// PivotPullbackTrigger evaluates closed base bar i of cs against the higher-TF
// series htf (ascending). Pure: it reads cs[:i+1] and only the htf bars whose
// CloseTime is at or before cs[i].OpenTime, so neither a later base bar nor an
// HTF bar still forming at i can change the answer. Swings confirm `strength`
// bars late by construction (FindSwingPoints needs bars on both sides inside
// the slice it is given).
//
// Long (short mirrors): HTF HH-HL trend with an intact up-leg 樞紐區; in the
// base bars since the HTF leg high, the last swing top below the leg high is
// the pullback's lower high LH; price dipped into the zone after LH; this bar
// closes above LH having closed at or below it on the previous bar. Market
// entry at the close, stop at the leg origin, TP at the 1:1 measured move.
func PivotPullbackTrigger(cs, htf []market.Candle, i int, wantLong, wantShort bool, p PivotPullbackParams) Trigger {
	if i < 1 || i >= len(cs) || len(htf) == 0 {
		return Trigger{}
	}
	asOf := cs[i].OpenTime
	n := sort.Search(len(htf), func(k int) bool { return htf[k].CloseTime.After(asOf) })
	if n == 0 {
		return Trigger{}
	}
	lb := 0
	if p.HTFLookback > 0 && n > p.HTFLookback {
		lb = n - p.HTFLookback
	}
	h := htf[lb:n]
	trend, _, _ := signal.ClassifyTrendStructure(h)
	var long bool
	switch {
	case wantLong && trend == signal.StructUptrend:
		long = true
	case wantShort && trend == signal.StructDowntrend:
		long = false
	default:
		return Trigger{}
	}

	// The impulse leg: the last HTF swing in the trend direction (top for an
	// uptrend) and the last opposite swing before it (its origin). Not
	// AnalyzeStructure's Zone: that one flips to the PULLBACK leg as soon as a
	// pullback swing confirms, which is exactly when this entry is sought.
	pts := signal.FindSwingPoints(h, p.Strength, 0)
	termK := -1
	for k := len(pts) - 1; k >= 0; k-- {
		if pts[k].IsTop == long {
			termK = k
			break
		}
	}
	if termK < 0 {
		return Trigger{}
	}
	origK := -1
	for k := termK - 1; k >= 0; k-- {
		if pts[k].IsTop != long {
			origK = k
			break
		}
	}
	if origK < 0 {
		return Trigger{}
	}
	z := impulseZone(long, pts[termK].Price, pts[origK].Price)
	if z == nil {
		return Trigger{}
	}
	legStart := h[pts[termK].Index].OpenTime

	// Pullback segment: base bars from the leg's terminal HTF bar to i.
	k0 := i
	for k0 > 0 && !cs[k0-1].OpenTime.Before(legStart) && (p.SegmentCap <= 0 || i-(k0-1) < p.SegmentCap) {
		k0--
	}
	if i-k0+1 < 5 {
		return Trigger{}
	}
	seg := cs[k0 : i+1]
	for _, c := range seg {
		// Closed through the origin = leg failed; traded beyond the terminal
		// pivot = the leg extended and the new pivot is not confirmed yet.
		if long && (c.Close <= z.Invalidate || c.High > z.LegHigh) {
			return Trigger{}
		}
		if !long && (c.Close >= z.Invalidate || c.Low < z.LegLow) {
			return Trigger{}
		}
	}

	bar, prev := cs[i], cs[i-1]
	near := z.Hi // the 0.5 edge price reaches first: Hi for an up-leg
	if !long {
		near = z.Lo
	}

	var why string
	var pbExt float64 // pullback extreme after the reference level (V2 stop)
	switch p.Trigger {
	case "reclaim":
		if long && !(bar.Close > near && prev.Close <= near) {
			return Trigger{}
		}
		if !long && !(bar.Close < near && prev.Close >= near) {
			return Trigger{}
		}
		pbExt = extremeOf(seg, long)
		why = fmt.Sprintf("4h %s, 1h close reclaims 樞紐區 edge %.4f", trend, near)
	default: // "bos"
		sw := signal.FindSwingPoints(seg, p.MicroStrength, 0)
		ref := -1
		for k := len(sw) - 1; k >= 0; k-- {
			if sw[k].IsTop != long {
				continue
			}
			ref = k
			break
		}
		if ref < 0 {
			return Trigger{}
		}
		lh := sw[ref]
		if (long && lh.Price >= z.LegHigh) || (!long && lh.Price <= z.LegLow) {
			return Trigger{} // no lower high (higher low) yet: the leg pivot itself
		}
		after := seg[lh.Index+1:]
		if len(after) == 0 {
			return Trigger{}
		}
		pbExt = extremeOf(after, long)
		if (long && pbExt > near) || (!long && pbExt < near) {
			return Trigger{} // the dip after LH never reached the zone
		}
		if long && !(bar.Close > lh.Price && prev.Close <= lh.Price) {
			return Trigger{}
		}
		if !long && !(bar.Close < lh.Price && prev.Close >= lh.Price) {
			return Trigger{}
		}
		why = fmt.Sprintf("4h %s, dip into 樞紐區 %.4f–%.4f, 1h close breaks pullback swing %.4f", trend, z.Lo, z.Hi, lh.Price)
	}

	entry, stop, tp := bar.Close, z.Invalidate, z.Target
	if p.Stop == "pullback" {
		stop = pbExt
	}
	side := "long"
	risk, reward := entry-stop, tp-entry
	if !long {
		side = "short"
		risk, reward = stop-entry, entry-tp
	}
	if risk <= 0 || reward <= 0 || reward/risk < p.MinR {
		return Trigger{}
	}
	return Trigger{
		Fire: true, Side: side, Market: true, Entry: entry, Stop: stop, TP: tp,
		Why:  fmt.Sprintf("%s (%.2fR)", why, reward/risk),
		Tags: []Tag{tag("pivot-zone", z.Lo, z.Hi), tag("leg-invalidate", z.Invalidate), tag("leg-target", z.Target)},
	}
}

// extremeOf is the lowest Low (long) or highest High (short) of cs.
func extremeOf(cs []market.Candle, long bool) float64 {
	v := cs[0].Low
	if !long {
		v = cs[0].High
	}
	for _, c := range cs[1:] {
		if long && c.Low < v {
			v = c.Low
		}
		if !long && c.High > v {
			v = c.High
		}
	}
	return v
}

// impulseZone is the 0.5–0.705 樞紐區 of the leg term←orig, the same formula
// as signal's buildPivotZone (Hi/Lo are price-ordered; Invalidate = origin,
// Target = 1:1 measured move). nil for a degenerate leg.
func impulseZone(long bool, term, orig float64) *signal.PivotZone {
	if long && term > orig {
		span := term - orig
		return &signal.PivotZone{Dir: signal.StructUptrend, LegHigh: term, LegLow: orig,
			Hi: term - 0.5*span, Lo: term - 0.705*span, Invalidate: orig, Target: term + span}
	}
	if !long && orig > term {
		span := orig - term
		return &signal.PivotZone{Dir: signal.StructDowntrend, LegHigh: orig, LegLow: term,
			Lo: term + 0.5*span, Hi: term + 0.705*span, Invalidate: orig, Target: term - span}
	}
	return nil
}
