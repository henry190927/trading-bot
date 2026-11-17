package main

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

var t0 = time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC) // a Sunday

// barsFromCloses: daily bars, Open = previous close (first: its own close),
// High = max(O,C)+0.5, Low = min(O,C)-0.5 — so TR = |C-O| + 1.
func barsFromCloses(closes []float64) []market.Candle {
	cs := make([]market.Candle, len(closes))
	prev := closes[0]
	for i, c := range closes {
		ot := t0.AddDate(0, 0, i)
		o := prev
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(24*time.Hour - time.Millisecond),
			Open: o, Close: c, High: math.Max(o, c) + 0.5, Low: math.Min(o, c) - 0.5}
		prev = c
	}
	return cs
}

func daysRebal(days ...int) func(time.Time) bool {
	return func(t time.Time) bool {
		d := int(t.Sub(t0).Hours() / 24)
		for _, x := range days {
			if d == x {
				return true
			}
		}
		return false
	}
}

func handSeries(edit func(map[string][]market.Candle)) []series {
	closes := map[string][]float64{
		"A": {100, 101, 102, 103, 104, 105, 106, 107, 108},
		"B": {100, 100, 100, 100, 100, 100, 100, 100, 100},
		"C": {100, 100, 100, 100, 100, 101, 103, 104, 105},
		"D": {100, 100, 100, 100, 100, 99, 97, 96, 95},
		"E": {100, 99, 98, 97, 96, 98, 97, 97, 97},
	}
	bars := map[string][]market.Candle{}
	for k, v := range closes {
		bars[k] = barsFromCloses(v)
	}
	if edit != nil {
		edit(bars)
	}
	var ss []series
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		ss = append(ss, newSeries(k, bars[k], 2))
	}
	return ss
}

var handP = Params{L: 2, K: 1, ATRPeriod: 2, StopATR: 1}

type wantLeg struct {
	side        string
	entryIdx    int
	entry, stop float64
	exitIdx     int
	exit        float64
	reason      string
	netR        float64
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-5 }

func checkLegs(t *testing.T, got [][]Leg, want map[int][]wantLeg) {
	t.Helper()
	for s := range got {
		w := want[s]
		if len(got[s]) != len(w) {
			t.Fatalf("sym %d: %d legs, want %d: %+v", s, len(got[s]), len(w), got[s])
		}
		for i, g := range got[s] {
			e := w[i]
			if g.Side != e.side || g.EntryIdx != e.entryIdx || !near(g.Entry, e.entry) || !near(g.InitStop, e.stop) ||
				g.ExitIdx != e.exitIdx || !near(g.Exit, e.exit) || g.Reason != e.reason || !near(g.NetR, e.netR) {
				t.Errorf("sym %d leg %d: got %s idx%d %.4f stop %.4f → idx%d %.4f %s net %.6f; want %+v",
					s, i, g.Side, g.EntryIdx, g.Entry, g.InitStop, g.ExitIdx, g.Exit, g.Reason, g.NetR, e)
			}
		}
	}
}

// Hand trace (docs/xsmom_design.md §5), rebalances on days 4, 6, 8, fee 10 bp:
//
//	day 4 rets C4/C2: A +1.96% B 0 C 0 D 0 E −2.04% → long A @104, short E @96.
//	  ATR2: A tr=2 every bar → 2, stop 102, fee .001×104/2 = .052.
//	        E tr=2 → 2, stop 98, fee .001×96/2 = .048.
//	day 5 E: O96 H98.5 → stopped at 98: gross −1, net −1.048.
//	day 6 rets C6/C4: C +3% A +1.92% E +1.04% B 0 D −3% → long C, short D;
//	  A exits at 106: gross +1, net .948.
//	  C: tr 1,1,1,1,2,3 → ATR[4]=1, [5]=1.5, [6]=2.25; stop 100.75, fee .001×103/2.25.
//	  D: same TRs → 2.25; short 97 stop 99.25, fee .001×97/2.25.
//	day 8 rets: C +1.94% A +1.89% B 0 E 0 D −2.06% → both held; last bar →
//	  marked: C (105−103)/2.25 − .045778 = .843111; D (97−95)/2.25 − .043111 = .845778.
func TestHandTrace(t *testing.T) {
	got, books := simulate(handSeries(nil), handP, daysRebal(4, 6, 8), 10)
	if books != 3 {
		t.Fatalf("books %d, want 3", books)
	}
	checkLegs(t, got, map[int][]wantLeg{
		0: {{"long", 4, 104, 102, 6, 106, "rebal", 0.948}},
		2: {{"long", 6, 103, 100.75, 8, 105, "open", 0.843111}},
		3: {{"short", 6, 97, 99.25, 8, 95, "open", 0.845778}},
		4: {{"short", 4, 96, 98, 5, 98, "stop", -1.048}},
	})
	if !got[2][0].Open || got[0][0].Open {
		t.Error("open flags")
	}
}

// Gap through the stop exits at the open: E day 5 opens at 99 (stop 98) →
// exit 99, gross −(99−96)/2 = −1.5, net −1.548. Nothing else changes.
func TestStopGapFill(t *testing.T) {
	got, _ := simulate(handSeries(func(b map[string][]market.Candle) {
		b["E"][5].Open, b["E"][5].High, b["E"][5].Low = 99, 99.5, 97.5
	}), handP, daysRebal(4, 6, 8), 10)
	if g := got[4][0]; g.Exit != 99 || g.ExitIdx != 5 || !near(g.NetR, -1.548) {
		t.Fatalf("gap: %+v", g)
	}
}

