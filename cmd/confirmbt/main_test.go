package main

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

func cndl(i int, o, h, l, c float64) market.Candle {
	t := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
	return market.Candle{
		OpenTime: t, CloseTime: t.Add(time.Hour - time.Millisecond),
		Open: o, High: h, Low: l, Close: c, Volume: 100,
	}
}

// zigzag builds an uptrend out of impulse+pullback legs so FindSwingPoints has
// real local extremes to work with.
//
// The first version of this helper was a monotonic staircase, which produced NO
// swing pivots and therefore no pivot zone — every zone test silently SKIPPED
// and asserted nothing. A skipping test is not a test, so the generator is
// verified below to actually yield directional zones.
func zigzag() []market.Candle {
	var cs []market.Candle
	px, i := 100.0, 0
	for leg := 0; leg < 14; leg++ {
		for k := 0; k < 8; k++ { // impulse up: +16
			o := px
			px += 2.0
			cs = append(cs, cndl(i, o, px+0.3, o-0.2, px))
			i++
		}
		// Pullback of -10, a ~62% retrace, so price lands INSIDE the zone's
		// 0.5-0.705 band. The first attempt used -4 (25%) and never reached
		// the band at all: 17 zone bars, 0 events.
		for k := 0; k < 5; k++ {
			o := px
			px -= 2.0
			cs = append(cs, cndl(i, o, o+0.2, px-0.3, px))
			i++
		}
	}
	return cs
}

// Guards the fixture itself: if this fails, every test below is vacuous.
func TestFixtureProducesDirectionalZones(t *testing.T) {
	cs := zigzag()
	if len(cs) < 120 {
		t.Fatalf("fixture too short: %d bars", len(cs))
	}
	n := 0
	for i := 100; i < len(cs); i++ {
		st := signal.AnalyzeStructure(cs[:i], 2)
		if st.Zone != nil && (st.Zone.Dir == signal.StructUptrend || st.Zone.Dir == signal.StructDowntrend) {
			n++
		}
	}
	if n == 0 {
		t.Fatal("fixture produces no directional pivot zone — the zone tests below would all skip and assert nothing")
	}
	if evs := detectZoneEvents(cs, 4); len(evs) == 0 {
		t.Fatalf("fixture yields %d zone bars but 0 events — detection is broken or the latch never opens", n)
	}
}

// Events must be once per ZONE OCCUPANCY, not once per bar. Re-firing every
// bar price sits inside the zone would inflate the sample with the same trade,
// which is how a harness reports a confident number about nothing.
func TestDetectZoneEventsOncePerOccupancy(t *testing.T) {
	cs := zigzag()
	evs := detectZoneEvents(cs, 4)
	if len(evs) == 0 {
		t.Fatal("no zone events — see TestFixtureProducesDirectionalZones")
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Bar == evs[i-1].Bar {
			t.Errorf("two events on the same bar: %+v", evs[i])
		}
		// Consecutive bars would mean the occupancy latch is not holding.
		if evs[i].Bar == evs[i-1].Bar+1 {
			t.Errorf("events on consecutive bars %d,%d — the in-zone latch is not holding",
				evs[i-1].Bar, evs[i].Bar)
		}
	}
}

// Every emitted event must be internally coherent: a stop on the losing side
// of entry and a target on the winning side, or the fire is not a trade.
func TestDetectZoneEventsGeometry(t *testing.T) {
	cs := zigzag()
	evs := detectZoneEvents(cs, 4)
	if len(evs) == 0 {
		t.Fatal("no zone events — see TestFixtureProducesDirectionalZones")
	}
	for _, e := range evs {
		switch e.Side {
		case "long":
			if e.Invalidate >= e.Near {
				t.Errorf("long: invalidate %.2f must be BELOW the near edge %.2f", e.Invalidate, e.Near)
			}
		case "short":
			if e.Invalidate <= e.Near {
				t.Errorf("short: invalidate %.2f must be ABOVE the near edge %.2f", e.Invalidate, e.Near)
			}
		default:
			t.Errorf("event with no side: %+v", e)
		}
		if e.ConfirmBar >= 0 {
			if e.ConfirmBar < e.Bar {
				t.Errorf("confirmation at bar %d precedes the event at %d", e.ConfirmBar, e.Bar)
			}
			if e.ConfirmPx == 0 {
				t.Errorf("confirmed event carries no confirm price: %+v", e)
			}
		} else if e.ConfirmPx != 0 {
			t.Errorf("unconfirmed event carries a confirm price: %+v", e)
		}
	}
}

// The confirmation window must be respected — a wider window can only find
// MORE confirmations, never fewer, and must never look past its limit.
func TestConfirmWindowIsMonotone(t *testing.T) {
	cs := zigzag()
	count := func(n int) (confirmed, total int) {
		for _, e := range detectZoneEvents(cs, n) {
			total++
			if e.ConfirmBar >= 0 {
				confirmed++
			}
		}
		return
	}
	c2, t2 := count(2)
	c8, t8 := count(8)
	if t2 != t8 {
		t.Errorf("event count changed with the confirm window (%d vs %d) — the window must only affect CONFIRMATION, not detection", t2, t8)
	}
	if c8 < c2 {
		t.Errorf("a wider window found FEWER confirmations (%d at 8 bars vs %d at 2)", c8, c2)
	}
	for _, e := range detectZoneEvents(cs, 2) {
		if e.ConfirmBar >= 0 && e.ConfirmBar > e.Bar+2 {
			t.Errorf("confirmation at bar %d is outside the 2-bar window from %d", e.ConfirmBar, e.Bar)
		}
	}
}

// No lookahead: structure is computed from cs[:i], so an event at bar i must
// not depend on bar i's own close. Truncating the series after an event must
// leave that event unchanged.
func TestDetectZoneEventsNoLookahead(t *testing.T) {
	cs := zigzag()
	full := detectZoneEvents(cs, 4)
	if len(full) == 0 {
		t.Fatal("no zone events — see TestFixtureProducesDirectionalZones")
	}
	first := full[0]
	// Cut the series to end right after the event bar. Detection of that
	// event must be identical — nothing after it informed it.
	trunc := detectZoneEvents(cs[:first.Bar+1], 4)
	if len(trunc) == 0 {
		t.Fatalf("event at bar %d vanished when later bars were removed — it was using future data", first.Bar)
	}
	got := trunc[len(trunc)-1]
	if got.Bar != first.Bar || got.Side != first.Side || got.Near != first.Near || got.Invalidate != first.Invalidate {
		t.Errorf("event changed when later bars were removed:\n full  %+v\n trunc %+v", first, got)
	}
}

// A series with no directional trend must produce no fade setups — the
// strategy is a TREND-aligned fade, and a flat tape has nothing to align to.
func TestDetectZoneEventsSkipsNonDirectional(t *testing.T) {
	var cs []market.Candle
	for i := 0; i < 150; i++ {
		cs = append(cs, cndl(i, 100, 100.3, 99.7, 100))
	}
	if evs := detectZoneEvents(cs, 4); len(evs) != 0 {
		t.Errorf("flat series produced %d fade events: %+v", len(evs), evs[:1])
	}
}

func TestMaxHelper(t *testing.T) {
	if max(3, 5) != 5 || max(5, 3) != 5 || max(-1, 0) != 0 {
		t.Error("max is wrong")
	}
}
