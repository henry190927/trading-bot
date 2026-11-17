package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// The executor runs autostrat.SimulateHTFTrendTouch's rules; the A/B that
// justified adopting touch-1d (docs/htf_trend_entry_variants_design.md §7)
// ran simulateTouch. They must be the same trades, fee included, so the
// adopted result is the thing that is running.
func TestSimulateTouchMatchesAutostrat(t *testing.T) {
	var series [][]market.Candle
	if b, err := os.ReadFile("../../autostrat/testdata/btc_4h_bingx_slice.json"); err == nil {
		var cs []market.Candle
		if json.Unmarshal(b, &cs) == nil {
			series = append(series, cs)
		}
	}
	for seed := int64(1); seed <= 6; seed++ {
		r := rand.New(rand.NewSource(seed))
		p, t0 := 100.0, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		var cs []market.Candle
		for i := range 800 {
			o := p
			c := o * (1 + r.NormFloat64()*0.02)
			h := max(o, c) * (1 + r.Float64()*0.01)
			l := min(o, c) * (1 - r.Float64()*0.01)
			if i%37 == 0 { // gaps
				o *= 1 + r.NormFloat64()*0.03
				h, l = max(h, o), min(l, o)
			}
			ot := t0.Add(time.Duration(i) * 24 * time.Hour)
			cs = append(cs, market.Candle{OpenTime: ot, CloseTime: ot.Add(24 * time.Hour), Open: o, High: h, Low: l, Close: c})
			p = c
		}
		series = append(series, cs)
	}
	if len(series) < 2 {
		t.Fatal("no series")
	}
	p := autostrat.DefaultHTFTrendParams()
	for n, cs := range series {
		atr := indicator.ATR(cs, p.ATRPeriod)
		old, _ := simulateTouch(cs, atr, 21, true, true, p, 10)
		cur := autostrat.SimulateHTFTrendTouch(cs, atr, 21, true, true, p, 10)
		if len(old) == 0 || len(old) != len(cur) {
			t.Fatalf("series %d: %d vs %d trades", n, len(old), len(cur))
		}
		for k := range old {
			a, b := old[k], cur[k]
			if a.EntryIdx != b.EntryIdx || a.ExitIdx != b.ExitIdx || a.Side != b.Side || a.Entry != b.Entry ||
				a.InitStop != b.InitStop || a.Exit != b.Exit || a.NetR != b.NetR || a.Open != b.Open {
				t.Fatalf("series %d trade %d:\n old %+v\n new %+v", n, k, a, b)
			}
		}
	}
}
