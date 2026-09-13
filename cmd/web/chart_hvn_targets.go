package main

// hvnTargetPayload serialises A3's HVN auto-targets for the chart's plan card.
//
// The value is not the list of nodes — it is the OBSTACLE count. A target four
// nodes away with +10R reads better than one at +4.7R with clear air, and it
// is the worse trade; showing R without the obstacles in front of it is how
// that mistake gets made. Checked against the two orders resting on
// 2026-09-05: BTC's first clean node sat at +4.73R while the plan's own TP2
// was BEYOND it for only 1.1R more, and ETH had no clean node at all.

import (
	"github.com/henry190927/trading-bot/signal"
)

// minHVNTargetR drops nodes too close to entry to be worth naming. A "target"
// under half a risk unit is noise — but it still counts as an obstacle for
// everything behind it, which HVNTargets handles.
const minHVNTargetR = 0.5

func hvnTargetPayload(sig signal.Signal, entry, stop float64) map[string]any {
	ahead, total := signal.NodesAhead(sig.VP, sig.Side, entry)
	ts := signal.HVNTargets(sig.VP, sig.Side, entry, stop, minHVNTargetR)

	// An empty list has two very different causes and the UI must not render
	// them the same. Live BTC on 2026-09-05: plan LONG at 79,464 while every
	// node sat at 77.7-78.2k, so there was nothing above at all — "clear air"
	// is a read, and showing a blank panel would have buried it.
	if len(ts) == 0 {
		switch {
		case total == 0:
			return map[string]any{"note": "no volume profile"}
		case ahead == 0:
			return map[string]any{"note": "clear air — no volume node ahead of entry", "clearAhead": true}
		default:
			return map[string]any{"note": "every node ahead is under the minimum R", "nodesAhead": ahead}
		}
	}
	clean := signal.FirstCleanHVNTarget(ts)
	list := make([]map[string]any, 0, len(ts))
	for i := range ts {
		t := ts[i]
		list = append(list, map[string]any{
			"price":     t.Price,
			"r":         t.R,
			"obstacles": t.Obstacles,
			"isPOC":     t.IsPOC,
			// clean marks the one worth pre-filling a TP field with.
			"clean": clean != nil && clean.Price == t.Price,
		})
	}
	res := map[string]any{"targets": list, "nodesAhead": ahead}
	if clean == nil {
		// Worth saying out loud: every candidate has something in front of
		// it, which is a different trade from one with a clear run.
		res["note"] = "no unobstructed target — every node ahead sits behind another"
	}
	return res
}
