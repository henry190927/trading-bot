package signal

import (
	"fmt"

	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
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

// StructMomentumOff forces the MR engine for EVERY symbol, overriding the
// strategyFor allowlist.
//
// Without it, an SM-vs-MR A/B on an ALREADY-ASSIGNED symbol is inert: the
// baseline arm runs `strategyFor(SOL,1h) == StrategyStructMomentum` and the
// variant arm runs StructMomentumEnabled, so BOTH arms execute SM and the two
// outputs are byte-identical. That is precisely the failure mode recorded for
// cmd/flipbt, where an inert -window flag produced a flattering +10.00R that
// turned into -4.00R once the parameter actually did something.
//
// So this is not a convenience switch — it is the only thing that makes the
// baseline arm a real baseline for SOL/LINK/SUI/HYPE. Takes precedence over
// StructMomentumEnabled; setting both is a caller error and cmd/backtest
// refuses it rather than silently picking one.
var StructMomentumOff = false

// StructMomentumRegime picks the strategy PER BAR from the measured structure
// instead of from the per-symbol allowlist.
//
// The allowlist asks "is this symbol a momentum symbol?", which a whole-window
// A/B answers on average. That average is the problem: a strategy that is
// strongly positive while a trend is running and strongly negative while price
// ranges can have a negative unconditional expectation and a positive
// conditional one, and the per-symbol gate cannot express the difference.
// StructMomentum failed on five of six alts that way — the question it was
// asked was whether it wins ALWAYS, not whether it wins WHEN.
//
// So: a confirmed HH-HL / LH-LL structure runs StructMomentum, anything
// neutral runs MR. The gate is deliberately the detector that already exists
// and already A/B'd as a counter-trend veto (+30R XAG, +7R XAU, negative on
// ETH) — that split is itself evidence that structure state carries
// strategy-relevant information, and reusing it means the arm tests the
// SELECTOR rather than a new detector at the same time.
//
// It must stay a pure function of closed candles. OI, funding and the whale
// ratios all say something about regime and NONE of them can be backtested:
// oi.jsonl holds about a week, so a 120-day window cannot see them. A regime
// input that cannot be measured over the gate's windows cannot be shipped
// through the gate.
//
// Selection is exhaustive: every bar gets exactly one strategy, none is
// skipped. That keeps the arm comparable to both baselines on trade count,
// which a post-hoc split of an existing run cannot be — under one-position
// dedup, skipping an SM entry frees the slot for an MR entry, so the gated
// arm has to be GENERATED gated, not filtered afterwards.
//
// Overrides StructMomentumEnabled and the allowlist, because it replaces the
// question both of them answer. StructMomentumOff still wins: that is the
// all-MR baseline the arm is measured against.
//
// RESULT 2026-09-19 — FAILS, 10 of 10 gate runs (BTC/ETH/SOL/SUI/NEAR 1h,
// 60/90/120d, against both baselines). Kept off by default and kept in the
// tree, because the negative result is the useful part and re-deriving it
// costs another afternoon.
//
//	symbol  MR-all 60/90/120      SM-all               REGIME
//	BTC     -4.96 -3.11 -11.05    -1.78 +0.56  +3.38   -6.69  -1.49  -1.44
//	ETH    +13.82 +8.57 +16.39    -3.63 -5.11  -2.51   -5.32  -4.53  -0.76
//	SOL     -8.67 -15.48 -19.75   +2.10 +2.48  +8.47   -4.35 -13.97  -8.32
//	SUI     -9.59 -12.46 -17.89   +0.50 -0.69  +2.36   +1.52  -3.74  -3.46
//	NEAR    +2.20 +12.45 +16.39   -5.22 -8.69 -11.78   -0.59  +9.21 +10.23
//
// The arm lands BETWEEN the two baselines everywhere and beats neither. That
// is not bad luck; it is what the trade counts say happened. On BTC 60d MR
// fires 52 times, SM 12, and the gated arm 49 — so gating does not split the
// book, it swaps a handful of MR entries for a smaller number of SM ones. The
// swap was negative on four of five symbols.
//
// ETH is the sharp case and it inverts the premise. Swapping roughly five
// entries moved the 60d window from +13.82 to -5.32. The MR trades that occur
// inside a CONFIRMED trend are ETH's best trades — buying the pullback in an
// uptrend is mean reversion, and routing exactly those bars to momentum
// deleted the edge. "MR for ranges, momentum for trends" is not true here.
//
// Two structural reasons, both known in advance and both worth stating rather
// than rediscovering:
//   - The gate is LATE by construction. A trend is confirmed only after the
//     swings that define it, so the switch happens after the move it was meant
//     to catch, and is least reliable at the turn — which is where both
//     strategies are worst.
//   - The costs are asymmetric. Deleting a good MR trade costs more than
//     adding a mediocre SM trade gains, so a selector has to be RIGHT, not
//     merely better than chance, to break even.
//
// A regime signal that is measurable, real, and even predictive is still not
// evidence that switching on it helps — same lesson as the cash-open bias.
var StructMomentumRegime = false

// regimeWantsMomentum reports whether the latest closed bar sits in a
// confirmed directional structure.
//
// Known cost, stated rather than discovered later: a trend is only confirmed
// AFTER the swings that define it, so the gate is late by construction, and it
// is latest exactly at a regime turn — which is where both strategies are at
// their worst. If this arm fails, that is the first place to look.
func regimeWantsMomentum(candles []market.Candle) bool {
	return AnalyzeStructure(candles, 0).Trend != StructNeutral
}

// strategyFor returns the strategy a (symbol, timeframe) runs. Per-(symbol,TF)
// allowlist — the A/B showed the StructMomentum edge is BOTH symbol- and
// TF-specific, so assignment must be per-pair. Everything defaults to MR; a
// pair opts into StructMomentum ONLY after clearing its A/B gate.
//
// Forward-log only: none of these are in market.All(), so this changes no
// daemon/live-core behaviour. It DOES steer autotrade.json rules whose
// strategy is "engine", which is how HYPE 1h ended up running a disproven
// strategy in paper.
//
// ── Phase-2 validation, 2026-09-03 (SM vs MR, netR per window 60/90/120d) ───
//
// Re-run required building signal.StructMomentumOff first: once a pair is IN
// this allowlist, the baseline arm runs SM too, so an SM-vs-MR A/B on an
// assigned pair is INERT and both arms emit byte-identical output. The 2026-08
// numbers below predate the assignment, when the baseline genuinely was MR.
// Gate = SM must beat MR in EVERY window.
//
//	SOL  1h  MR -19.37/-21.17/-20.10  ->  SM  +3.65/+2.48/+8.47   PASS 3/3  KEPT
//	SUI  1h  MR  -6.10/ -8.13/-18.05  ->  SM  -2.44/-2.44/-0.69   PASS 3/3  KEPT*
//	LINK 1h  MR  -0.08/ +5.69/ -4.05  ->  SM  -3.66/+2.22/+0.71   1/3       REMOVED
//	HYPE 1h  MR  +1.35/ +4.22/ -1.28  ->  SM  -0.61/-1.90/-1.94   0/3       REMOVED
//
// SOL 1h is the strongest result in the file, and it is really a finding about
// MR: -19 to -21R across all three windows on 36-75 trades is not noise, it is
// mean-reversion bleeding on that pair. SM fires ~1/3 as often (0.19-0.23
// trades/day vs 0.60-0.62) for far better R/trade.
//
// *SUI 1h passes the gate while being NEGATIVE in every window. It wins only
// because MR is catastrophic there. Better-than-a-disaster is not an edge — a
// purely relative gate cannot express that, and the honest reading is that SUI
// 1h should probably be traded by neither engine. Kept because removing it
// would silently hand those bars back to the worse of the two; revisit with a
// minimum-absolute-netR gate rather than by picking a winner.
//
// LINK's earlier note claimed it "cleared strongly" on 1h/2h. Not a
// contradiction of that test: it ran on 2026-08-22 windows, before LINK was
// assigned, so its baseline was real. The re-run uses windows ending
// 2026-09-03. Two valid tests disagreeing across a two-week shift is itself
// the finding — LINK 1h is regime-dependent, not a stable edge, which is
// exactly what the every-window gate exists to reject.
//
// 2h assignments are UNDECIDED, not passing: SM fires 3-14 times per 2h window
// (SOL 4/6/12, LINK 3/6/11), and calling n=3 a window loss is reading noise.
// Left in place with the uncertainty stated rather than churned on thin data.
// NEAR stays MR on all TFs (it A/B'd as a mean-reversion symbol). BTC/ETH stay
// MR pending their own forward-log decision.
//
// The alt expansion (XRP/NEAR/…) remains BLOCKED: SM is validated on exactly
// one pair out of six tested. One pass in six is not a strategy ready to grow.
func strategyFor(sym market.Symbol, tf market.Timeframe) StrategyKind {
	switch sym {
	case market.SOLUSDT:
		// 1h PASS 3/3. 2h undecided (n=4/6/12) — kept, not validated.
		if tf == "1h" || tf == "2h" {
			return StrategyStructMomentum
		}
	case market.LINKUSDT:
		// 1h REMOVED 2026-09-03 (1/3). 2h undecided (n=3/6/11) — kept.
		if tf == "2h" {
			return StrategyStructMomentum
		}
	case market.SUIUSDT:
		// 1h PASS 3/3 but negative in every window; see the note above.
		if tf == "1h" {
			return StrategyStructMomentum
		}
	}
	// HYPE removed entirely 2026-09-03: 1h was its only assignment and it
	// FAILED 0/3 with adequate n (11/18/23). Its "engine" autotrade rule now
	// falls back to MR, which beat SM in every window.
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
