package signal

// HVN auto-targets — A3.
//
// A high-volume node is where price has spent time, so it is where price
// stalls: HVNs are simultaneously the natural place to take profit and the
// thing standing between you and a further one. Listing the nodes above entry
// is the easy half and not the useful half — what decides whether a target is
// reachable is HOW MANY nodes sit in the way, which is why Obstacles is here.
//
// DISPLAY ONLY. This suggests targets for a human to pick; it does not change
// BuildPlan, and it votes nothing. Wiring an HVN target into the engine's TP
// would be a strategy change needing cmd/gate — and note the liquidity-TP A/B
// of 2026-08-27 was REJECTED, so "target the obvious level" is not a free win
// in this system.

import (
	"math"
	"sort"

	"myFirstGo/trading-bot/indicator"
)

// HVNTarget is one candidate take-profit at a volume node.
type HVNTarget struct {
	Price float64
	// R is the reward in units of the trade's own risk. Computed from the
	// caller's entry and stop, so it is directly comparable to the plan's
	// TP1/TP2 rather than being an abstract distance.
	R float64
	// Obstacles counts HVNs strictly between entry and this target. Zero
	// means clear air; two means price has to chew through two shelves of
	// accumulated volume to get here, which is the honest reason a distant
	// high-R target is not the same trade as a near one.
	//
	// Counted over ALL nodes, including ones too close to be targets
	// themselves — a node at 0.4R still stalls price even though nobody
	// would aim at it.
	Obstacles int
	// IsPOC marks the point of control, the single heaviest node. It is the
	// strongest magnet and the strongest obstacle.
	IsPOC bool
}

// HVNTargets returns the volume nodes in the trade's profit direction, nearest
// first, each annotated with its R and how many nodes precede it.
//
// minR drops nodes too close to entry to be worth naming (a "target" at 0.3R
// is noise). Pass 0 to keep them all. Nodes on the losing side are never
// returned — those are stop-side context, a different question.
func HVNTargets(vp indicator.VolumeProfile, side Side, entry, stop, minR float64) []HVNTarget {
	risk := math.Abs(entry - stop)
	if entry <= 0 || risk <= 0 || (side != Long && side != Short) {
		return nil
	}

	// DEDUPE FIRST. BuildVolumeProfile can return the same price twice in
	// HVN (observed on live BTC 1h: 77708.067 appeared twice), and a repeated
	// price would count as two obstacles for everything behind it when it is
	// one level. Obstacles is the whole point of this function, so an inflated
	// count is not cosmetic.
	var nodes []float64
	for _, n := range vp.HVN {
		if !containsPrice(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	if vp.POC > 0 && !containsPrice(nodes, vp.POC) {
		// The POC is not guaranteed to be in the top-N HVN list (HVN is
		// ranked by volume and truncated), and leaving out the single
		// heaviest node would understate every obstacle count.
		nodes = append(nodes, vp.POC)
	}

	// Profit-side only, ordered by distance from entry.
	var ahead []float64
	for _, n := range nodes {
		if n <= 0 {
			continue
		}
		if (side == Long && n > entry) || (side == Short && n < entry) {
			ahead = append(ahead, n)
		}
	}
	if len(ahead) == 0 {
		return nil
	}
	sort.Slice(ahead, func(i, j int) bool {
		return math.Abs(ahead[i]-entry) < math.Abs(ahead[j]-entry)
	})

	out := make([]HVNTarget, 0, len(ahead))
	for i, n := range ahead {
		r := math.Abs(n-entry) / risk
		if minR > 0 && r < minR {
			continue
		}
		out = append(out, HVNTarget{
			Price:     n,
			R:         r,
			Obstacles: i, // everything nearer than this node is in the way
			IsPOC:     vp.POC > 0 && math.Abs(n-vp.POC) < 1e-9,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FirstCleanHVNTarget returns the nearest target with nothing in the way, or
// nil. This is the one worth pre-filling a TP field with: further nodes may
// offer more R, but each obstacle is a place the move can end.
func FirstCleanHVNTarget(ts []HVNTarget) *HVNTarget {
	for i := range ts {
		if ts[i].Obstacles == 0 {
			return &ts[i]
		}
	}
	return nil
}

func containsPrice(xs []float64, v float64) bool {
	for _, x := range xs {
		if math.Abs(x-v) < 1e-9 {
			return true
		}
	}
	return false
}

// NodesAhead reports how many DISTINCT volume nodes sit in the trade's profit
// direction, regardless of minR. Zero with a non-empty profile means clear
// air, which is a finding — a caller that renders an empty target list as
// "no data" hides it.
func NodesAhead(vp indicator.VolumeProfile, side Side, entry float64) (ahead, total int) {
	var nodes []float64
	for _, n := range append(append([]float64(nil), vp.HVN...), vp.POC) {
		if n > 0 && !containsPrice(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	for _, n := range nodes {
		if (side == Long && n > entry) || (side == Short && n < entry) {
			ahead++
		}
	}
	return ahead, len(nodes)
}
