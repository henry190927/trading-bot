package main

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func bar(i int, h, l, c float64) market.Candle {
	return market.Candle{
		OpenTime:  base.Add(time.Duration(i) * time.Hour),
		CloseTime: base.Add(time.Duration(i+1)*time.Hour - time.Millisecond),
		Open:      c, High: h, Low: l, Close: c,
	}
}

// flat returns n identical candles that cannot form a gap with each other:
// every High is the same, so no Low ever clears a previous High.
func flat(n int) []market.Candle {
	out := make([]market.Candle, n)
	for i := range out {
		out[i] = bar(i, 100, 99, 99.5)
	}
	return out
}

func ones(n int) []float64 {
	a := make([]float64, n)
	for i := range a {
		a[i] = 1.0
	}
	return a
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFindFVGs(t *testing.T) {
	// Hand-traced. Gap at i is between cs[i-2] and cs[i].
	//   i=2: cs[0].High 10 < cs[2].Low 11            → bull [10, 11]
	//   i=3: cs[1].High 12 < cs[3].Low 14            → bull [12, 14]
	//   i=4: cs[2].High 15 vs cs[4].Low 15 — EQUAL, the test is strict → none
	//   i=5: cs[5].High 10 < cs[3].Low 14            → bear [10, 14]
	cs := []market.Candle{
		bar(0, 10, 8, 9),
		bar(1, 12, 9, 11),
		bar(2, 15, 11, 14),
		bar(3, 16, 14, 15),
		bar(4, 17, 15, 16),
		bar(5, 10, 8, 9),
	}
	got := FindFVGs(cs)
	want := []FVG{
		{Lo: 10, Hi: 11, Bullish: true, FormedIdx: 2},
		{Lo: 12, Hi: 14, Bullish: true, FormedIdx: 3},
		{Lo: 10, Hi: 14, Bullish: false, FormedIdx: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d gaps, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if !near(g.Lo, w.Lo) || !near(g.Hi, w.Hi) || g.Bullish != w.Bullish || g.FormedIdx != w.FormedIdx {
			t.Errorf("gap %d: got %+v, want %+v", i, g, w)
		}
	}
}

// longSeries: 30 flat bars, then a gap-up, then a dip back into the gap.
//
//	i=30  H 101   L 100.5  C 101    cs[28].High 100 < 100.5 → bull gap [100, 100.5]
//	i=31  H 102   L 99.8   C 101.5  no new gap (99.8 < cs[29].High 100)
//
// At i=31 the gap is eligible (FormedIdx 30 <= 30) and:
//
//	prev.Close 101 > Hi 100.5 ✓ came from above
//	bar.Low   99.8 <= Hi 100.5 ✓ dipped in
//	bar.Close 101.5 > Lo 100   ✓ held it
//
// stop = Lo − 0.25·ATR(1) = 99.75 · risk = 101.5 − 99.75 = 1.75 · TP = 101.5 + 2·1.75 = 105
func longSeries() []market.Candle {
	cs := flat(30)
	return append(cs, bar(30, 101, 100.5, 101), bar(31, 102, 99.8, 101.5))
}

func TestLongFire(t *testing.T) {
	cs := longSeries()
	f := genFVGFires(cs, ones(len(cs)), "BTC", 0.25, 0.25, 2.0, 50, "auto", nil)
	if len(f) != 1 {
		t.Fatalf("got %d fires, want 1: %+v", len(f), f)
	}
	g := f[0]
	if g.Side != "long" || !near(g.Entry, 101.5) || !near(g.Stop, 99.75) || !near(g.TP, 105) {
		t.Errorf("got %s entry %.4f stop %.4f tp %.4f, want long 101.5 / 99.75 / 105",
			g.Side, g.Entry, g.Stop, g.TP)
	}
	if !g.Time.Equal(cs[31].CloseTime) {
		t.Errorf("fired at %v, want bar 31 close %v", g.Time, cs[31].CloseTime)
	}
}

// Mirror of longSeries.
//
//	i=30  H 98.5  L 97    C 97.2  cs[28].Low 99 > 98.5 → bear gap [98.5, 99]
//	i=31  H 99.2  L 97.5  C 98.3  no new gap
//
// prev.Close 97.2 < Lo 98.5 ✓ · bar.High 99.2 >= Lo 98.5 ✓ · bar.Close 98.3 < Hi 99 ✓
// stop = Hi + 0.25 = 99.25 · risk = 99.25 − 98.3 = 0.95 · TP = 98.3 − 1.9 = 96.4
func TestShortFire(t *testing.T) {
	cs := append(flat(30), bar(30, 98.5, 97, 97.2), bar(31, 99.2, 97.5, 98.3))
	f := genFVGFires(cs, ones(len(cs)), "BTC", 0.25, 0.25, 2.0, 50, "auto", nil)
	if len(f) != 1 {
		t.Fatalf("got %d fires, want 1: %+v", len(f), f)
	}
	g := f[0]
	if g.Side != "short" || !near(g.Entry, 98.3) || !near(g.Stop, 99.25) || !near(g.TP, 96.4) {
		t.Errorf("got %s entry %.4f stop %.4f tp %.4f, want short 98.3 / 99.25 / 96.4",
			g.Side, g.Entry, g.Stop, g.TP)
	}
}

// Each filter gets its own case, and each asserts the fire DISAPPEARS. A param
// that cannot change the output is a bug, not a safe default.
func TestFiltersAreLive(t *testing.T) {
	cs := longSeries()
	atr := ones(len(cs))
	cases := []struct {
		name                  string
		minGap, bufATR, rMult float64
		maxAge                int
		side                  string
		want                  int
	}{
		{"baseline", 0.25, 0.25, 2.0, 50, "auto", 1},
		// gap height is 0.5; a 1.0·ATR floor rejects it at formation
		{"min-gap too high", 1.0, 0.25, 2.0, 50, "auto", 0},
		// gap formed at 30, touched at 31: age 1 > 0
		{"max-age zero", 0.25, 0.25, 2.0, 0, "auto", 0},
		{"side short only", 0.25, 0.25, 2.0, 50, "short", 0},
		{"side long only", 0.25, 0.25, 2.0, 50, "long", 1},
	}
	for _, c := range cases {
		got := len(genFVGFires(cs, atr, "BTC", c.minGap, c.bufATR, c.rMult, c.maxAge, c.side, nil))
		if got != c.want {
			t.Errorf("%s: got %d fires, want %d", c.name, got, c.want)
		}
	}
}

// A bar that reaches into the gap but CLOSES below it has filled the pocket.
// No entry, and the gap is gone for good.
//
//	i=31  H 100.4  L 99  C 99.5 → Close 99.5 < Lo 100
func TestCloseThroughInvalidates(t *testing.T) {
	cs := append(flat(30), bar(30, 101, 100.5, 101), bar(31, 100.4, 99, 99.5),
		// bar 32 pokes back up into where the gap was; it must not fire.
		bar(32, 100.6, 99.4, 100.2))
	f := genFVGFires(cs, ones(len(cs)), "BTC", 0.25, 0.25, 2.0, 50, "auto", nil)
	if len(f) != 0 {
		t.Fatalf("got %d fires, want 0 — a filled gap must not trade: %+v", len(f), f)
	}
}

// The guard that matters most: a fire at bar i must be reproducible from the
// bars up to and including i, with nothing after it visible.
func TestNoLookAhead(t *testing.T) {
	cs := append(longSeries(),
		bar(32, 103, 101, 102.5), bar(33, 104, 102, 103.5),
		bar(34, 106, 104, 105.5), bar(35, 107, 105, 106))
	full := genFVGFires(cs, ones(len(cs)), "BTC", 0.25, 0.25, 2.0, 50, "auto", nil)
	if len(full) == 0 {
		t.Fatal("no fires on the full series — the fixture proves nothing")
	}
	for _, f := range full {
		// Find the bar this fire closed on, and re-run on that prefix only.
		cut := -1
		for i, c := range cs {
			if c.CloseTime.Equal(f.Time) {
				cut = i + 1
				break
			}
		}
		if cut < 0 {
			t.Fatalf("fire at %v matches no bar close", f.Time)
		}
		pre := genFVGFires(cs[:cut], ones(cut), "BTC", 0.25, 0.25, 2.0, 50, "auto", nil)
		if len(pre) == 0 {
			t.Fatalf("fire at %v vanishes when later bars are hidden — look-ahead", f.Time)
		}
		last := pre[len(pre)-1]
		if !last.Time.Equal(f.Time) || !near(last.Entry, f.Entry) || !near(last.Stop, f.Stop) ||
			!near(last.TP, f.TP) || last.Side != f.Side {
			t.Errorf("prefix run differs at %v:\n  prefix %+v\n  full   %+v", f.Time, last, f)
		}
	}
}

// An outside bar can touch a bullish gap below and a bearish gap above at
// once. That is two opposite reads on one candle, and the bar must produce
// neither.
//
// The fixture has to EARN that claim, so it asserts three runs: side=long
// fires, side=short fires, side=auto does not. A bar that only ever had one
// live gap would pass a zero-fire assertion for the wrong reason.
//
// Hand-traced. flat(30) at H100/L99, then:
//
//	idx  H       L       C       gap formed (vs i-2)
//	30   94.9    94      94.2    bear [94.9, 99]   vs cs[28].Low 99
//	31   94.5    93.8    94.0    bear [94.5, 99]   vs cs[29].Low 99
//	32   94.0    93.7    93.9    none (94.0 < 94.0 is false — strict)
//	33   94.0    93.7    93.9    none
//	34   94.4    93.7    93.9    none
//	35   94.4    94.1    94.3    bull [94.0, 94.1]  vs cs[33].High 94.0
//	36   94.45   94.15   94.3    none; High 94.45 stays UNDER the 94.5 bear edge
//	37   95.0    93.5    94.3    the outside bar
//
// Nothing fires before 37: every bar's High stays below 94.5, and bar 36's Low
// 94.15 stops just above the bull gap's 94.1 ceiling. At 37 the bull gap is
// touched from above AND both bear gaps from below.
func contradictionSeries() []market.Candle {
	return append(flat(30),
		bar(30, 94.9, 94, 94.2),
		bar(31, 94.5, 93.8, 94.0),
		bar(32, 94.0, 93.7, 93.9),
		bar(33, 94.0, 93.7, 93.9),
		bar(34, 94.4, 93.7, 93.9),
		bar(35, 94.4, 94.1, 94.3),
		bar(36, 94.45, 94.15, 94.3),
		bar(37, 95.0, 93.5, 94.3),
	)
}

func TestContradictoryBarFiresNothing(t *testing.T) {
	cs := contradictionSeries()
	atr := ones(len(cs))
	at37 := cs[37].CloseTime

	// min-gap 0.05 because the bull pocket is a deliberate 0.10 wide; this
	// case is about direction, not about the quality filter.
	long := genFVGFires(cs, atr, "BTC", 0.05, 0.25, 2.0, 50, "long", nil)
	if len(long) != 1 || !long[0].Time.Equal(at37) || long[0].Side != "long" {
		t.Fatalf("side=long: want one long at bar 37, got %+v", long)
	}
	short := genFVGFires(cs, atr, "BTC", 0.05, 0.25, 2.0, 50, "short", nil)
	if len(short) != 1 || !short[0].Time.Equal(at37) || short[0].Side != "short" {
		t.Fatalf("side=short: want one short at bar 37, got %+v", short)
	}
	// Both reads are available on that bar, so auto must decline it.
	if auto := genFVGFires(cs, atr, "BTC", 0.05, 0.25, 2.0, 50, "auto", nil); len(auto) != 0 {
		t.Errorf("side=auto: want no fire on a contradictory bar, got %+v", auto)
	}
}
