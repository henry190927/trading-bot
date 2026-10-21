package main

import (
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
