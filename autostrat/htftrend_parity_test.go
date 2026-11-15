package autostrat

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// PREFIX PARITY (docs/htf_trend_executor_design.md §5, P2). The backtest scores
// a trade once, over the whole series; the paper executor and the panel score
// the same fire again every minute over whatever has closed so far (and the
// live trail manager will too). They must agree at EVERY bar, not just at the
// end: for each trade SimulateHTFTrend finds, build the fire the executor
// would have logged, and for every prefix cs[:k+1] with k >= EntryIdx
//
//   - k < ExitIdx (or the trade is still open): EvaluateTrailFire says open,
//     CurStop is the stop for bar k+1, UnrealR is the mark to Close[k];
//   - k >= ExitIdx: stop, at the same exit price, exit time and gross R.
//
// The reference stop for bar k+1 is computed here by a naive loop written
// independently of autotrade.TrailStop, so a bug shared by the backtest and
// the resolver (they run the same primitive since P1) is still caught against
// the rules in docs/htf_trend_design.md §2.

type parityCase struct {
	name string
	cs   []market.Candle
	atr  []float64
	p    HTFTrendParams
	warm int
}

func loadBingXSlice(t *testing.T) []market.Candle {
	t.Helper()
	// BTC-USDT 4h, 600 closed bars 2026-08-07 → 2026-11-15, public BingX
	// klines as cmd/trendbt fetched them (the Sep–Oct range and the Nov drop).
	b, err := os.ReadFile("testdata/btc_4h_bingx_slice.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []market.Candle
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func parityCases(t *testing.T) []parityCase {
	def := DefaultHTFTrendParams()
	var out []parityCase
	real := loadBingXSlice(t)
	out = append(out, parityCase{"bingx BTC 4h", real, indicator.ATR(real, def.ATRPeriod), def, 21})
	for seed := int64(1); seed <= 4; seed++ {
		cs := randomWalk(500, seed)
		out = append(out, parityCase{"random walk", cs, indicator.ATR(cs, def.ATRPeriod), def, 21})
	}
	// Short channels make many short trades, and the hand fixture has a gap
	// through the stop (exit at the open).
	cs := randomWalk(300, 99)
	out = append(out, parityCase{"random walk M2", cs, constATR(len(cs), 2), fixtureParams(), 3})
	hand := trendFixture()
	out = append(out, parityCase{"hand fixture (gap)", hand, constATR(len(hand), 1), fixtureParams(), 0})
	return out
}

// naiveStops returns stop[j] = the stop in force during bar j, for j in
// (entryIdx, len(cs)], by the written rule: start at the initial stop; after
// each closed bar j >= entryIdx, ratchet toward the opposite M-bar extreme.
func naiveStops(cs []market.Candle, entryIdx int, long bool, init float64, m int) map[int]float64 {
	out := map[int]float64{}
	s := init
	for j := entryIdx; j < len(cs); j++ {
		ext := cs[j].Low
		if !long {
			ext = cs[j].High
		}
		for q := j - m + 1; q <= j; q++ {
			if long && cs[q].Low < ext {
				ext = cs[q].Low
			}
			if !long && cs[q].High > ext {
				ext = cs[q].High
			}
		}
		if long && ext > s {
			s = ext
		}
		if !long && ext < s {
			s = ext
		}
		out[j+1] = s
	}
	return out
}

func TestHTFTrendPrefixParity(t *testing.T) {
	total := 0
	for _, pc := range parityCases(t) {
		trades := SimulateHTFTrend(pc.cs, pc.atr, pc.warm, true, true, pc.p, 0)
		if len(trades) < 2 {
			t.Fatalf("%s: only %d trades, fixture too quiet to test anything", pc.name, len(trades))
		}
		for ti, tr := range trades {
			if tr.EntryIdx < pc.p.TrailM-1 {
				t.Fatalf("%s trade %d: entry %d inside the trail lookback", pc.name, ti, tr.EntryIdx)
			}
			long := tr.Side == "long"
			ref := naiveStops(pc.cs, tr.EntryIdx, long, tr.InitStop, pc.p.TrailM)
			// The backtest's own exit, checked against the written rule.
			if !tr.Open {
				st, b := ref[tr.ExitIdx], pc.cs[tr.ExitIdx]
				want := math.Min(st, b.Open)
				if !long {
					want = math.Max(st, b.Open)
				}
				if tr.Exit != want {
					t.Fatalf("%s trade %d: backtest exit %v, rule says %v (stop %v, open %v)", pc.name, ti, tr.Exit, want, st, b.Open)
				}
			}
			withBar := autotrade.PaperFire{
				Time: tr.EntryTime.Add(time.Minute), BarTime: tr.EntryTime, TF: "4h", Symbol: "X", Strategy: "htf-trend",
				Side: tr.Side, Market: true, Entry: tr.Entry, Stop: tr.InitStop, TrailM: pc.p.TrailM,
			}
			noBar := withBar // a row without BarTime takes the newest-bar-at-Time fallback
			noBar.BarTime = time.Time{}
			for _, f := range []autotrade.PaperFire{withBar, noBar} {
				for k := tr.EntryIdx; k < len(pc.cs); k++ {
					oc := autotrade.EvaluateTrailFire(f, pc.cs[:k+1])
					total++
					if tr.Open || k < tr.ExitIdx {
						if oc.Status != autotrade.OutOpen {
							t.Fatalf("%s trade %d prefix %d: %s, want open (exit at %d)", pc.name, ti, k, oc.Status, tr.ExitIdx)
						}
						if oc.CurStop != ref[k+1] {
							t.Fatalf("%s trade %d prefix %d: CurStop %v, want %v", pc.name, ti, k, oc.CurStop, ref[k+1])
						}
						if want := tr.MTMR(pc.cs, k) + tr.FeeR; k > tr.EntryIdx && math.Abs(oc.UnrealR-want) > 1e-12 {
							t.Fatalf("%s trade %d prefix %d: UnrealR %v, want %v", pc.name, ti, k, oc.UnrealR, want)
						}
						continue
					}
					if oc.Status != autotrade.OutStop || oc.ExitPrice != tr.Exit || !oc.ExitAt.Equal(tr.ExitTime) || oc.NetR != tr.GrossR {
						t.Fatalf("%s trade %d prefix %d: %s exit %v at %s R %v, want stop %v at %s R %v",
							pc.name, ti, k, oc.Status, oc.ExitPrice, oc.ExitAt, oc.NetR, tr.Exit, tr.ExitTime, tr.GrossR)
					}
					if oc.BarsHeld != tr.BarsHeld {
						t.Fatalf("%s trade %d: BarsHeld %d, want %d", pc.name, ti, oc.BarsHeld, tr.BarsHeld)
					}
				}
			}
		}
	}
	if total < 5000 {
		t.Errorf("only %d prefix evaluations — the fixtures stopped exercising the walk", total)
	}
}
