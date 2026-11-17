package main

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func vbar(t time.Time, o, h, l, c, v float64) market.Candle {
	return market.Candle{OpenTime: t, CloseTime: t.Add(time.Minute), Open: o, High: h, Low: l, Close: c, Volume: v}
}

// priorDay: bins span 93.75..106.25 (width 0.125, exact in binary).
//   - bar 0: 93.75–94 and bar 1: 106–106.25 (vol 1 each) only fix the range;
//   - 438 "A" bars 97–103 → bins 26..74 (49 bins), 438/49 = 8.94 each;
//   - 1000 "B" bars 98–102 → bins 34..66 (33 bins), 1000/33 = 30.30 each.
//
// Bins 34..66 tie at 39.24 → POC = bin 34 (first max), mid 98.0625. Target
// 70% of 1440 = 1008. Expansion from 34: left neighbour 8.94 < right 39.24,
// so it grows right; 26 bins × 39.24 = 1020 ≥ 1008 and
// 25 × 39.24 = 981 < 1008 → bins 34..59. VAL = 93.75 + 34×0.125 = 98.0,
// VAH = 93.75 + 60×0.125 = 101.25.
func priorDay(d time.Time) []market.Candle {
	var cs []market.Candle
	cs = append(cs, vbar(d, 93.9, 94, 93.75, 93.9, 1))
	cs = append(cs, vbar(d.Add(time.Minute), 106.1, 106.25, 106, 106.1, 1))
	for i := 2; i < 1440; i++ {
		t := d.Add(time.Duration(i) * time.Minute)
		if i < 440 {
			cs = append(cs, vbar(t, 100, 103, 97, 100, 1))
		} else {
			cs = append(cs, vbar(t, 100, 102, 98, 100, 1))
		}
	}
	return cs
}

func TestValueAreaHandTraced(t *testing.T) {
	d := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	va, ok := ValueArea(priorDay(d), 100)
	if !ok || math.Abs(va.VAL-98.0) > 1e-9 || math.Abs(va.VAH-101.25) > 1e-9 || math.Abs(va.POC-98.0625) > 1e-9 {
		t.Fatalf("VA %+v ok=%v; want VAL 98 VAH 101.25 POC 98.0625", va, ok)
	}
}

// Day D (hand-traced, VA 98.0–101.25 from priorDay):
//
//	00:00–00:29 (#1)  flat 101.6 (H 101.7 L 101.5)   open 101.6 > VAH → SHORT setup; close outside
//	00:30–00:59 (#2)  00:30 bar 101.6→101.0 (H 101.65); 00:45 pokes 101.4; close 101.0 inside (re-entry bar)
//	01:00–01:29 (#3)  01:10 pokes H 101.8; 01:29 closes 100.8 (L 100.75) inside → accepted
//	01:30             falls to 97.95 (L 97.9) → TP
//
// ATR14(30m), Wilder, from the aggregates: day-0 bucket 0 TR 12.5, buckets
// 1..14 TR 6 (A bars), 15..47 TR 4 → seed ATR[14] = 6, ATR[47] = 4 +
// 2×(13/14)^33 ≈ 4.173. #1 TR = |101.7−100| = 1.7 → ATR[48] ≈ 3.996; #2 TR
// = 0.7 → ATR[49] ≈ 3.761; #3 TR = 101.8−100.75 = 1.05 → ATR[50] ≈ 3.567.
//
// 30m×2: entry 100.8 at 01:30; extreme since 00:30 = 101.8 → stop = 101.8 +
// 0.25×3.567 ≈ 102.692 (1.88%), TP = VAL 98.0, RR = 2.8/1.892 ≈ 1.48.
// 1h×1: the 00:00–00:59 1h bar closes 101.0 inside → entry 101.0 at 01:00;
// extreme since 00:00 = 101.7 → stop = 101.7 + 0.25×ATR[49] ≈ 102.640, TP
// 98.0, RR = 3.0/1.640 ≈ 1.83.
func dayD(d time.Time) []market.Candle {
	var cs []market.Candle
	at := func(m int) time.Time { return d.Add(time.Duration(m) * time.Minute) }
	for m := 0; m < 30; m++ {
		cs = append(cs, vbar(at(m), 101.6, 101.7, 101.5, 101.6, 1))
	}
	for m := 30; m < 60; m++ {
		switch m {
		case 30:
			cs = append(cs, vbar(at(m), 101.6, 101.65, 101.0, 101.0, 1))
		case 45:
			cs = append(cs, vbar(at(m), 101.0, 101.4, 100.95, 101.0, 1))
		default:
			cs = append(cs, vbar(at(m), 101.0, 101.05, 100.95, 101.0, 1))
		}
	}
	for m := 60; m < 90; m++ {
		switch m {
		case 70:
			cs = append(cs, vbar(at(m), 101.0, 101.8, 100.95, 101.0, 1))
		case 89:
			cs = append(cs, vbar(at(m), 101.0, 101.05, 100.75, 100.8, 1))
		default:
			cs = append(cs, vbar(at(m), 101.0, 101.05, 100.95, 101.0, 1))
		}
	}
	cs = append(cs, vbar(at(90), 100.8, 100.85, 97.9, 97.95, 1))
	for m := 91; m < 180; m++ {
		cs = append(cs, vbar(at(m), 97.95, 98.05, 97.9, 97.95, 1))
	}
	return cs
}

