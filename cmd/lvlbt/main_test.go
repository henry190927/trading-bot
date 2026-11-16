package main

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func bar(t time.Time, o, h, l, c float64) market.Candle {
	return market.Candle{OpenTime: t, CloseTime: t.Add(time.Minute), Open: o, High: h, Low: l, Close: c}
}

// flatFrom appends flat 1m bars at price p (range ±0.1) for n minutes.
func flatFrom(cs []market.Candle, t time.Time, n int, p float64) []market.Candle {
	for i := range n {
		cs = append(cs, bar(t.Add(time.Duration(i)*time.Minute), p, p+0.1, p-0.1, p))
	}
	return cs
}

// Hand-traced: Sunday flat at 100, so Monday's DO (= WO = AO/LO/NYO before
// they move) is 100. The 00:15 UTC 15m bar wicks to 99.5 and closes 100.3:
// close > DO and low < DO → long event (站穩). Signal at 00:30. The 00:30
// bar is flat (no body); the 00:31 bar 100.3 → 100.8 (range 100.25–100.85,
// body 0.5 ≫ ATR ≈ 0.23, close at 92% of range) is the action bar.
// Stop = lowest low since 00:15 (99.5) − 0.1 × ATR(00:30), TP = 2R.
func TestHandTracedDOReclaim(t *testing.T) {
	sun := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mon := sun.AddDate(0, 0, 1)
	cs := flatFrom(nil, sun, 24*60, 100)
	cs = flatFrom(cs, mon, 15, 100) // 00:00–00:14
	for i := range 15 {             // 00:15–00:29
		tt := mon.Add(time.Duration(15+i) * time.Minute)
		switch i {
		case 5:
			cs = append(cs, bar(tt, 100, 100.1, 99.5, 100))
		case 14:
			cs = append(cs, bar(tt, 100, 100.35, 99.95, 100.3))
		default:
			cs = append(cs, bar(tt, 100, 100.1, 99.9, 100))
		}
	}
	cs = append(cs, bar(mon.Add(30*time.Minute), 100.3, 100.4, 100.2, 100.3))
	cs = append(cs, bar(mon.Add(31*time.Minute), 100.3, 100.85, 100.25, 100.8))
	cs = flatFrom(cs, mon.Add(32*time.Minute), 6*60, 100.8)

	p := DefaultParams()
	p.OpensOnly = true
	fires := GenFires(cs, "BTC", p)
	if len(fires) != 1 {
		t.Fatalf("want 1 fire, got %d: %+v", len(fires), fires)
	}
	f := fires[0]
	atr := indicator.ATR(cs, 20)
	iAct := 24*60 + 31
	wantStop := 99.5 - 0.1*atr[iAct-1]
	if f.Side != "long" || f.Entry != 100.8 || math.Abs(f.Stop-wantStop) > 1e-9 ||
		math.Abs(f.TP-(100.8+2*(100.8-wantStop))) > 1e-9 || !f.Time.Equal(mon.Add(32*time.Minute)) {
		t.Fatalf("fire %+v; want long entry 100.8 stop %.4f at %s", f, wantStop, mon.Add(32*time.Minute))
	}
	if f.Why[:2] != "DO" {
		t.Fatalf("level %q, want DO (WO coincides and is merged)", f.Why)
	}
}

func randomWalk(seed int64, start time.Time, n int) []market.Candle {
	r := rand.New(rand.NewSource(seed))
	p := 100.0
	var cs []market.Candle
	for i := range n {
		o := p
		c := o * (1 + r.NormFloat64()*0.0012)
		h := math.Max(o, c) * (1 + math.Abs(r.NormFloat64())*0.0006)
		l := math.Min(o, c) * (1 - math.Abs(r.NormFloat64())*0.0006)
		cs = append(cs, bar(start.Add(time.Duration(i)*time.Minute), o, h, l, c))
		p = c
	}
	return cs
}

// Every fire before bar X must be identical when every bar from X on is
// replaced: the rule may not read the future (levels, HTF aggregation,
// action scan, stop range).
func TestNoLookahead(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) // a Monday
	cs := randomWalk(7, start, 12*24*60)
	cut := 8*24*60 + 437
	poisoned := append([]market.Candle(nil), cs[:cut]...)
	poisoned = append(poisoned, randomWalk(99, cs[cut].OpenTime, len(cs)-cut)...)
	for i := cut; i < len(poisoned); i++ { // shift the poisoned tail far away
		poisoned[i].Open *= 1.3
		poisoned[i].High *= 1.3
		poisoned[i].Low *= 1.3
		poisoned[i].Close *= 1.3
	}
	for _, opens := range []bool{true, false} {
		for _, h := range []time.Duration{15 * time.Minute, time.Hour} {
			for _, k := range []int{1, 2} {
				p := DefaultParams()
				p.HTF, p.K, p.OpensOnly = h, k, opens
				a, b := GenFires(cs, "X", p), GenFires(poisoned, "X", p)
				limit := cs[cut-1].CloseTime
				var pa, pb []string
				for _, f := range a {
					if !f.Time.After(limit) {
						pa = append(pa, f.Time.String()+f.Side+f.Why)
					}
				}
				for _, f := range b {
					if !f.Time.After(limit) {
						pb = append(pb, f.Time.String()+f.Side+f.Why)
					}
				}
				if len(pa) == 0 {
					t.Fatalf("%v K%d opens=%v: no fires before the cut — the test proves nothing", h, k, opens)
				}
				if !reflect.DeepEqual(pa, pb) {
					t.Fatalf("%v K%d opens=%v: fires before the cut changed (%d vs %d)", h, k, opens, len(pa), len(pb))
				}
			}
		}
	}
}
