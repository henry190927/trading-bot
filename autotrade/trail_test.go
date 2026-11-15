package autotrade

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

func trailBar(i int, o, h, l, c float64) market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ot := t0.Add(time.Duration(i) * 4 * time.Hour)
	return market.Candle{OpenTime: ot, CloseTime: ot.Add(4*time.Hour - time.Millisecond), Open: o, High: h, Low: l, Close: c}
}

// The fields the backtest never needed and the resolver does: the stop in
// force on the exit bar (it differs from Exit on a gap) and the stop for the
// forming bar while the position is still open. Hand-traced, M=2.
func TestWalkTrailStops(t *testing.T) {
	cs := []market.Candle{
		trailBar(0, 10, 11, 9, 10),
		trailBar(1, 10, 12, 10, 11.5),     // signal: long 11.5, stop 10.5
		trailBar(2, 11.5, 13, 11, 12.8),   // stop 10.5 (min L(0..1)=9)
		trailBar(3, 12.8, 14, 12, 13.5),   // stop max(10.5, min L(1..2)=10) = 10.5
		trailBar(4, 13.5, 14.5, 13, 14),   // stop 11 (min L(2..3))
		trailBar(5, 10.9, 11.2, 10, 10.5), // stop 12 (min L(3..4)); opens 10.9 through it → exit 10.9
	}
	// Open at bar 4: the forming bar 5 would carry min L(3..4) = 12.
	w := WalkTrail(cs[:5], 1, "long", 10.5, 2)
	if !w.Open || w.CurStop != 12 || w.ExitStop != 0 || w.Exit != 14 || w.ExitIdx != 4 {
		t.Errorf("open walk = %+v, want Open, CurStop 12, Exit = last close 14", w)
	}
	w = WalkTrail(cs, 1, "long", 10.5, 2)
	if w.Open || w.ExitIdx != 5 || w.Exit != 10.9 || w.ExitStop != 12 || w.CurStop != 0 {
		t.Errorf("gap exit = %+v, want bar 5 at the open 10.9 with ExitStop 12", w)
	}
	// Short, nothing after the signal bar yet: the first trail already reads
	// the signal bar, max H(0..1) = 12, tighter than the initial 13.5.
	ws := WalkTrail(cs[:2], 1, "short", 13.5, 2)
	if !ws.Open || ws.CurStop != 12 || ws.Exit != 11.5 {
		t.Errorf("short open walk = %+v, want CurStop min(13.5, 12) = 12, Exit = close 11.5", ws)
	}
}