// Stopped mid-week and still in the bottom bucket at the next rebalance → a
// NEW leg. E day 6 closes 92 (O 98): ret 92/96 = −4.17% < D −3% → short E @92.
// E's day-6 TR = |92−98|+1 = 7, ATR[5] = (2 + 3)/2 = 2.5 (day-5 TR |98−96|+1 = 3),
// ATR[6] = (2.5+7)/2 = 4.75 → stop 96.75. D is not traded.
func TestReentryAfterStop(t *testing.T) {
	ss := handSeries(func(b map[string][]market.Candle) {
		b["E"][6].Close, b["E"][6].Low = 92, 91.5
		b["E"][7].Open, b["E"][7].High, b["E"][7].Low = 92, 97.5, 91.5 // close 97 → rallies through 96.75
	})
	got, _ := simulate(ss, handP, daysRebal(4, 6), 0)
	if len(got[3]) != 0 {
		t.Fatalf("D traded: %+v", got[3])
	}
	if len(got[4]) != 2 {
		t.Fatalf("E legs %+v", got[4])
	}
	e := got[4][1]
	if e.Side != "short" || e.EntryIdx != 6 || e.Entry != 92 || !near(e.InitStop, 96.75) || e.ExitIdx != 7 || !near(e.Exit, 96.75) || e.Reason != "stop" {
		t.Fatalf("re-entry leg %+v", e)
	}
}

// Fewer than 2k+1 eligible → no book and open legs close at the rebalance.
func TestMinEligible(t *testing.T) {
	ss := handSeries(nil)[:3] // A B C only
	got, books := simulate(ss, Params{L: 2, K: 2, ATRPeriod: 2, StopATR: 1}, daysRebal(4, 6, 8), 0)
	if books != 0 || len(got[0])+len(got[1])+len(got[2]) != 0 {
		t.Fatalf("books %d legs %+v", books, got)
	}
}

func randomSeries(seed int64, nSym, n int) []series {
	r := rand.New(rand.NewSource(seed))
	var ss []series
	for s := 0; s < nSym; s++ {
		cl := make([]float64, n)
		px := 100.0
		for i := range cl {
			px *= 1 + r.NormFloat64()*0.03 + 0.002*float64(s-nSym/2)*math.Sin(float64(i)/20)
			cl[i] = px
		}
		cs := barsFromCloses(cl)
		for i := range cs {
			cs[i].High += r.Float64() * px * 0.01
			cs[i].Low -= r.Float64() * px * 0.01
		}
		ss = append(ss, newSeries(string(rune('A'+s)), cs, 20))
	}
	return ss
}

// Poison every bar after a cut: every leg entered at or before the cut keeps
// side / bar / fill / stop, and every leg exited at or before the cut is
// identical.
func TestNoLookahead(t *testing.T) {
	p := Params{L: 30, K: 2, ATRPeriod: 20, StopATR: 3}
	ss := randomSeries(7, 8, 300)
	base, _ := simulate(ss, p, sundayRebal, 10)
	for _, cut := range []int{100, 157, 220} {
		pois := make([]series, len(ss))
		for s := range ss {
			cs := append([]market.Candle(nil), ss[s].cs...)
			for i := cut + 1; i < len(cs); i++ {
				cs[i].Open, cs[i].High, cs[i].Low, cs[i].Close = 1e6*float64(s+1), 2e6, 1e-3, 5e5*float64(len(cs)-i)
			}
			pois[s] = newSeries(ss[s].short, cs, 20)
		}
		got, _ := simulate(pois, p, sundayRebal, 10)
		nb := 0
		for s := range base {
			var b, g []Leg
			for _, l := range base[s] {
				if l.EntryIdx <= cut {
					b = append(b, l)
				}
			}
			for _, l := range got[s] {
				if l.EntryIdx <= cut {
					g = append(g, l)
				}
			}
			if len(b) != len(g) {
				t.Fatalf("cut %d sym %d: %d vs %d legs entered", cut, s, len(b), len(g))
			}
			for i := range b {
				if b[i].Side != g[i].Side || b[i].EntryIdx != g[i].EntryIdx || b[i].Entry != g[i].Entry || b[i].InitStop != g[i].InitStop {
					t.Fatalf("cut %d sym %d leg %d entry differs", cut, s, i)
				}
				if b[i].ExitIdx <= cut && b[i].TrendTrade != g[i].TrendTrade {
					t.Fatalf("cut %d sym %d leg %d closed before cut differs", cut, s, i)
				}
				nb++
			}
		}
		if nb < 10 {
			t.Fatalf("cut %d: only %d legs compared", cut, nb)
		}
	}
}

// Each pre-registered parameter must change the output.
func TestParamsNotInert(t *testing.T) {
	ss := randomSeries(11, 8, 400)
	sig := func(p Params, fee float64) (int, float64) {
		got, _ := simulate(ss, p, sundayRebal, fee)
		n, sum := 0, 0.0
		for _, ls := range got {
			for _, l := range ls {
				n++
				sum += l.NetR * float64(l.EntryIdx+1)
			}
		}
		return n, sum
	}
	a := Params{L: 30, K: 2, ATRPeriod: 20, StopATR: 3}
	n0, s0 := sig(a, 10)
	if n0 < 20 {
		t.Fatalf("too few legs %d", n0)
	}
	for name, p := range map[string]Params{
		"L14": {L: 14, K: 2, ATRPeriod: 20, StopATR: 3},
		"k3":  {L: 30, K: 3, ATRPeriod: 20, StopATR: 3},
		"K2":  {L: 30, K: 2, ATRPeriod: 20, StopATR: 2},
	} {
		if n, s := sig(p, 10); n == n0 && s == s0 {
			t.Errorf("%s inert", name)
		}
	}
	if _, s := sig(a, 0); s == s0 {
		t.Error("fee inert")
	}
}
