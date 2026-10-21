package main

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
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
		{"filled, placed, tp1 fill not recorded", journal.Trade{ID: 1, Entry: 80, FilledAt: filled, TP1Auto: true, TP1OrderID: "9", StopAuto: true, StopOrderID: "8"}, true},
		{"filled, placed, tp1 leg recorded", journal.Trade{ID: 1, Entry: 80, FilledAt: filled, TP1Auto: true, TP1OrderID: "9", StopAuto: true, StopOrderID: "8", TP1FillPrice: 82, TP1FillPct: 50}, false},
		{"bundled tp only", journal.Trade{ID: 1, Entry: 80, FilledAt: filled, TP2OrderID: "ATTACHED:7", TP1OrderID: "ATTACHED:7"}, false},
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

// #92's real numbers: margin 15 x 100 / 80.424 floored to the lot = 18.6511,
// TP1 closed 9.3255 @ 82. Hand-traced pct = 9.3255/18.6511 = 49.9997%.
func TestTP1LegFrom(t *testing.T) {
	at := time.Date(2026, 10, 21, 16, 2, 0, 0, time.UTC)
	tr := journal.Trade{Side: "long", Entry: 80.424, Stop: 79.2, MarginUSDT: 15, Leverage: 100,
		EntryOrderID: "E", TP1OrderID: "T"}
	tp1 := bingx.FilledOrder{OrderID: "T", AvgPrice: 82, Quantity: 9.3255, Status: "FILLED", Time: at}
	entry := bingx.FilledOrder{OrderID: "E", AvgPrice: 80.424, Quantity: 18.6511, Status: "FILLED"}
	other := bingx.FilledOrder{OrderID: "X", AvgPrice: 1, Quantity: 1, Status: "FILLED"}

	price, pct, when, ok := tp1LegFrom(tr, []bingx.FilledOrder{other, entry, tp1}, 4)
	if !ok || price != 82 || math.Abs(pct-9.3255/18.6511*100) > 1e-9 || !when.Equal(at) {
		t.Fatalf("got %v %v %v %v", price, pct, when, ok)
	}
	// Entry order outside the history: falls back to the planned size,
	// floor(1500/80.424, 4) = 18.6511 — the same denominator.
	if _, pct2, _, ok := tp1LegFrom(tr, []bingx.FilledOrder{tp1}, 4); !ok || math.Abs(pct2-pct) > 1e-9 {
		t.Errorf("planned-size fallback pct = %v, want %v", pct2, pct)
	}
	// No denominator at all: no leg rather than a guess.
	bare := tr
	bare.MarginUSDT = 0
	if _, _, _, ok := tp1LegFrom(bare, []bingx.FilledOrder{tp1}, 4); ok {
		t.Error("recorded a leg with no entry quantity to divide by")
	}
	// Partially filled TP1 is not a leg yet.
	part := tp1
	part.Status = "PARTIALLY_FILLED"
	if _, _, _, ok := tp1LegFrom(tr, []bingx.FilledOrder{entry, part}, 4); ok {
		t.Error("recorded a partially filled TP1")
	}
	if _, _, _, ok := tp1LegFrom(tr, []bingx.FilledOrder{entry, other}, 4); ok {
		t.Error("matched an unrelated order")
	}
}

func TestMergeSweepCarriesTP1Leg(t *testing.T) {
	before := []journal.Trade{{ID: 92}}
	after := []journal.Trade{{ID: 92, TP1FillPrice: 82, TP1FillPct: 50}}
	fresh := []journal.Trade{{ID: 92, OpenNotes: "edited"}}
	if !mergeSweep(fresh, before, after) || fresh[0].TP1FillPrice != 82 || fresh[0].OpenNotes != "edited" {
		t.Errorf("fresh = %+v", fresh[0])
	}
}
