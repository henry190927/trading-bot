package signal

import (
	"math"

	"github.com/henry190927/trading-bot/market"
)

// PerSymbolStopBuffer widens the stop by `fraction * original_risk` for the
// listed symbols. Entry is left unchanged; TakeProfit levels re-derive at
// 1R / 2R from the new (wider) risk so R-multiple semantics stay clean.
//
// Defaults (2026-06-03): XAU and XAG get +0.5R. The 60/90/120d backtest
// A/B in this session showed these two symbols have ~42-47% reclaim rates
// after stop-outs — wider stops survive a meaningful fraction of stop
// hunts and yield +42.7R (XAU) and +18.8R (XAG) per 90 days. BTC and ETH
// have lower reclaim rates (~34%) and the same buffer is net negative for
// them, so they stay at 0.
//
// The map is a package var (not a const) so backtest CLIs can mutate it
// for A/B comparisons (e.g. clear to test the pre-buffer baseline).
var PerSymbolStopBuffer = map[market.Symbol]float64{
	market.XAUUSDT: 0.5,
	market.XAGUSDT: 0.5,
}

// applyPerSymbolStopBuffer mutates the plan in place. Called from
// Evaluate after BuildPlan. No-op when:
//   - Plan has no entry or no stop
//   - The plan's symbol has no entry in PerSymbolStopBuffer
//   - The configured fraction is <= 0
func applyPerSymbolStopBuffer(plan *Plan, side Side, sym market.Symbol) {
	bufferR, ok := PerSymbolStopBuffer[sym]
	if !ok || bufferR <= 0 {
		return
	}
	if plan.Entry == 0 || plan.StopLoss == 0 || side == Flat {
		return
	}
	origRisk := math.Abs(plan.StopLoss - plan.Entry)
	if origRisk == 0 {
		return
	}
	extra := origRisk * bufferR
	switch side {
	case Long:
		plan.StopLoss -= extra
	case Short:
		plan.StopLoss += extra
	}
	// Re-derive TPs from the new (wider) risk so a "1R win" still means
	// price moved by the trade's actual risk distance.
	newRisk := math.Abs(plan.StopLoss - plan.Entry)
	if newRisk > 0 {
		switch side {
		case Long:
			plan.TakeProfit = []float64{plan.Entry + newRisk, plan.Entry + 2*newRisk}
		case Short:
			plan.TakeProfit = []float64{plan.Entry - newRisk, plan.Entry - 2*newRisk}
		}
	}
}
