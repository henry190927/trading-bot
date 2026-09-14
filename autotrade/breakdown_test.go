package autotrade

import (
	"math"
	"testing"
)

// Seven positions, every figure below folded by hand from this table rather
// than read off a run:
//
//	engine BTC long  tp   +1.5
//	engine ETH short tp   +2.0
//	engine BTC long  stop -1.0
//	sweep  ETH long  stop -1.0
//	sweep  SOL long  stop -1.0
//	sweep  SUI short open  (unreal +0.5)
//	range  BTC long  no-fill
func bdFixture() []Position {
	p := func(strat, sym, side string, st OutcomeStatus, netR, unreal float64) Position {
		return Position{
			Fire:    PaperFire{Symbol: sym, Strategy: strat, Side: side},
			Outcome: Outcome{Status: st, NetR: netR, UnrealR: unreal},
		}
	}
	return []Position{
		p("engine", "BTC", "long", OutTP, 1.5, 0),
		p("engine", "ETH", "short", OutTP, 2.0, 0),
		p("engine", "BTC", "long", OutStop, -1.0, 0),
		p("sweep", "ETH", "long", OutStop, -1.0, 0),
		p("sweep", "SOL", "long", OutStop, -1.0, 0),
		p("sweep", "SUI", "short", OutOpen, 0, 0.5),
		p("range", "BTC", "long", OutNoFill, 0, 0),
	}
}

func eq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBreakdownByStrategy(t *testing.T) {
	got := Breakdown(bdFixture(), ByStrategy)

	// NetR descending: engine 1.5+2.0-1.0 = +2.5, range 0 (nothing settled),
	// sweep -1.0-1.0 = -2.0.
	want := []struct {
		key          string
		total, tp    int
		stop, noFill int
		open         int
		netR, rPer   float64
	}{
		{"engine", 3, 2, 1, 0, 0, +2.5, 2.5 / 3},
		{"range", 1, 0, 0, 1, 0, 0, 0},       // no-fill never settles → RPerTrade 0, not -inf
		{"sweep", 3, 0, 2, 0, 1, -2.0, -1.0}, // open is Filled but unsettled
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Key != w.key {
			t.Errorf("group %d key = %q, want %q (sort is NetR desc)", i, g.Key, w.key)
			continue
		}
		if g.Total != w.total || g.TP != w.tp || g.Stop != w.stop || g.NoFill != w.noFill || g.Open != w.open {
			t.Errorf("%s counts = total %d tp %d stop %d nofill %d open %d, want %d/%d/%d/%d/%d",
				w.key, g.Total, g.TP, g.Stop, g.NoFill, g.Open, w.total, w.tp, w.stop, w.noFill, w.open)
		}
		if !eq(g.NetR, w.netR) {
			t.Errorf("%s NetR = %+.4f, want %+.4f", w.key, g.NetR, w.netR)
		}
		if !eq(g.RPerTrade(), w.rPer) {
			t.Errorf("%s RPerTrade = %+.4f, want %+.4f", w.key, g.RPerTrade(), w.rPer)
		}
	}
	// engine wins 2 of its 3 settled; sweep wins none of its 2.
	if wr := got[0].WinRate; !eq(wr, 2.0/3.0) {
		t.Errorf("engine WinRate = %.4f, want %.4f", wr, 2.0/3.0)
	}
	if wr := got[2].WinRate; !eq(wr, 0) {
		t.Errorf("sweep WinRate = %.4f, want 0", wr)
	}
	// The open position's floating R belongs to its own group, not the book's
	// realized total.
	if !eq(got[2].UnrealR, 0.5) {
		t.Errorf("sweep UnrealR = %+.4f, want +0.5", got[2].UnrealR)
	}
}

