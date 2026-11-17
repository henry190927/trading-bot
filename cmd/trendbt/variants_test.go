package main

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// flatBars: n 4h bars O=C=100 in a contracting range (H = 101 − 0.001k,
// L = 99 + 0.001k), so no bar ever touches its prior channel.
func flatBars(n int) []market.Candle {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, n)
	for i := range cs {
		ot := t0.Add(time.Duration(i) * 4 * time.Hour)
		k := float64(i) * 0.001
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(4*time.Hour - time.Millisecond), Open: 100, High: 101 - k, Low: 99 + k, Close: 100}
	}
	return cs
}

func withBar(cs []market.Candle, i int, o, h, l, c float64) []market.Candle {
	cs = append([]market.Candle(nil), cs...)
	cs[i].Open, cs[i].High, cs[i].Low, cs[i].Close = o, h, l, c
	return cs
}

func touchOne(t *testing.T, cs []market.Candle) ([]autostrat.TrendTrade, int) {
	t.Helper()
	p := autostrat.DefaultHTFTrendParams()
	return simulateTouch(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 0)
}

func TestTouchMechanics(t *testing.T) {
	base := flatBars(40)
	const i = 30
	p := autostrat.DefaultHTFTrendParams()
	if tr, both := touchOne(t, base); len(tr) != 0 || both != 0 {
		t.Fatalf("contracting base touched: %d trades, both %d", len(tr), both)
	}
	hi, lo := base[i-20].High, base[i-20].Low      // 100.99 / 99.01
	d := 3 * indicator.ATR(base, p.ATRPeriod)[i-1] // ATR known before bar i; bar i itself does not change it
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	// Plain touch: fill at the level, stop 3×ATR[i-1] below it, not at the close.
	tr, _ := touchOne(t, withBar(base, i, 100, 102, 99.5, 101.5))
	if len(tr) == 0 || tr[0].EntryIdx != i || tr[0].Side != "long" || !near(tr[0].Entry, hi) || !near(tr[0].InitStop, hi-d) {
		t.Fatalf("plain touch: %+v (hi %v d %v)", tr, hi, d)
	}
	// Gap through the level: fill at the open.
	tr, _ = touchOne(t, withBar(base, i, 103, 104, 102.5, 103.5))
	if len(tr) == 0 || tr[0].Entry != 103 || !near(tr[0].InitStop, 103-d) {
		t.Fatalf("gap: %+v", tr)
	}
	// Both edges touched, open nearer the upper → long first; its bar also
	// reaches the stop → a stop on the entry bar at the stop, −1R gross.
	tr, both := touchOne(t, withBar(base, i, 100.5, 102, 94, 96))
	if both != 1 || len(tr) == 0 || tr[0].Side != "long" || tr[0].ExitIdx != i || !near(tr[0].Exit, hi-d) || tr[0].GrossR != -1 || tr[0].Open {
		t.Fatalf("same-bar stop: both=%d %+v", both, tr)
	}
	// Open nearer the lower → short at the lower edge, stop not reached (high 102).
	tr, _ = touchOne(t, withBar(base, i, 99.5, 102, 94, 96))
	if len(tr) == 0 || tr[0].Side != "short" || !near(tr[0].Entry, lo) || tr[0].ExitIdx == i {
		t.Fatalf("short first: %+v", tr)
	}
	// Exact tie (open at the channel midpoint) → no entry on that bar.
	tr, both = touchOne(t, withBar(base, i, (hi+lo)/2, 102, 98, 100))
	if both != 1 || (len(tr) > 0 && tr[0].EntryIdx == i) {
		t.Fatalf("tie: both=%d %+v", both, tr)
	}
	// The close baseline does NOT fire on a bar that pokes and closes back in.
	cs := withBar(base, i, 100, 102, 99.5, 100.5)
	if c := autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 0); len(c) != 0 {
		t.Fatalf("close fired on a poke: %+v", c)
	}
	if tr, _ = touchOne(t, cs); len(tr) == 0 || tr[0].EntryIdx != i {
		t.Fatalf("touch missed the poke: %+v", tr)
	}
}

// closesBars builds 1d bars from closes (O = previous close, H/L ±0.5%).
func closesBars(cl []float64) []market.Candle {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, len(cl))
	for i, c := range cl {
		o := c
		if i > 0 {
			o = cl[i-1]
		}
		ot := t0.AddDate(0, 0, i)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(24*time.Hour - time.Millisecond), Open: o,
			High: math.Max(o, c) * 1.005, Low: math.Min(o, c) * 0.995, Close: c}
	}
	return cs
}

func TestTSMOMFlips(t *testing.T) {
	// 40 bars falling, then rising: the 5-bar return turns positive once.
	var cl []float64
	for i := 0; i < 40; i++ {
		cl = append(cl, 200-float64(i))
	}
	for i := 0; i < 40; i++ {
		cl = append(cl, 161+3*float64(i))
	}
	cs := closesBars(cl)
	s := tsmomSigns(cs, 5)
	var flips []int
	for i := 6; i < len(s); i++ {
		if s[i] != s[i-1] {
			flips = append(flips, i)
		}
	}
	if len(flips) != 1 {
		t.Fatalf("flips %v", flips)
	}
	p := autostrat.DefaultHTFTrendParams()
	tr, _ := simulateTSMOM(cs, indicator.ATR(cs, p.ATRPeriod), 21, 5, true, true, p, 0)
	if len(tr) != 1 || tr[0].EntryIdx != flips[0] || tr[0].Side != "long" || tr[0].Entry != cs[flips[0]].Close {
		t.Fatalf("tsmom trades %+v (flip at %v)", tr, flips)
	}
	// Short-only: the one flip is long, so nothing.
	if tr, _ := simulateTSMOM(cs, indicator.ATR(cs, p.ATRPeriod), 21, 5, false, true, p, 0); len(tr) != 0 {
		t.Fatalf("short-only took %+v", tr)
	}
}

