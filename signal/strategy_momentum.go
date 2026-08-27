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

// SMStopBufferATR, when >0, pushes the StructMomentum stop this many ATR(14)
// BEYOND the leg-origin invalidate (long: lower, short: higher) instead of sitting
// exactly on it — the "don't stop on the sweep magnet" idea.
//
// A/B RESULT (2026-08-27, --sm-stop-buffer 0.5, SOL+LINK, 60/90/120d): WORSE in
// every window (SOL +0.72/+6.78/+7.64 → −1.18/+5.21/+6.11; LINK −1.71/+4.20/+1.27
// → −3.25/+1.87/−2.11). Stop-on-the-invalidate wins. Joins --stop-buffer-r and
// --slide-offset-pct as tested-and-rejected stop refinements: widening pays more
// on the majority real-move losers than it saves on the minority sweep-reclaims.
// Kept as an off-by-default variant. The 🪝 "stop off the magnet" edge is
// DISCRETIONARY (manual confirm-entry below the wall), not a systematic engine stop.
var SMStopBufferATR = 0.0

// StructMomentumEnabled forces the StructMomentum strategy ON for ALL symbols.
// Mirrors StructureVetoEnabled / StructureZoneVoteEnabled — the A/B backtest
// switch (cmd/backtest --struct-momentum) used to validate the strategy on
// existing symbols before any alt is added to strategyFor. Default false.
var StructMomentumEnabled = false

// strategyFor returns the strategy a (symbol, timeframe) runs. Per-(symbol,TF)
// allowlist — the A/B showed the StructMomentum edge is BOTH symbol- and
// TF-specific (BTC likes it on 1h not 2h; ETH the reverse), so assignment must
// be per-pair. Everything defaults to MR; a pair opts into StructMomentum ONLY
// after clearing its A/B gate (Phase 3, 2026-08-22).
//
// Assignments (forward-log only — none of these are in market.All(), so this
// changes NO daemon/live-core behavior; the core 4 stay MR on every TF):
//   SOL 1h/2h, LINK 1h/2h — cleared strongly.  SUI 1h, HYPE 1h — marginal
//   (avoid MR's bleed; forward-log to confirm).  NEAR — MR on all TFs (it
//   A/B'd as a mean-reversion symbol).  BTC/ETH left on MR pending their own
//   forward-log decision despite BTC-1h/ETH-2h showing promise.
func strategyFor(sym market.Symbol, tf market.Timeframe) StrategyKind {
	switch sym {
	case market.SOLUSDT, market.LINKUSDT:
		if tf == "1h" || tf == "2h" {
			return StrategyStructMomentum
		}
	case market.SUIUSDT, market.HYPEUSDT:
		if tf == "1h" {
			return StrategyStructMomentum
		}
	}
	return StrategyMR
}

// StrategyFor is the exported view of the per-(symbol,TF) assignment, for
// callers (e.g. web /setups tagging) that need to know which strategy is
// active without duplicating the allowlist.
func StrategyFor(sym market.Symbol, tf market.Timeframe) StrategyKind {
	return strategyFor(sym, tf)
}

// String renders the strategy for tagging/display.
func (k StrategyKind) String() string {
	if k == StrategyStructMomentum {
		return "struct-momentum"
	}
	return "mr"
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
	// Optional sweep buffer: push the stop past the leg-origin magnet by N×ATR.
	if SMStopBufferATR > 0 {
		if atrs := indicator.ATR(cs, 14); len(atrs) > 0 {
			a := atrs[len(atrs)-1]
			if want == Long {
				stop -= SMStopBufferATR * a
			} else {
				stop += SMStopBufferATR * a
			}
		}
	}
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
