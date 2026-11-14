package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
)

// -warmup bars must inform the read but never trade: a fire on a bar that
// opened before the window (its close is at or before start) is dropped, the
// first in-window bar's fire is kept.
func TestInWindowDropsWarmupFires(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fires := []autotrade.PaperFire{
		{Time: start.Add(-time.Hour)},    // warm-up bar
		{Time: start},                    // bar that opened at start-1h
		{Time: start.Add(time.Hour)},     // first in-window bar
		{Time: start.Add(5 * time.Hour)}, // later in-window bar
	}
	got := inWindow(fires, start)
	if len(got) != 2 || !got[0].Time.Equal(start.Add(time.Hour)) {
		t.Fatalf("inWindow kept %v, want the two in-window fires", got)
	}
	if fires[0].Time != start.Add(-time.Hour) {
		t.Error("inWindow mutated its input")
	}
	if d := tfDur("1h"); d != time.Hour {
		t.Errorf("tfDur(1h) = %v", d)
	}
	if d := tfDur("1d"); d != 24*time.Hour {
		t.Errorf("tfDur(1d) = %v", d)
	}
}
