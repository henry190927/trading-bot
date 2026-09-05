package main

// shelfBands serialises the swing-shelf layer for /chart (A2, second half).
//
// The EQH/EQL "bands" layer next to it is same-side liquidity — a sweep
// target. A shelf is a band price has pivoted around in BOTH directions,
// which is what support/resistance actually means, and a same-side clusterer
// cannot tell the two apart.
//
// Display only: this feeds no vote and no gate. Every pool-derived scoring
// idea so far has been rejected by A/B, so a new band detector ships as a
// drawing until it earns more through cmd/gate.

import (
	"strconv"

	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

const (
	// 15bps, matching the EQH/EQL layer so the two agree on what "the same
	// price" means.
	shelfTolPct = 0.0015
	// 3 pivots minimum. At 2 the output roughly doubles and fills with
	// one-high-one-low pairs that are coincidence rather than structure.
	shelfMinTouches = 3
	// Within 5% of price, same window the pools layer uses. Raw detection
	// returned a BTC band 20% away with a 16-bar span, which is not
	// support or resistance for the decision in front of you.
	shelfMaxDistPct = 0.05
	// Six is what fits on a chart before the reader loses the two that
	// matter. Raw detection returns ~15.
	shelfMaxDrawn = 6
)

func shelfBands(candles []market.Candle, price float64) []map[string]any {
	if len(candles) < 30 || price <= 0 {
		return nil
	}
	raw := signal.FindShelves(candles, 2, 0, shelfTolPct, shelfMinTouches)
	top := signal.TopShelves(raw, price, shelfMaxDistPct, shelfMaxDrawn)
	if len(top) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(top))
	for _, s := range top {
		out = append(out, map[string]any{
			"price":   s.Price,
			"lo":      s.Lo,
			"hi":      s.Hi,
			"highs":   s.Highs,
			"lows":    s.Lows,
			"touches": s.Touches(),
			"span":    s.Span(),
			// Pre-rendered so the front end doesn't re-derive the label and
			// drift from what the ranking actually used.
			"label": shelfLabel(s),
		})
	}
	return out
}

// shelfLabel names the two things that decide the ranking: how two-sided the
// band is, and how long it has mattered.
func shelfLabel(s signal.Shelf) string {
	return "H" + strconv.Itoa(s.Highs) + "/L" + strconv.Itoa(s.Lows) + " · " + strconv.Itoa(s.Span()) + " bars"
}
