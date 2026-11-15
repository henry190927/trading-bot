package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func walk4h(n int, seed int64) []market.Candle {
	r := rand.New(rand.NewSource(seed))
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, n)
	px := 100.0
	for i := range cs {
		o := px
		c := o * (1 + r.NormFloat64()*0.02 + 0.002*math.Sin(float64(i)/25))
		ot := t0.Add(time.Duration(i) * 4 * time.Hour)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(4*time.Hour - time.Millisecond), Open: o,
			High: math.Max(o, c) * (1 + r.Float64()*0.01), Low: math.Min(o, c) * (1 - r.Float64()*0.01), Close: c}
		px = c
	}
	return cs
}

// The offline replay in CI: the executor emulation (499-bar trigger window,
// SlotBlocked on a 1439-bar cache) must log exactly the backtest's trades
// after a burn-in, and the live resolver must score each one to the
// backtest's exit and R.
func TestEmulatedExecutorMatchesBacktest(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	for seed := int64(1); seed <= 3; seed++ {
		cs := walk4h(2200, seed)
		trades := autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 0)
		fires := emulateExecutor("BTC", cs, market.TF4h, 600, 0.25)
		since := cs[1200].CloseTime // burn-in: both must be flat somewhere in 600..1200
		execCS := cs[len(cs)-execCacheBars:]
		rows := compareSymbol("BTC", cs, trades, fires, since,
			func(f autotrade.PaperFire) autotrade.Outcome { return autotrade.Resolve(f, execCS, 6) }, nil)
		if len(rows) < 30 {
			t.Fatalf("seed %d: only %d rows compared", seed, len(rows))
		}
		for _, r := range rows {
			if r.kind != "match" {
				t.Errorf("seed %d: %s %s %s", seed, r.bar.Format("2006-01-02 15:04"), r.kind, r.detail)
			}
		}
		for _, f := range fires {
			if f.TrailM != p.TrailM || f.TP != 0 || f.RiskPct != 0.25 || f.Strategy != "htf-trend" || !f.Time.After(f.BarTime) {
				t.Fatalf("emulated fire shape: %+v", f)
			}
		}
	}
}

// The matcher's own verdicts: a missing signal is explained only by a
// blocked-log reason, an extra fire and a wrong stop are never explained.
func TestCompareSymbolVerdicts(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	cs := walk4h(900, 4)
	trades := autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 0)
	if len(trades) < 4 {
		t.Fatal("fixture too quiet")
	}
	fire := func(tr autostrat.TrendTrade) autotrade.PaperFire {
		return autotrade.PaperFire{Time: tr.EntryTime.Add(time.Minute), BarTime: tr.EntryTime, Symbol: "BTC", TF: "4h",
			Strategy: "htf-trend", Side: tr.Side, Market: true, Entry: tr.Entry, Stop: tr.InitStop, TrailM: p.TrailM}
	}
	var fires []autotrade.PaperFire
	for _, tr := range trades[1:] { // trades[0] never fired
		fires = append(fires, fire(tr))
	}
	fires[1].Stop *= 1.01    // a stop that disagrees
	extra := fire(trades[2]) // a fire on a bar the backtest did not signal
	extra.BarTime = trades[2].EntryTime.Add(-4 * time.Hour)
	fires = append(fires, extra)
	resolve := func(f autotrade.PaperFire) autotrade.Outcome { return autotrade.Resolve(f, cs, 6) }

	count := func(rows []cmpRow) map[string]int {
		m := map[string]int{}
		for _, r := range rows {
			m[r.kind]++
			if !r.ok {
				m["unexplained"]++
			}
		}
		return m
	}
	c := count(compareSymbol("BTC", cs, trades, fires, time.Time{}, resolve, nil))
	if c["missing"] != 1 || c["mismatch"] != 1 || c["extra"] != 1 || c["unexplained"] != 3 || c["match"] != len(trades)-2 {
		t.Errorf("verdicts = %v, want 1 missing, 1 mismatch, 1 extra, 3 unexplained, %d match", c, len(trades)-2)
	}
	c = count(compareSymbol("BTC", cs, trades, fires, time.Time{}, resolve, map[time.Time]string{trades[0].EntryTime: "blackout: macro/earnings"}))
	if c["missing"] != 1 || c["unexplained"] != 2 {
		t.Errorf("with a blocked-log reason: %v, want the missing row explained", c)
	}
}
