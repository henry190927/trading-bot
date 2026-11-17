package autostrat

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// EXECUTOR PARITY for htf-trend-1d-touch: walking the bars the way the paper
// executor does (one position at a time, evaluate each closed bar while flat,
// log the trigger as a fire, let the resolver score it) must reproduce
// SimulateHTFTrendTouch trade for trade — entry, stop, exit bar, exit price
// and gross R — including the same-bar stops the resolver checks via Touch.
func TestHTFTrendTouchExecutorParity(t *testing.T) {
	sameBar := 0
	for _, pc := range parityCases(t) {
		sim := SimulateHTFTrendTouch(pc.cs, pc.atr, pc.warm, true, true, pc.p, 0)
		if len(sim) == 0 {
			t.Fatalf("%s: no touch trades — the case proves nothing", pc.name)
		}
		k, i := 0, max(pc.warm, pc.p.EntryN, 1)
		for ; i < len(pc.cs); i++ {
			tr := HTFTrendTouchTrigger(pc.cs, pc.atr[i-1], i, true, true, pc.p)
			if !tr.Fire {
				continue
			}
			if k >= len(sim) {
				t.Fatalf("%s: executor fired at bar %d past the %d simulated trades", pc.name, i, len(sim))
			}
			want := sim[k]
			f := autotrade.PaperFire{Time: pc.cs[i].CloseTime, TF: "4h", Side: tr.Side, Market: true,
				Entry: tr.Entry, Stop: tr.Stop, TrailM: tr.TrailM, BarTime: tr.BarTime, Touch: tr.Touch}
			oc := autotrade.EvaluateTrailFire(f, pc.cs)
			if want.EntryIdx != i || tr.Side != want.Side || tr.Entry != want.Entry || tr.Stop != want.InitStop {
				t.Fatalf("%s trade %d: executor %s@%d e %.6f s %.6f, sim %s@%d e %.6f s %.6f",
					pc.name, k, tr.Side, i, tr.Entry, tr.Stop, want.Side, want.EntryIdx, want.Entry, want.InitStop)
			}
			if want.Open {
				if oc.Status != autotrade.OutOpen {
					t.Fatalf("%s trade %d: sim open, resolver %s", pc.name, k, oc.Status)
				}
				break
			}
			if oc.Status != autotrade.OutStop || !oc.ExitAt.Equal(want.ExitTime) || oc.ExitPrice != want.Exit ||
				math.Abs(oc.NetR-want.GrossR) > 1e-9 {
				t.Fatalf("%s trade %d: resolver %s exit %.6f at %s R %.6f; sim exit %.6f at %s R %.6f",
					pc.name, k, oc.Status, oc.ExitPrice, oc.ExitAt, oc.NetR, want.Exit, want.ExitTime, want.GrossR)
			}
			if want.ExitIdx == i {
				sameBar++
			}
			k++
			i = want.ExitIdx // flat again from the bar after the exit
		}
		if k != len(sim) && !sim[len(sim)-1].Open {
			t.Fatalf("%s: executor matched %d of %d trades", pc.name, k, len(sim))
		}
	}
	if sameBar == 0 {
		t.Fatal("no same-bar stop in any case — the Touch branch of the resolver is untested")
	}
}

// The 500-bar live window gives the same fire as the full history.
func TestHTFTrendTouchLiveWindowParity(t *testing.T) {
	cs := loadBingXSlice(t)
	p := DefaultHTFTrendParams()
	atr := indicator.ATR(cs, p.ATRPeriod)
	fires := 0
	for i := htfTrendLiveBars - 1; i < len(cs); i++ {
		full := HTFTrendTouchTrigger(cs, atr[i-1], i, true, true, p)
		win := HTFTrendTouchOnWindow(cs[i-htfTrendLiveBars+1:i+1], autotrade.Rule{Side: "auto"})
		if full.Fire != win.Fire || full.Side != win.Side || full.Entry != win.Entry ||
			math.Abs(full.Stop-win.Stop) > 1e-6*full.Entry {
			t.Fatalf("bar %d: full %+v window %+v", i, full, win)
		}
		if full.Fire {
			fires++
		}
	}
	if fires == 0 {
		t.Fatal("no touch fire in the window range — the test proves nothing")
	}
}

// Hand-traced: channel high 10 over bars 0-2, ATR 1, StopATR 3. Bar 3 opens
// 9.5 and trades to 10.4 → long filled at 10, stop 7. Bar 4 gaps open at
// 10.6 above a channel high now 10.4 → fill at the open 10.6 (gap), stop 7.6.
// Bar 5 also reaches its stop inside the fill bar → same-bar stop, -1R.
func TestHTFTrendTouchHandTraced(t *testing.T) {
	p := HTFTrendParams{EntryN: 3, TrailM: 2, StopATR: 3, ATRPeriod: 1}
	c := func(o, h, l, cl float64) market.Candle { return market.Candle{Open: o, High: h, Low: l, Close: cl} }
	cs := []market.Candle{c(9, 10, 8, 9), c(9, 9.5, 8, 9), c(9, 9.8, 8.5, 9.5), c(9.5, 10.4, 9.4, 10.2), c(10.6, 11, 10.5, 10.8)}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for k := range cs {
		cs[k].OpenTime = t0.Add(time.Duration(k) * 4 * time.Hour)
		cs[k].CloseTime = cs[k].OpenTime.Add(4 * time.Hour)
	}
	tr := HTFTrendTouchTrigger(cs, 1, 3, true, true, p)
	if !tr.Fire || tr.Side != "long" || tr.Entry != 10 || tr.Stop != 7 || !tr.Touch {
		t.Fatalf("bar 3: %+v, want long 10 stop 7 touch", tr)
	}
	tr = HTFTrendTouchTrigger(cs, 1, 4, true, true, p)
	if !tr.Fire || tr.Entry != 10.6 || math.Abs(tr.Stop-7.6) > 1e-12 {
		t.Fatalf("bar 4 gap: %+v, want fill at the open 10.6 stop 7.6", tr)
	}
	// Same-bar stop: fill at 10, stop 9.9 (ATR 1/30), bar low 9.4 <= 9.9.
	f := autotrade.PaperFire{TF: "4h", Side: "long", Entry: 10, Stop: 9.9, TrailM: 2, Touch: true,
		BarTime: cs[3].CloseTime, Time: cs[3].CloseTime}
	oc := autotrade.EvaluateTrailFire(f, cs)
	if oc.Status != autotrade.OutStop || math.Abs(oc.NetR+1) > 1e-9 || oc.ExitPrice != 9.9 {
		t.Fatalf("same-bar stop: %+v, want stop at 9.9, -1R", oc)
	}
	f.Touch = false // a close-entry fire never stops on its own signal bar
	if oc := autotrade.EvaluateTrailFire(f, cs); oc.Status == autotrade.OutStop && oc.ExitAt.Equal(cs[3].CloseTime) {
		t.Fatalf("non-touch fire stopped on its signal bar: %+v", oc)
	}
}
