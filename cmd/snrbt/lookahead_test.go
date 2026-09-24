package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// An HTF swing is not a swing until `strength` bars after it have printed. A
// backtest that tests a base bar against every swing in the series is reading
// the future, and it inflates results in the direction that looks like an edge
// — the levels it "knew about" are exactly the ones price later respected.
//
// This drives genSNRFires over a series whose ONLY qualifying HTF swing is
// confirmed after every base bar, and asserts it fires nothing. Drop the
// CloseTime guard and this produces fires.
func TestNoLookAheadOnHTFSwings(t *testing.T) {
	const strength = 3
	base := t0()

	// HTF: a clear swing high at index 10, confirmed at index 13 — timed so
	// that its confirmation closes AFTER the last base bar opens.
	var hcs []market.Candle
	for i := 0; i < 20; i++ {
		p := 100.0 + float64(i)*0.1
		if i == 10 {
			p = 120.0 // the swing high
		}
		open := base.Add(time.Duration(i) * 4 * time.Hour)
		hcs = append(hcs, market.Candle{
			OpenTime: open, CloseTime: open.Add(4 * time.Hour),
			Open: p, High: p, Low: p - 1, Close: p - 0.5,
		})
	}
	// Sanity: the fixture must actually contain the swing, or this proves nothing.
	sw := signal.FindSwingPoints(hcs, strength, 0)
	var found bool
	for _, p := range sw {
		if p.IsTop && p.Price == 120.0 {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture has no HTF swing high — the test would pass vacuously")
	}

	// Base bars that all OPEN before that swing's confirming HTF candle closes,
	// and that would each reject the 120 level if it were known.
	var cs []market.Candle
	atr := make([]float64, 0, 40)
	for i := 0; i < 40; i++ {
		open := base.Add(time.Duration(i) * time.Hour)
		cs = append(cs, market.Candle{
			OpenTime: open, CloseTime: open.Add(time.Hour),
			Open: 119, High: 120.5, Low: 118, Close: 119.2, // pokes 120 and closes below
		})
		atr = append(atr, 1.0)
	}
	confirm := hcs[10+strength].CloseTime
	if !cs[len(cs)-1].OpenTime.Before(confirm) {
		t.Fatalf("fixture misaligned: last base bar opens %v, swing confirms %v — "+
			"every base bar must open BEFORE confirmation", cs[len(cs)-1].OpenTime, confirm)
	}

	fires := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "auto")
	if len(fires) != 0 {
		t.Errorf("fired %d time(s) on a swing that was not yet confirmed — look-ahead", len(fires))
	}
}

// And the mirror: once confirmation HAS closed, the same setup must fire, or
// the guard is just suppressing everything and the zero above means nothing.
func TestFiresOnceTheSwingIsConfirmed(t *testing.T) {
	const strength = 3
	base := t0()

	// Swing at index 5, not 2: fractal strength 3 needs three bars on EITHER
	// side, so an index below `strength` can never be a swing and the fixture
	// would contain nothing to confirm.
	const swingAt = 5
	var hcs []market.Candle
	for i := 0; i < 20; i++ {
		p := 100.0 + float64(i)*0.1
		if i == swingAt {
			p = 120.0
		}
		open := base.Add(time.Duration(i) * 4 * time.Hour)
		hcs = append(hcs, market.Candle{
			OpenTime: open, CloseTime: open.Add(4 * time.Hour),
			Open: p, High: p, Low: p - 1, Close: p - 0.5,
		})
	}
	var ok bool
	for _, p := range signal.FindSwingPoints(hcs, strength, 0) {
		if p.IsTop && p.Price == 120.0 {
			ok = true
		}
	}
	if !ok {
		t.Fatal("fixture has no HTF swing high — the test would prove nothing")
	}
	// Base bars start well AFTER that swing is confirmed.
	startAt := hcs[swingAt+strength].CloseTime.Add(time.Hour)
	var cs []market.Candle
	atr := make([]float64, 0, 40)
	for i := 0; i < 40; i++ {
		open := startAt.Add(time.Duration(i) * time.Hour)
		cs = append(cs, market.Candle{
			OpenTime: open, CloseTime: open.Add(time.Hour),
			Open: 119, High: 120.5, Low: 118, Close: 119.2,
		})
		atr = append(atr, 1.0)
	}
	fires := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "auto")
	if len(fires) == 0 {
		t.Fatal("no fires on a confirmed swing — the look-ahead guard suppresses everything")
	}
	f := fires[0]
	if f.Side != "short" || f.Entry != 119.2 {
		t.Errorf("fire = %s @ %v, want short @ 119.2 (the reject close)", f.Side, f.Entry)
	}
	if f.Stop <= f.Entry {
		t.Errorf("short stop %v must sit above the entry %v", f.Stop, f.Entry)
	}
	// TP is 2R below the entry.
	if want := f.Entry - 2*(f.Stop-f.Entry); f.TP != want {
		t.Errorf("TP = %v, want %v (2R)", f.TP, want)
	}
	// side=long must produce nothing on a rejected HIGH.
	if got := genSNRFires(cs, hcs, atr, "TEST", strength, 0.002, 0.25, 2.0, "long"); len(got) != 0 {
		t.Errorf("side=long fired %d time(s) on a swing-high rejection", len(got))
	}
}

func t0() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }
