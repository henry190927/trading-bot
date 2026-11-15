package main

import (
	"testing"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/indicator"
)

// cap off is the plain backtest per rule; a cap of 1 never lets two group
// legs on one side overlap (exit-bar close is when a leg leaves the book).
func TestSimulateCapped(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	var rules []capRule
	for seed := int64(1); seed <= 4; seed++ {
		cs := walk4h(1500, seed)
		rules = append(rules, capRule{"r", cs, indicator.ATR(cs, p.ATRPeriod), 21})
	}
	off, refused := simulateCapped(rules, p, 10, 0)
	if refused != 0 {
		t.Fatalf("cap off refused %d", refused)
	}
	for i, r := range rules {
		want := autostrat.SimulateHTFTrend(r.cs, r.atr, r.first, true, true, p, 10)
		if len(want) != len(off[i]) {
			t.Fatalf("rule %d: %d trades vs %d", i, len(off[i]), len(want))
		}
		for k := range want {
			if want[k] != off[i][k] {
				t.Fatalf("rule %d trade %d: %+v vs %+v", i, k, off[i][k], want[k])
			}
		}
	}
	one, refused := simulateCapped(rules, p, 10, 1)
	if refused == 0 {
		t.Fatal("cap 1 over 4 rules refused nothing — the cap is inert")
	}
	var all []autostrat.TrendTrade
	for _, ts := range one {
		all = append(all, ts...)
	}
	for i, a := range all {
		for _, b := range all[i+1:] {
			if a.Side != b.Side {
				continue
			}
			// a enters while b is on the book, or vice versa
			if (!a.EntryTime.Before(b.EntryTime) && a.EntryTime.Before(b.ExitTime)) ||
				(!b.EntryTime.Before(a.EntryTime) && b.EntryTime.Before(a.ExitTime)) {
				t.Fatalf("cap 1: overlapping %s legs %s→%s and %s→%s", a.Side, a.EntryTime, a.ExitTime, b.EntryTime, b.ExitTime)
			}
		}
	}
}
