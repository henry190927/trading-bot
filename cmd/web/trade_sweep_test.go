package main

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/journal"
)

func TestNeedsSweep(t *testing.T) {
	filled := time.Date(2026, 10, 21, 13, 34, 0, 0, time.UTC)
	cases := []struct {
		name string
		tr   journal.Trade
		want bool
	}{
		{"pending entry", journal.Trade{ID: 1, Entry: 80}, true},
		{"filled, tp1 still to place", journal.Trade{ID: 1, Entry: 80, FilledAt: filled, TP1Auto: true}, true},
		{"filled, everything placed", journal.Trade{ID: 1, Entry: 80, FilledAt: filled, TP1Auto: true, TP1OrderID: "9", StopAuto: true, StopOrderID: "8"}, false},
		{"filled, nothing opted in", journal.Trade{ID: 1, Entry: 80, FilledAt: filled}, false},
		{"closed", journal.Trade{ID: 1, Entry: 80, ClosedAt: filled, TP1Auto: true}, false},
	}
	for _, c := range cases {
		if got := needsSweep([]journal.Trade{c.tr}); got != c.want {
			t.Errorf("%s: needsSweep = %v, want %v", c.name, got, c.want)
		}
	}
}

// The sweep holds a copy of the journal across BingX round trips. Whatever
// happened to the file meanwhile must survive the write-back.
func TestMergeSweepKeepsConcurrentChanges(t *testing.T) {
	filled := time.Date(2026, 10, 21, 13, 34, 0, 0, time.UTC)
	before := []journal.Trade{
		{ID: 92, Entry: 80.424, TP1Auto: true},
		{ID: 93, Entry: 1.5, TP1Auto: true, FilledAt: filled},
	}
	after := []journal.Trade{
		{ID: 92, Entry: 80.424, TP1Auto: true, FilledAt: filled, TP1OrderID: "tp1-92"},
		{ID: 93, Entry: 1.5, TP1Auto: true, FilledAt: filled, TP1OrderID: "tp1-93"},
	}
	// Meanwhile: #92's notes were edited, #93 got a TP1 id by hand, #94 was
	// recorded.
	fresh := []journal.Trade{
		{ID: 92, Entry: 80.424, TP1Auto: true, OpenNotes: "edited"},
		{ID: 93, Entry: 1.5, TP1Auto: true, FilledAt: filled, TP1OrderID: "manual"},
		{ID: 94, Entry: 2},
	}
	if !mergeSweep(fresh, before, after) {
		t.Fatal("mergeSweep reported no change")
	}
	if len(fresh) != 3 || fresh[2].ID != 94 {
		t.Fatalf("new row lost: %+v", fresh)
	}
	if f := fresh[0]; f.OpenNotes != "edited" || f.TP1OrderID != "tp1-92" || !f.FilledAt.Equal(filled) {
		t.Errorf("#92 = %+v, want the edit kept AND the sweep's fill + tp1 id", f)
	}
	if fresh[1].TP1OrderID != "manual" {
		t.Errorf("#93 tp1 = %q, the hand-set id must win", fresh[1].TP1OrderID)
	}
}

func TestMergeSweepNoChangeNoWrite(t *testing.T) {
	rows := []journal.Trade{{ID: 1, Entry: 80}}
	if mergeSweep([]journal.Trade{{ID: 1, Entry: 80}}, rows, rows) {
		t.Error("identical before/after reported a change")
	}
}

func TestRTrack(t *testing.T) {
	long := journal.Trade{Side: "long", Entry: 80, Stop: 79, TP1: 82, TP2: 83}
	// Scale -1R..+3R: x(r) = (r+1)/4*100.
	k := newRTrack(long, 80.5)
	if k == nil || k.Entry != 25 || k.TP1 != 75 || k.TP2 != 100 || k.Mark != 37.5 || k.R != 0.5 {
		t.Fatalf("long track = %+v", k)
	}
	if k.FillL != 25 || k.FillW != 12.5 {
		t.Errorf("fill = %v,%v, want 25,12.5", k.FillL, k.FillW)
	}
	// Short mirrors; drawdown fills leftward from entry; beyond the stop pins.
	short := journal.Trade{Side: "short", Entry: 100, Stop: 102, TP1: 96}
	// -1R..+2R: x(-0.5) = 0.5/3*100 = 16.67, entry = 33.33.
	if k := newRTrack(short, 101); k.R != -0.5 || math.Abs(k.Mark-100.0/6) > 1e-9 || k.FillL != k.Mark || math.Abs(k.Entry-100.0/3) > 1e-9 {
		t.Errorf("short track = %+v", k)
	}
	if k := newRTrack(short, 110); k.Mark != 0 {
		t.Errorf("mark beyond stop = %v, want pinned to 0", k.Mark)
	}
	// No target: right end is +1R.
	if k := newRTrack(journal.Trade{Side: "long", Entry: 10, Stop: 9}, 10); k.Entry != 50 || k.HasTP1 {
		t.Errorf("no-target track = %+v", k)
	}
	for _, bad := range []journal.Trade{
		{Side: "long", Entry: 80, Stop: 0},
		{Side: "long", Entry: 80, Stop: 81},
	} {
		if newRTrack(bad, 80) != nil {
			t.Errorf("R undefined but got a track for %+v", bad)
		}
	}
}
