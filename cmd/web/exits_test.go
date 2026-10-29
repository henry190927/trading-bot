package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
)

func th(h int) time.Time { return time.Date(2026, 10, 30, h, 0, 0, 0, time.UTC) }

func longTrade() journal.Trade {
	return journal.Trade{ID: 7, Symbol: "SUI", Side: "long", Entry: 100, Stop: 90, OpenedAt: th(0), FilledAt: th(1)}
}

// Entry 100 units; two partial closes while the position is still open.
// 30/100 = 30%, 20/100 = 20%. The opening BUY is not a close.
func TestNewExitsTwoPartials(t *testing.T) {
	fills := []bingx.FilledOrder{
		{OrderID: "e", Side: "BUY", AvgPrice: 100, Quantity: 100, Time: th(1)},
		{OrderID: "a", Side: "SELL", AvgPrice: 110, Quantity: 30, Time: th(2)},
		{OrderID: "b", Side: "SELL", AvgPrice: 120, Quantity: 20, Time: th(3)},
	}
	want := []journal.Exit{
		{Price: 110, Qty: 30, Pct: 30, At: th(2), OrderID: "a", Src: "exchange"},
		{Price: 120, Qty: 20, Pct: 20, At: th(3), OrderID: "b", Src: "exchange"},
	}
	if got := newExitsFrom(longTrade(), fills, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// 30 then 70: the 70 takes the running total to 100 >= 99.9 — the final
// close, which the close form handles. Only the 30 is a partial.
func TestNewExitsStopsAtTheFinalClose(t *testing.T) {
	fills := []bingx.FilledOrder{
		{OrderID: "a", Side: "SELL", AvgPrice: 110, Quantity: 30, Time: th(2)},
		{OrderID: "c", Side: "SELL", AvgPrice: 95, Quantity: 70, Time: th(3)},
	}
	got := newExitsFrom(longTrade(), fills, 100)
	if len(got) != 1 || got[0].OrderID != "a" {
		t.Fatalf("got %+v, want only a", got)
	}
}

// "a" (30%) is already recorded, so the running total starts at 30.
// Fills arrive out of order; they are taken in time order:
// a (skip, recorded) → b at 50 → c at 70, both below 99.9.
func TestNewExitsSkipsRecordedAndSortsByTime(t *testing.T) {
	tr := longTrade()
	tr.Exits = []journal.Exit{{Price: 110, Qty: 30, Pct: 30, OrderID: "a"}}
	fills := []bingx.FilledOrder{
		{OrderID: "c", Side: "SELL", AvgPrice: 125, Quantity: 20, Time: th(4)},
		{OrderID: "a", Side: "SELL", AvgPrice: 110, Quantity: 30, Time: th(2)},
		{OrderID: "b", Side: "SELL", AvgPrice: 120, Quantity: 20, Time: th(3)},
	}
	orig := append([]bingx.FilledOrder(nil), fills...)
	got := newExitsFrom(tr, fills, 100)
	if len(got) != 2 || got[0].OrderID != "b" || got[1].OrderID != "c" || got[0].Pct != 20 {
		t.Fatalf("got %+v, want b then c", got)
	}
	if !reflect.DeepEqual(fills, orig) {
		t.Errorf("fills were modified: %+v", fills)
	}
}

// A short closes with BUY. Each of the first four fills is excluded for its
// own reason; only the last one is a partial close (10/100 = 10%).
func TestNewExitsFiltersForAShort(t *testing.T) {
	tr := journal.Trade{ID: 8, Symbol: "SUI", Side: "short", Entry: 100, Stop: 110, OpenedAt: th(0), FilledAt: th(1)}
	fills := []bingx.FilledOrder{
		{OrderID: "hedge", Side: "BUY", PositionSide: "LONG", AvgPrice: 90, Quantity: 10, Time: th(2)},
		{OrderID: "early", Side: "BUY", PositionSide: "SHORT", AvgPrice: 91, Quantity: 10, Time: th(0)},
		{OrderID: "moving", Side: "BUY", PositionSide: "SHORT", AvgPrice: 92, Quantity: 10, Status: "PARTIALLY_FILLED", Time: th(2)},
		{OrderID: "open", Side: "SELL", PositionSide: "SHORT", AvgPrice: 100, Quantity: 10, Time: th(2)},
		{OrderID: "ok", Side: "BUY", PositionSide: "SHORT", AvgPrice: 93, Quantity: 10, Status: "FILLED", Time: th(3)},
	}
	got := newExitsFrom(tr, fills, 100)
	if len(got) != 1 || got[0].OrderID != "ok" || got[0].Pct != 10 || got[0].Price != 93 {
		t.Fatalf("got %+v, want only ok at 10%%", got)
	}
}

// The legacy TP1 leg (50%, order t1) counts as recorded and starts the
// total at 50: x takes it to 80 (a partial), y to 100 (the final close).
func TestNewExitsAfterALegacyTP1Leg(t *testing.T) {
	tr := longTrade()
	tr.TP1OrderID, tr.TP1FillPrice, tr.TP1FillPct = "t1", 110, 50
	fills := []bingx.FilledOrder{
		{OrderID: "t1", Side: "SELL", AvgPrice: 110, Quantity: 50, Time: th(2)},
		{OrderID: "x", Side: "SELL", AvgPrice: 115, Quantity: 30, Time: th(3)},
		{OrderID: "y", Side: "SELL", AvgPrice: 100, Quantity: 20, Time: th(4)},
	}
	got := newExitsFrom(tr, fills, 100)
	if len(got) != 1 || got[0].OrderID != "x" || got[0].Pct != 30 {
		t.Fatalf("got %+v, want only x at 30%%", got)
	}
}

func TestNewExitsWithoutADenominator(t *testing.T) {
	fills := []bingx.FilledOrder{{OrderID: "a", Side: "SELL", AvgPrice: 110, Quantity: 30, Time: th(2)}}
	if got := newExitsFrom(longTrade(), fills, 0); got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestMergeExitsSkipsKnownAndClosed(t *testing.T) {
	open := longTrade()
	open.Exits = []journal.Exit{{OrderID: "a", Pct: 30, Price: 110}}
	closed := longTrade()
	closed.ID, closed.ClosedAt = 9, th(5)
	fresh := []journal.Trade{open, closed}
	added := map[int][]journal.Exit{
		7: {{OrderID: "a", Pct: 30, Price: 110}, {OrderID: "b", Pct: 20, Price: 120}},
		9: {{OrderID: "z", Pct: 10, Price: 1}},
	}
	if !mergeExits(fresh, added) {
		t.Fatal("want a change")
	}
	if len(fresh[0].Exits) != 2 || fresh[0].Exits[1].OrderID != "b" || len(fresh[1].Exits) != 0 {
		t.Fatalf("fresh = %+v", fresh)
	}
}

// The close-from-exchange prefill leaves a recorded exit out of the exit
// price and counts it as leg quantity: 3 recorded + 7 closing = 10 = entry.
func TestExchangeCloseSkipsRecordedExits(t *testing.T) {
	tr := journal.Trade{Side: "long", Entry: 100, Stop: 90, OpenedAt: th(0), EntryOrderID: "e",
		Exits: []journal.Exit{{OrderID: "p", Pct: 30, Price: 110}}}
	fills := []bingx.FilledOrder{
		{OrderID: "e", Side: "BUY", AvgPrice: 100, Quantity: 10, Time: th(1)},
		{OrderID: "p", Side: "SELL", AvgPrice: 110, Quantity: 3, Time: th(2)},
		{OrderID: "x", Side: "SELL", AvgPrice: 95, Quantity: 7, Time: th(3)},
	}
	ec, ok := exchangeCloseFor(tr, fills)
	if !ok || ec.Qty != 7 || ec.LegQty != 3 || ec.EntryQty != 10 || ec.ExitPrice != 95 {
		t.Fatalf("got %+v ok=%v", ec, ok)
	}
}