func TestBreakdownBySideAndSymbol(t *testing.T) {
	// long: +1.5 -1.0 -1.0 -1.0 = -1.5 over 4 settled; short: +2.0 over 1.
	side := Breakdown(bdFixture(), BySide)
	for i, w := range []struct {
		key        string
		netR, rPer float64
	}{{"short", +2.0, +2.0}, {"long", -1.5, -0.375}} {
		if side[i].Key != w.key || !eq(side[i].NetR, w.netR) || !eq(side[i].RPerTrade(), w.rPer) {
			t.Errorf("side %d = %s NetR %+.4f R/trd %+.4f, want %s %+.4f %+.4f",
				i, side[i].Key, side[i].NetR, side[i].RPerTrade(), w.key, w.netR, w.rPer)
		}
	}
	// ETH +2.0-1.0 = +1.0; BTC +1.5-1.0 = +0.5; SUI open only = 0; SOL = -1.0.
	sym := Breakdown(bdFixture(), BySymbol)
	for i, w := range []struct {
		key  string
		netR float64
	}{{"ETH", +1.0}, {"BTC", +0.5}, {"SUI", 0}, {"SOL", -1.0}} {
		if sym[i].Key != w.key || !eq(sym[i].NetR, w.netR) {
			t.Errorf("symbol %d = %s %+.4f, want %s %+.4f", i, sym[i].Key, sym[i].NetR, w.key, w.netR)
		}
	}
}

// The whole point: a breakdown must PARTITION the book. A page that scrapes a
// display-capped table gets group numbers that describe a subset while reading
// like they describe the book.
func TestBreakdownPartitionsTheBook(t *testing.T) {
	ps := bdFixture()
	whole := Summarize(func() []Outcome {
		out := make([]Outcome, len(ps))
		for i, p := range ps {
			out[i] = p.Outcome
		}
		return out
	}())

	for _, c := range []struct {
		name string
		key  func(PaperFire) string
	}{{"strategy", ByStrategy}, {"symbol", BySymbol}, {"side", BySide}} {
		var total, tp, stop int
		var netR, unrealR float64
		for _, g := range Breakdown(ps, c.key) {
			total += g.Total
			tp += g.TP
			stop += g.Stop
			netR += g.NetR
			unrealR += g.UnrealR
		}
		if total != len(ps) || total != whole.Total {
			t.Errorf("by %s: group totals = %d, want %d", c.name, total, len(ps))
		}
		if tp != whole.TP || stop != whole.Stop {
			t.Errorf("by %s: tp/stop = %d/%d, want %d/%d", c.name, tp, stop, whole.TP, whole.Stop)
		}
		if !eq(netR, whole.NetR) || !eq(netR, 0.5) {
			t.Errorf("by %s: summed NetR = %+.4f, want %+.4f (book) = +0.5", c.name, netR, whole.NetR)
		}
		if !eq(unrealR, whole.UnrealR) {
			t.Errorf("by %s: summed UnrealR = %+.4f, want %+.4f", c.name, unrealR, whole.UnrealR)
		}
	}
}

func TestBreakdownTieBreakAndEmpty(t *testing.T) {
	// Two groups with identical NetR must come back in key order, so a refresh
	// does not reshuffle rows that did not change.
	ps := []Position{
		{Fire: PaperFire{Symbol: "ZZZ", Strategy: "b"}, Outcome: Outcome{Status: OutNoFill}},
		{Fire: PaperFire{Symbol: "AAA", Strategy: "a"}, Outcome: Outcome{Status: OutNoFill}},
	}
	if g := Breakdown(ps, BySymbol); g[0].Key != "AAA" || g[1].Key != "ZZZ" {
		t.Errorf("tie order = %s,%s, want AAA,ZZZ", g[0].Key, g[1].Key)
	}
	if g := Breakdown(nil, ByStrategy); len(g) != 0 {
		t.Errorf("Breakdown(nil) = %+v, want empty", g)
	}
	// A blank key must still be a group — dropping it would break the partition.
	blank := Breakdown([]Position{{Outcome: Outcome{Status: OutTP, NetR: 1}}}, ByStrategy)
	if len(blank) != 1 || blank[0].Key != "(unset)" || !eq(blank[0].NetR, 1) {
		t.Errorf("blank key = %+v, want one (unset) group with NetR +1", blank)
	}
}
