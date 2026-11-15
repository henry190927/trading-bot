package autostrat

import (
	"math"
	"testing"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// The executor sees only the newest htfTrendLiveBars closed bars; trendbt
// sees the whole history. At the same bar they must take the same trade:
// same fire, side, entry and signal bar, and a stop equal up to the ATR
// seed's residual weight ((19/20)^480 ≈ 2e-11 relative).
func TestHTFTrendLiveWindowParity(t *testing.T) {
	p := DefaultHTFTrendParams()
	rule := autotrade.Rule{Strategy: "htf-trend", TF: "4h", Side: "auto"}
	for _, full := range [][]market.Candle{loadBingXSlice(t), randomWalk(1500, 7), randomWalk(1500, 8)} {
		atr := indicator.ATR(full, p.ATRPeriod)
		fires := 0
		for k := htfTrendLiveBars - 1; k < len(full); k++ {
			want := HTFTrendTrigger(full, atr[k], k, true, true, p)
			got := HTFTrendOnWindow(full[k-htfTrendLiveBars+1:k+1], rule)
			if got.Fire != want.Fire || got.Side != want.Side || got.Entry != want.Entry || got.TrailM != want.TrailM || !got.BarTime.Equal(want.BarTime) {
				t.Fatalf("bar %d: live %+v, full-history %+v", k, got, want)
			}
			if want.Fire {
				fires++
				if math.Abs(got.Stop-want.Stop) > 1e-9*want.Entry {
					t.Fatalf("bar %d: live stop %v, full-history %v", k, got.Stop, want.Stop)
				}
				if got.TP != 0 || got.TrailM != p.TrailM || !got.Market {
					t.Fatalf("bar %d: %+v, want a market fire with no TP and TrailM %d", k, got, p.TrailM)
				}
			}
		}
		if fires == 0 {
			t.Fatal("no fire in the comparable range — the test compared nothing")
		}
	}
	// Side filters still apply on the window path.
	cs := randomWalk(600, 7)
	for k := htfTrendLiveBars - 1; k < len(cs); k++ {
		w := cs[k-htfTrendLiveBars+1 : k+1]
		if tr := HTFTrendOnWindow(w, autotrade.Rule{Side: "long"}); tr.Fire && tr.Side != "long" {
			t.Fatalf("side long fired %s", tr.Side)
		}
		if tr := HTFTrendOnWindow(w, autotrade.Rule{Side: "short"}); tr.Fire && tr.Side != "short" {
			t.Fatalf("side short fired %s", tr.Side)
		}
	}
}
