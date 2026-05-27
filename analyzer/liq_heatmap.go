package analyzer

import "sort"

// LiquidationCluster is an approximated price band where leveraged positions
// would be liquidated. Source ideally is a real liquidation feed (Coinglass,
// exchange-aggregated WS); this package only does the clustering math.
type LiquidationCluster struct {
	Price    float64
	Notional float64 // sum of position notional liquidated at this price band
	Side     string  // "long" or "short"
}

// Provider abstracts where liquidation data comes from. Implementations:
//   - coinglass.Provider — paid API, most accurate
//   - approx.Provider     — estimate from OI + funding rate (rough)
//   - mock.Provider       — for tests
type Provider interface {
	// Heatmap returns clusters within `priceRange` of `refPrice`.
	Heatmap(symbol string, refPrice, priceRange float64) ([]LiquidationCluster, error)
}

// Cluster bins raw liquidation events by price band of width `binWidth`.
func Cluster(events []LiquidationCluster, binWidth float64) []LiquidationCluster {
	if binWidth <= 0 || len(events) == 0 {
		return nil
	}
	type key struct {
		bin  int
		side string
	}
	m := map[key]float64{}
	for _, e := range events {
		k := key{bin: int(e.Price / binWidth), side: e.Side}
		m[k] += e.Notional
	}
	out := make([]LiquidationCluster, 0, len(m))
	for k, n := range m {
		out = append(out, LiquidationCluster{
			Price:    (float64(k.bin) + 0.5) * binWidth,
			Notional: n,
			Side:     k.side,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Notional > out[j].Notional })
	return out
}
