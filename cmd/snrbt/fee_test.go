package main

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// -fee-bps must actually reach the score. A flag that leaves the output
// byte-identical across values is a bug that reads as robustness, so this
// pins both halves: 0 is EvaluateFire untouched, and 10 bps costs exactly
// 10/10000 * fill / risk in R on a resolved trade — here 0.1R on a 1% stop.
func TestFeeChangesR(t *testing.T) {
	base := t0()
	var cs []market.Candle
	for i := 0; i < 4; i++ {
		open := base.Add(time.Duration(i) * time.Hour)
		c := market.Candle{OpenTime: open, CloseTime: open.Add(time.Hour), Open: 100, High: 100.5, Low: 99.5, Close: 100}
		if i == 2 {
			c.High = 103 // runs through the 2R target
		}
		cs = append(cs, c)
	}
	f := autotrade.PaperFire{
		Time: cs[0].CloseTime, Symbol: "TEST", Strategy: "htf-snr", Side: "long",
		Market: true, Entry: 100, Stop: 99, TP: 102,
	}

	gross := autotrade.EvaluateFire(f, cs, 6)
	if gross.Status != autotrade.OutTP {
		t.Fatalf("fixture outcome = %s, want tp — the fee comparison would be vacuous", gross.Status)
	}
	if got := resolveFire(f, cs, 0); got != gross {
		t.Errorf("fee 0 changed the outcome: %+v vs gross %+v", got, gross)
	}
	net := resolveFire(f, cs, 10)
	fill := gross.FillPrice
	if fill == 0 {
		fill = f.Entry
	}
	want := gross.NetR - 10.0/10000.0*fill/(f.Entry-f.Stop)
	if math.Abs(net.NetR-want) > 1e-12 {
		t.Errorf("fee 10bp NetR = %v, want %v (gross %v)", net.NetR, want, gross.NetR)
	}
	if net.NetR >= gross.NetR {
		t.Errorf("fee did not reduce R: net %v >= gross %v", net.NetR, gross.NetR)
	}
}

// -htf-bars limits eligible swings to the daemon's Klines(200) view. A swing
// that has scrolled out of the last N HTF bars must stop producing fires,
// while a window wide enough to hold it must match the unbounded run.
func TestHTFBarsDropsSwingsOutsideTheLiveWindow(t *testing.T) {
	const strength, swingAt = 3, 5
	base := t0()
	var hcs []market.Candle
	for i := 0; i < 20; i++ {
		p := 100.0 + float64(i)*0.1
		if i == swingAt {
			p = 120.0
		}
		open := base.Add(time.Duration(i) * 4 * time.Hour)
		hcs = append(hcs, market.Candle{OpenTime: open, CloseTime: open.Add(4 * time.Hour), Open: p, High: p, Low: p - 1, Close: p - 0.5})
	}
	startAt := hcs[swingAt+strength].CloseTime.Add(time.Hour)
	var cs []market.Candle
	var atr []float64
	for i := 0; i < 40; i++ {
		open := startAt.Add(time.Duration(i) * time.Hour)
		cs = append(cs, market.Candle{OpenTime: open, CloseTime: open.Add(time.Hour), Open: 119, High: 120.5, Low: 118, Close: 119.2})
		atr = append(atr, 1.0)
	}

	all := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "auto", 0)
	if len(all) == 0 {
		t.Fatal("unbounded run fired nothing — fixture proves nothing")
	}
	wide := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "auto", 200)
	if len(wide) != len(all) {
		t.Errorf("htf-bars 200 (holds the swing) fired %d, unbounded %d — want equal", len(wide), len(all))
	}
	// Base bars i>=30 close from 68h on; the swing (opened 20h) is >3 HTF bars back.
	if narrow := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "auto", 3); len(narrow) != 0 {
		t.Errorf("htf-bars 3 fired %d time(s) on a swing outside the window", len(narrow))
	}
}
