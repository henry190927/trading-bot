package signal

import (
	"fmt"

	"myFirstGo/trading-bot/analyzer"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
)

// StopRefineEnabled toggles the stop-refinement logic. When true, BuildPlan
// will push the provisional stop past any HVN or equal-highs/lows cluster
// that sits inside a 0.5×ATR buffer zone beyond it, to avoid being swept
// out by obvious stop-hunt flows.
//
// **Default OFF.** Backtest showed wider stops shrink R-multiples within the
// 24-bar hold window — more timeouts at small loss, fewer 2R winners. The
// in-theory live benefit (escaping real stop hunts) cannot be modeled by
// the R-based simulator (no slippage, no wick-then-reverse patterns), so we
// default-disable and let the trader opt in if they observe live edge.
var StopRefineEnabled = false

// stopRefineSearchATR is how far past the provisional stop we scan for
// obstacles. Wider → catches more potential sweep targets but may compound
// adjustments unnecessarily.
const stopRefineSearchATR = 0.5

// stopRefinePushATR is the buffer applied past a detected obstacle when
// moving the stop. Should be small — just enough to avoid the obstacle's
// own noise wicks.
const stopRefinePushATR = 0.3

// stopRefineMaxWidenATR caps total widening from the original provisional
// stop, in ATR units. Prevents runaway adjustments when many obstacles
// cluster below/above (which would compress fee-R-ratio per trade).
const stopRefineMaxWidenATR = 1.0

// refineStopLoss takes a provisional stop, scans for nearby "obstacles"
// (HVN price levels and equal-highs/lows clusters within stopRefineSearchATR
// past the stop), and pushes the stop just past the nearest one + a small
// buffer. Returns the refined stop and a human-readable note (empty if no
// adjustment was made).
//
// Rationale (清算區 / 流動性獵取): obvious stop locations attract sweep flows
// designed to take them out before the real move. Placing the stop *past*
// the next visible obstacle costs slightly more risk per trade but markedly
// improves survival rate on mean-reversion setups.
func refineStopLoss(side Side, provisionalStop, atr float64,
	candles []market.Candle, vp indicator.VolumeProfile) (float64, string) {

	if !StopRefineEnabled || atr <= 0 || len(candles) < 20 {
		return provisionalStop, ""
	}

	obstacles := gatherObstacles(candles, vp)
	if len(obstacles) == 0 {
		return provisionalStop, ""
	}

	searchBuffer := stopRefineSearchATR * atr
	pushBuffer := stopRefinePushATR * atr
	maxWiden := stopRefineMaxWidenATR * atr

	var bestObstacle float64
	found := false

	if side == Long {
		// Long stop sits BELOW entry. Scan downward from provisional stop.
		searchMin := provisionalStop - searchBuffer
		for _, o := range obstacles {
			if o <= provisionalStop && o >= searchMin {
				if !found || o < bestObstacle {
					bestObstacle = o
					found = true
				}
			}
		}
		if !found {
			return provisionalStop, ""
		}
		newStop := bestObstacle - pushBuffer
		// Cap maximum widening from the *original* provisional stop.
		minAllowed := provisionalStop - maxWiden
		if newStop < minAllowed {
			newStop = minAllowed
		}
		if newStop >= provisionalStop {
			return provisionalStop, ""
		}
		widening := provisionalStop - newStop
		return newStop, fmt.Sprintf(
			"Stop widened past obstacle @ %.4f → %.4f (+%.2fR risk to avoid sweep)",
			bestObstacle, newStop, widening/atr)
	}

	// Short stop sits ABOVE entry. Scan upward from provisional stop.
	searchMax := provisionalStop + searchBuffer
	for _, o := range obstacles {
		if o >= provisionalStop && o <= searchMax {
			if !found || o > bestObstacle {
				bestObstacle = o
				found = true
			}
		}
	}
	if !found {
		return provisionalStop, ""
	}
	newStop := bestObstacle + pushBuffer
	maxAllowed := provisionalStop + maxWiden
	if newStop > maxAllowed {
		newStop = maxAllowed
	}
	if newStop <= provisionalStop {
		return provisionalStop, ""
	}
	widening := newStop - provisionalStop
	return newStop, fmt.Sprintf(
		"Stop widened past obstacle @ %.4f → %.4f (+%.2fR risk to avoid sweep)",
		bestObstacle, newStop, widening/atr)
}

// gatherObstacles collects HVN levels + equal-highs/lows clusters from the
// recent candles as a flat slice of price levels to scan.
func gatherObstacles(candles []market.Candle, vp indicator.VolumeProfile) []float64 {
	out := make([]float64, 0, 16)
	out = append(out, vp.HVN...)

	lookback := 80
	if len(candles) < lookback {
		lookback = len(candles)
	}
	recent := candles[len(candles)-lookback:]
	out = append(out, analyzer.FindEqualLevels(market.Highs(recent), 0.0005, 2)...)
	out = append(out, analyzer.FindEqualLevels(market.Lows(recent), 0.0005, 2)...)

	return out
}
