package signal

import (
	"fmt"

	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
)

// StrategyKind selects which evaluation path a symbol runs through Evaluate.
type StrategyKind int

const (
	// StrategyMR is the original mean-reversion + confluence engine (the
	// entire body of Evaluate below the dispatch). Default for every symbol.
	StrategyMR StrategyKind = iota
	// StrategyStructMomentum is the trend/structure-aligned strategy:
	// BOS-continuation retrace into the 樞紐區. Better fit for trend-driven
	// alts where mean-reversion fights the move. See
	// docs/struct_momentum_strategy_design.md.
	StrategyStructMomentum
)

// StructMomentumEnabled forces the StructMomentum strategy ON for ALL symbols.
// Mirrors StructureVetoEnabled / StructureZoneVoteEnabled — the A/B backtest
// switch (cmd/backtest --struct-momentum) used to validate the strategy on
// existing symbols before any alt is added to strategyFor. Default false.
var StructMomentumEnabled = false

// strategyFor returns the strategy a symbol runs. Per-symbol allowlist, same
// pattern as isStructureVetoSymbol. Everything defaults to MR; a symbol opts
// into StructMomentum ONLY after passing its own A/B gate (none yet — alts are
// added here in Phase 3 after validation, e.g. `case market.SOLUSDT: return
// StrategyStructMomentum`).
func strategyFor(sym market.Symbol) StrategyKind {
	switch sym {
	// (no symbols assigned to StructMomentum yet — Phase 1 validation first)
	}
	return StrategyMR
}

// StructMomentum tunables.
const (
	smEMAFast        = 20 // fast EMA for the momentum-alignment gate
	smEMASlow        = 50 // slow EMA
	smStructStrength = 2  // pivot strength for AnalyzeStructure (matches web /bias)
)

// evaluateStructMomentum is the trend/structure-aligned strategy — a SIBLING of
// the MR path in Evaluate. It does NOT touch MR logic; closed-bar only, so
// backtest parity for MR symbols is preserved. Both strategies share the macro
// + earnings blackout gates at the top of Evaluate (dispatch happens after them).
//
// v1 logic (design doc §4, user-signed §10):
//   - regime gate: Trend must be Up/Down (Neutral → Flat: this strategy sits
//     out chop, which is exactly why it suits trending alts)
//   - entry (A-only): a live aligned 樞紐區 with price inside it (InZone), on an
//     intact leg (no counter-trend BOS/CHoCH) = a BOS-continuation retrace
//   - momentum gate: EMA(fast) on the trend side of EMA(slow). NOTE: the design
//     said "reuse the engine's MomentumScore", but that counter is interleaved
//     with the MR confluence across ~350 lines and can't be extracted cleanly
//     without risking MR backtest parity — so we use a self-contained EMA
//     alignment gate (§4.3's listed fallback). Flagged for the record.
//   - stop = Zone.Invalidate (leg origin); target = Zone.Target (measured move)
//   - Score lands in the 3-4 band so the existing MIN_SCORE=3 gate + ntfy +
//     dashboard chips work unchanged. All-momentum by construction.
func evaluateStructMomentum(in Inputs) Signal {
	sig := Signal{Symbol: in.Symbol, Timeframe: in.Timeframe}
	cs := in.Candles
	if len(cs) < smEMASlow+5 {
		return sig // not enough history → Flat
	}
	sig.Price = cs[len(cs)-1].Close

	st := AnalyzeStructure(cs, smStructStrength)

	// Regime gate — trend only.
	var want Side
	switch st.Trend {
	case StructUptrend:
		want = Long
	case StructDowntrend:
		want = Short
	default:
		return sig
	}

	// Aligned 樞紐區 with price inside it (the retrace trigger).
	if st.Zone == nil || !st.InZone {
		return sig
	}
	if (want == Long && st.Zone.Dir != StructUptrend) || (want == Short && st.Zone.Dir != StructDowntrend) {
		return sig
	}

	// A-only continuation: reject if the latest structural event is a
	// counter-trend BOS/CHoCH (leg turning against us).
	switch want {
	case Long:
		if st.Event == EvBOSDown || st.Event == EvCHoCHDown {
			return sig
		}
	case Short:
		if st.Event == EvBOSUp || st.Event == EvCHoCHUp {
			return sig
		}
	}

	// Momentum gate — EMA alignment with the trend.
	closes := market.Closes(cs)
	emaF := indicator.EMA(closes, smEMAFast)
	emaS := indicator.EMA(closes, smEMASlow)
	f, s := emaF[len(emaF)-1], emaS[len(emaS)-1]
	if (want == Long && !(f > s)) || (want == Short && !(f < s)) {
		return sig
	}

	// Plan straight from structure. Entry = current close (we're in-zone now →
	// take it); stop = leg-origin invalidation; target = 1:1 measured move.
	z := st.Zone
	entry, stop, target := sig.Price, z.Invalidate, z.Target
	// Sanity: stop/target must bracket entry in the right direction.
	if want == Long && !(stop < entry && entry < target) {
		return sig
	}
	if want == Short && !(target < entry && entry < stop) {
		return sig
	}
	risk := entry - stop
	if want == Short {
		risk = stop - entry
	}
	if risk <= 0 {
		return sig
	}

	// TPs at 1R and 2R off the structural stop. The shared backtest R-model
	// scores a fixed +2R when the LAST TP is hit and requires >=2 TP levels,
	// so 1R/2R keeps StructMomentum directly comparable to the MR baseline.
	// The measured-move (z.Target) is kept in the plan Note for live TP
	// placement (design §4.4) — decoupled from the backtest's fixed-R yardstick.
	var tp1, tp2 float64
	if want == Long {
		tp1, tp2 = entry+risk, entry+2*risk
	} else {
		tp1, tp2 = entry-risk, entry-2*risk
	}

	sig.Side = want
	score := 3
	if (want == Long && st.Event == EvBOSUp) || (want == Short && st.Event == EvBOSDown) {
		score++ // fresh BOS this bar = stronger continuation
	}
	sig.Score = score
	sig.MomentumScore = score
	sig.MRScore = 0
	sig.Plan = Plan{
		OrderType:  OrderMarket,
		Entry:      entry,
		StopLoss:   stop,
		TakeProfit: []float64{tp1, tp2},
		RR:         []float64{1, 2},
		Anchor:     fmt.Sprintf("%s 樞紐區 retrace", st.Trend.String()),
		Note:       fmt.Sprintf("struct-momentum: BOS retrace into 樞紐區; measured-move %.4f", target),
	}
	sig.Reasons = append(sig.Reasons, fmt.Sprintf(
		"%s + intact leg, retrace into 樞紐區 [%.4f–%.4f]; EMA%d %s EMA%d",
		st.Trend.String(), z.Lo, z.Hi, smEMAFast,
		map[bool]string{true: ">", false: "<"}[f > s], smEMASlow))
	return sig
}
