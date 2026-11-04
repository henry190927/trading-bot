package autotrade

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// Hourly bars from `from`, n of them. Prices are irrelevant to coversFire.
func hourBars(from time.Time, n int) []market.Candle {
	cs := make([]market.Candle, n)
	for i := range cs {
		cs[i] = bar(from.Add(time.Duration(i)*time.Hour), 99, 101)
	}
	return cs
}

func TestCoversFire(t *testing.T) {
	t10 := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	fireAt := t10.Add(13 * time.Second) // the daemon fires seconds after a bar closes
	cases := []struct {
		name string
		at   time.Time
		from time.Time
		want bool
	}{
		{"window starts an hour before the fire", fireAt, t10.Add(-time.Hour), true},
		{"window starts with the bar the fire landed in", fireAt, t10, true},
		{"fire exactly on the first bar's open", t10, t10, true},
		{"window starts one bar after the fire", fireAt, t10.Add(time.Hour), false},
		{"window starts weeks after the fire (the 2026-11-04 panel)", fireAt, t10.Add(48 * 24 * time.Hour), false},
		{"fire newer than the last candle", t10.Add(30 * time.Hour), t10, true},
	}
	for _, c := range cases {
		f := PaperFire{Time: c.at}
		if got := coversFire(f, hourBars(c.from, 24)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The bug, end to end: a long whose bracket the window's first bar straddles.
// Replayed on the late window it read as a take-profit it never had.
func TestEvaluateFireOutsideTheWindowIsUnscored(t *testing.T) {
	t0 := time.Date(2026, 8, 27, 4, 0, 13, 0, time.UTC)
	f := PaperFire{Time: t0, Side: "long", Market: true, Entry: 100, Stop: 98, TP: 104}

	late := []market.Candle{bar(t0.Add(30*24*time.Hour).Truncate(time.Hour), 103, 110)}
	if got := EvaluateFire(f, late, 6); got.Status != OutUnscored || got.NetR != 0 {
		t.Fatalf("late window: got %s %.2f, want unscored 0", got.Status, got.NetR)
	}

	// The same fire with its own bars still scores: bar 0 holds, bar 1 tags tp.
	h := t0.Truncate(time.Hour)
	own := []market.Candle{bar(h, 99, 101), bar(h.Add(time.Hour), 101, 105)}
	if got := EvaluateFire(f, own, 6); got.Status != OutTP || got.NetR != 2 {
		t.Fatalf("own window: got %s %.2f, want tp 2", got.Status, got.NetR)
	}
}

// An unscored fire must not hold its rule's slot. Before the guard, the old
// fire "filled" on the window's first bar and "exited" at T0+4h, absorbing the
// T0+2h fire of the same rule: one position where there are two.
func TestUnscoredHoldsNoSlot(t *testing.T) {
	t0 := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	mk := func(at time.Time) PaperFire {
		return PaperFire{Symbol: "ETH", Strategy: "engine", Side: "long", Market: true,
			Time: at.Add(13 * time.Second), Entry: 100, Stop: 98, TP: 104, Margin: 35}
	}
	old, next := mk(t0), mk(t0.Add(2*time.Hour))
	cs := []market.Candle{
		bar(t0.Add(1*time.Hour), 99, 101),  // the window starts after `old`
		bar(t0.Add(2*time.Hour), 99, 101),  // `next` fills here and holds
		bar(t0.Add(3*time.Hour), 101, 105), // tp for anything filled before
	}
	resolve := func(f PaperFire) Outcome { return EvaluateFire(f, cs, 6) }

	pos, blocked := ReplayWithCaps([]PaperFire{old, next}, cfgWith(0, 0, 0, 0, rule("ETH", "engine")), 6, 6, time.Hour, resolve)
	if len(blocked) != 0 || len(pos) != 2 {
		t.Fatalf("ReplayWithCaps: %d positions, %d blocked; want 2 and 0", len(pos), len(blocked))
	}
	if pos[0].Outcome.Status != OutUnscored || pos[1].Outcome.Status != OutTP {
		t.Fatalf("ReplayWithCaps: got %s then %s, want unscored then tp", pos[0].Outcome.Status, pos[1].Outcome.Status)
	}

	if dp := DedupFires([]PaperFire{old, next}, 6, 6, time.Hour, resolve); len(dp) != 2 {
		t.Fatalf("DedupFires: %d positions, want 2", len(dp))
	}

	s := Summarize([]Outcome{pos[0].Outcome, pos[1].Outcome})
	if s.Unscored != 1 || s.TP != 1 || s.NetR != 2 {
		t.Fatalf("Summarize: %+v, want 1 unscored, 1 tp, netR 2", s)
	}
}