func TestHandTracedShort(t *testing.T) {
	d0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	d := d0.AddDate(0, 0, 1)
	cs := append(priorDay(d0), dayD(d)...)
	atr := indicator.ATR(asCandles(Aggregate(cs, 30*time.Minute)), 14)
	// the hand-traced Wilder values (approximate) must agree with the code's
	if math.Abs(atr[49]-3.761) > 0.01 || math.Abs(atr[50]-3.567) > 0.01 {
		t.Fatalf("ATR[49]=%.4f ATR[50]=%.4f; hand-traced 3.761 / 3.567", atr[49], atr[50])
	}

	p := DefaultParams()
	fires := GenFires(cs, "BTC", p)
	if len(fires) != 1 {
		t.Fatalf("30mx2: want 1 fire, got %d: %+v", len(fires), fires)
	}
	f := fires[0]
	wantStop := 101.8 + 0.25*atr[50]
	if f.Side != "short" || f.Entry != 100.8 || f.TP != 98.0 || math.Abs(f.Stop-wantStop) > 1e-9 ||
		math.Abs(f.Stop-102.692) > 0.005 || !f.Time.Equal(d.Add(90*time.Minute)) || !f.Market {
		t.Fatalf("30mx2 fire %+v; want short 100.8 stop %.4f tp 98 at 01:30", f, wantStop)
	}
	oc := autotrade.EvaluateFire(f, cs, 0)
	if oc.Status != autotrade.OutTP || math.Abs(oc.NetR-2.8/(wantStop-100.8)) > 1e-9 {
		t.Fatalf("outcome %+v; want TP at %.4fR", oc, 2.8/(wantStop-100.8))
	}

	p.HTF, p.Need = time.Hour, 1
	fires = GenFires(cs, "BTC", p)
	if len(fires) != 1 {
		t.Fatalf("1hx1: want 1 fire, got %d: %+v", len(fires), fires)
	}
	f = fires[0]
	wantStop = 101.7 + 0.25*atr[49]
	if f.Side != "short" || f.Entry != 101.0 || f.TP != 98.0 || math.Abs(f.Stop-wantStop) > 1e-9 ||
		math.Abs(f.Stop-102.640) > 0.005 || !f.Time.Equal(d.Add(60*time.Minute)) {
		t.Fatalf("1hx1 fire %+v; want short 101.0 stop %.4f tp 98 at 01:00", f, wantStop)
	}
}

// An open inside the VA is no setup; a close beyond the opposite edge before
// acceptance voids the day.
func TestNoSetupAndVoid(t *testing.T) {
	d0 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	d := d0.AddDate(0, 0, 1)
	var inside []market.Candle
	for m := 0; m < 180; m++ {
		inside = append(inside, vbar(d.Add(time.Duration(m)*time.Minute), 100, 100.1, 99.9, 100, 1))
	}
	if f := GenFires(append(priorDay(d0), inside...), "X", DefaultParams()); len(f) != 0 {
		t.Fatalf("open inside VA fired: %+v", f)
	}
	var void []market.Candle
	for m := 0; m < 30; m++ { // opens 101.6, then the first 30m closes below VAL
		c := 101.6
		if m == 29 {
			c = 97.5
		}
		void = append(void, vbar(d.Add(time.Duration(m)*time.Minute), 101.6, 101.7, math.Min(c, 101.5), c, 1))
	}
	for m := 30; m < 180; m++ {
		void = append(void, vbar(d.Add(time.Duration(m)*time.Minute), 100, 100.1, 99.9, 100, 1))
	}
	if f := GenFires(append(priorDay(d0), void...), "X", DefaultParams()); len(f) != 0 {
		t.Fatalf("void day fired: %+v", f)
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
		cs = append(cs, vbar(start.Add(time.Duration(i)*time.Minute), o, h, l, c, 1+r.ExpFloat64()*10))
		p = c
	}
	return cs
}

// Every fire at or before the cut must be identical when every bar after the
// cut is replaced: VA, aggregation, ATR and the stop scan may not read ahead.
func TestNoLookahead(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cs := randomWalk(7, start, 30*24*60)
	cut := 22*24*60 + 437
	poisoned := append([]market.Candle(nil), cs[:cut]...)
	poisoned = append(poisoned, randomWalk(99, cs[cut].OpenTime, len(cs)-cut)...)
	for i := cut; i < len(poisoned); i++ {
		poisoned[i].Open *= 1.3
		poisoned[i].High *= 1.3
		poisoned[i].Low *= 1.3
		poisoned[i].Close *= 1.3
		poisoned[i].Volume *= 7
	}
	limit := cs[cut-1].CloseTime
	for _, v := range []struct {
		htf  time.Duration
		need int
	}{{30 * time.Minute, 2}, {time.Hour, 1}} {
		p := DefaultParams()
		p.HTF, p.Need = v.htf, v.need
		keep := func(fs []autotrade.PaperFire) []autotrade.PaperFire {
			var out []autotrade.PaperFire
			for _, f := range fs {
				if !f.Time.After(limit) {
					out = append(out, f)
				}
			}
			return out
		}
		a, b := keep(GenFires(cs, "X", p)), keep(GenFires(poisoned, "X", p))
		if len(a) == 0 {
			t.Fatalf("%v×%d: no fires before the cut — the test proves nothing", v.htf, v.need)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%v×%d: fires before the cut changed (%d vs %d)", v.htf, v.need, len(a), len(b))
		}
		// and the series after the cut does produce different fires (the poison bites)
		if reflect.DeepEqual(GenFires(cs, "X", p), GenFires(poisoned, "X", p)) {
			t.Fatalf("%v×%d: poison changed nothing after the cut", v.htf, v.need)
		}
	}
}