// TSMOM's walk is the baseline's: rebuilding every SimulateHTFTrend trade from
// its entry and initial stop with trendTrade gives the same trade.
func TestTSMOMWalkIsBaselineWalk(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	for seed := int64(1); seed <= 3; seed++ {
		cs := walk4h(1500, seed)
		for _, w := range autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 10) {
			if g := trendTrade(cs, w.EntryIdx, w.Side, w.Entry, w.InitStop, p.TrailM, 10); g != w {
				t.Fatalf("seed %d: %+v vs %+v", seed, g, w)
			}
		}
	}
}

// No lookahead: poison every bar from `cut` on; every entry before the cut
// (side, bar, fill, initial stop) — and every trade that also exited before
// it — must be identical.
func TestVariantsNoLookahead(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	for seed := int64(1); seed <= 3; seed++ {
		cs := walk4h(2000, seed)
		const cut = 1200
		bad := append([]market.Candle(nil), cs...)
		r := rand.New(rand.NewSource(seed + 100))
		for k := cut; k < len(bad); k++ {
			x := 50 + r.Float64()*500
			bad[k].Open, bad[k].Close = x, x*(0.5+r.Float64())
			bad[k].High = math.Max(bad[k].Open, bad[k].Close) * (1 + r.Float64())
			bad[k].Low = math.Min(bad[k].Open, bad[k].Close) * (1 - 0.5*r.Float64())
		}
		for _, arm := range []struct {
			name string
			run  func([]market.Candle) []autostrat.TrendTrade
		}{
			{"touch", func(c []market.Candle) []autostrat.TrendTrade {
				tr, _ := simulateTouch(c, indicator.ATR(c, p.ATRPeriod), 21, true, true, p, 10)
				return tr
			}},
			{"tsmom30", func(c []market.Candle) []autostrat.TrendTrade {
				tr, _ := simulateTSMOM(c, indicator.ATR(c, p.ATRPeriod), 21, 30, true, true, p, 10)
				return tr
			}},
		} {
			a, b := before(arm.run(cs), cut), before(arm.run(bad), cut)
			if len(a) < 5 || len(a) != len(b) {
				t.Fatalf("%s seed %d: %d vs %d entries before the cut", arm.name, seed, len(a), len(b))
			}
			for k := range a {
				x, y := a[k], b[k]
				if x.Side != y.Side || x.EntryIdx != y.EntryIdx || x.Entry != y.Entry || x.InitStop != y.InitStop {
					t.Fatalf("%s seed %d entry %d: %+v vs %+v", arm.name, seed, k, x, y)
				}
				if x.ExitIdx < cut && x != y {
					t.Fatalf("%s seed %d trade %d closed before the cut differs: %+v vs %+v", arm.name, seed, k, x, y)
				}
			}
		}
	}
}

func before(ts []autostrat.TrendTrade, cut int) []autostrat.TrendTrade {
	var out []autostrat.TrendTrade
	for _, t := range ts {
		if t.EntryIdx < cut {
			out = append(out, t)
		}
	}
	return out
}

// Inert-param guard: each new setting must change the trade list.
func TestVariantsNotInert(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	cs := walk4h(2000, 7)
	atr := indicator.ATR(cs, p.ATRPeriod)
	run := func(entry string, n int) []autostrat.TrendTrade {
		return simulateEntry(entry, n, cs, atr, 21, true, true, p, 10)
	}
	c, tch, m30, m60 := run("close", 30), run("touch", 30), run("tsmom", 30), run("tsmom", 60)
	if !reflect.DeepEqual(c, autostrat.SimulateHTFTrend(cs, atr, 21, true, true, p, 10)) {
		t.Fatal("-entry close is not SimulateHTFTrend")
	}
	for _, x := range []struct {
		name string
		a, b []autostrat.TrendTrade
	}{{"touch vs close", tch, c}, {"tsmom vs close", m30, c}, {"tsmom 30 vs 60", m30, m60}} {
		if len(x.a) == 0 || reflect.DeepEqual(x.a, x.b) {
			t.Fatalf("%s: identical (or empty) — the setting is inert", x.name)
		}
	}
}

// dailyR is a partition of each trade's R: over a span covering everything
// it sums to the total NetR (open trades are marked at the last close = NetR).
func TestDailyRSumsToNetR(t *testing.T) {
	p := autostrat.DefaultHTFTrendParams()
	cs := walk4h(1500, 3)
	tr := autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, p.ATRPeriod), 21, true, true, p, 10)
	var want float64
	for _, x := range tr {
		want += x.NetR
	}
	d := dailyR([]symData{{short: "X", cs: cs, trades: tr}}, cs[0].OpenTime.Add(-48*time.Hour), cs[len(cs)-1].CloseTime.Add(48*time.Hour))
	var got float64
	for _, x := range d {
		got += x
	}
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("daily sum %+.6f vs NetR %+.6f", got, want)
	}
	if r := pearson(d, d); math.Abs(r-1) > 1e-9 {
		t.Fatalf("self-correlation %v", r)
	}
}
