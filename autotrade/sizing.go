package autotrade

import (
	"fmt"
	"math"
)

// Position sizing for the auto-executor.
//
// # WHY THIS EXISTS (2026-10-16)
//
// Every rule used to open margin_usdt × leverage regardless of where the stop
// was — 35u at 100-125x, so 3,500-4,375u of notional per fire. Two things
// followed, both measured on the last 120 paper fires:
//
//  1. The stop was usually BEYOND liquidation. At 125x an isolated position
//     is liquidated about 0.8% away; SOL sweep-reject stops ran 0.41-1.86%,
//     SUI 0.60-3.85% at 100x. 20 of 22 SUI fires and 13 of 14 BTC range-edge
//     fires had a stop the position could never reach. Live, those trades
//     lose the whole margin before the stop, and the paper log — which
//     assumes every trade survives to its stop — scores them as -1R or, worse,
//     as wins that only dipped first.
//  2. One stop cost 10-17% of a ~400u account (NEAR once 16%), so 1R had no
//     fixed meaning in money and the -6R daily halt was half the account.
//
// So sizing now starts from the stop: risk a fixed fraction of equity, derive
// notional from the stop distance, and pick a leverage low enough that the
// stop comes well before liquidation.

// LiqStopMaxFrac is how much of the liquidation distance a stop may use. An
// isolated position at L× is liquidated roughly 1/L away, minus maintenance
// margin and fees, so a stop at 1/L is already too late. Half leaves room for
// both plus slippage on a stop-market fill. Not configurable on purpose: it is
// the guard, not a preference.
const LiqStopMaxFrac = 0.5

// Sizing is the order the executor may place for one fire, or why it may not.
type Sizing struct {
	Blocked bool
	Reason  string // "" when not blocked

	Qty      float64 // base-asset units
	Notional float64 // USDT
	Margin   float64 // USDT committed = Notional / Lev
	Lev      int
	RiskUSDT float64 // loss if the stop fills at its price
	StopPct  float64 // |entry-stop| / entry, as a percent
	Equity   float64 // equity the size was computed from (0 in fixed-margin mode)
}

// MaxLevForStop is the highest leverage at which a stop stopPct (a fraction,
// 0.01 = 1%) away still sits inside LiqStopMaxFrac of the liquidation
// distance. 0 means no leverage, not even 1x, is safe for that stop.
func MaxLevForStop(stopFrac float64) int {
	if stopFrac <= 0 {
		return 0
	}
	// Tiny epsilon so a stop of exactly 0.5% gives 100, not 99 from 0.5/0.005
	// landing at 99.99999.
	return int(math.Floor(LiqStopMaxFrac/stopFrac + 1e-9))
}

// Size decides quantity and leverage for one fire.
//
// Risk mode (cfg.RiskPerTradePct > 0): risk = equity × pct, notional =
// risk / stop distance, leverage = min(rule leverage, MaxLevForStop). The
// rule's margin_usdt is not used; margin falls out of notional / leverage
// (= risk / (stop distance × leverage)). It is about 2 × risk when the stop
// sets the leverage and larger when the rule's ceiling does — a 0.1% stop at
// 125x commits 8 × risk — because notional scales with 1 / stop distance.
// The LOSS is what is fixed, not the margin or the exposure.
//
// Fixed-margin mode (pct 0, the pre-2026-10-16 behaviour): margin_usdt ×
// leverage as before, but a fire whose stop is past the liquidation guard is
// BLOCKED rather than placed — lowering the leverage at the same margin would
// silently change the risk, so the honest answer is no.
//
// Equity unreadable in risk mode BLOCKS. That is the opposite of package
// risk's allow-and-warn posture, and deliberately: risk only grades an order
// that is already sized, whereas here there is nothing to size from, and the
// only fallback — fixed margin — is the thing this replaces.
func Size(cfg Config, r Rule, entry, stop, equity float64, equityKnown bool) Sizing {
	if entry <= 0 || stop <= 0 || entry == stop {
		return Sizing{Blocked: true, Reason: fmt.Sprintf("sizing: no usable stop (entry %g, stop %g)", entry, stop)}
	}
	stopFrac := math.Abs(entry-stop) / entry
	s := Sizing{StopPct: stopFrac * 100}
	maxLev := MaxLevForStop(stopFrac)

	if cfg.RiskPerTradePct <= 0 {
		s.Lev = r.Leverage
		s.Margin = r.MarginUSDT
		s.Notional = r.MarginUSDT * float64(r.Leverage)
		s.Qty = s.Notional / entry
		s.RiskUSDT = s.Notional * stopFrac
		if r.Leverage > maxLev {
			s.Blocked = true
			s.Reason = fmt.Sprintf("liq-guard: stop %.2f%% away, but at %dx liquidation is ~%.2f%% away — max %dx keeps the stop inside half of it",
				s.StopPct, r.Leverage, 100/float64(r.Leverage), maxLev)
		}
		return s
	}

	if !equityKnown || equity <= 0 {
		s.Blocked = true
		s.Reason = fmt.Sprintf("sizing: account equity unreadable (known=%v, %.2fu) — cannot size by risk", equityKnown, equity)
		return s
	}
	lev := min(r.Leverage, maxLev)
	if lev < 1 {
		s.Blocked = true
		s.Reason = fmt.Sprintf("liq-guard: stop %.2f%% away is too wide even at 1x", s.StopPct)
		return s
	}
	s.Equity = equity
	s.RiskUSDT = equity * cfg.RiskPerTradePct / 100
	s.Notional = s.RiskUSDT / stopFrac
	s.Lev = lev
	s.Margin = s.Notional / float64(lev)
	s.Qty = s.Notional / entry
	return s
}
