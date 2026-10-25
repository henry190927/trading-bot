package main

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
)

var tpe = time.FixedZone("Asia/Taipei", 8*3600)

func at(h, m, s int) time.Time { return time.Date(2026, 10, 25, h, m, s, 0, tpe) }

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// #93, from /ops/fills on 2026-10-25. Hand-traced:
//
//	closing fills: the one SELL 1628 @ 0.9249308968058968 → exit = that price
//	fees: -0.299845 (entry) + -0.752894 (close) = -1.052739
//	outcome: no order type; exit is 2.5% from TP2 and 1.7% from the stop
//	(both > 0.2%), TP1 is 0 → "manual"
//	R: (0.9249308968 - 0.920912) / (0.920912 - 0.909) = 0.0040189 / 0.011912 = 0.33738
func TestExchangeCloseFor93(t *testing.T) {
	tr := journal.Trade{
		ID: 93, Symbol: "SUI", Side: "long", Entry: 0.920912, Stop: 0.909, TP2: 0.948,
		OpenedAt: at(0, 59, 48), EntryOrderID: "2114039166723649536",
	}
	fills := []bingx.FilledOrder{
		{OrderID: "2114039166723649536", Side: "BUY", PositionSide: "LONG", AvgPrice: 0.9209, Quantity: 1628, FeeUSDT: -0.299845, Time: at(1, 37, 23)},
		{OrderID: "2114230036026585088", Side: "SELL", PositionSide: "LONG", AvgPrice: 0.9249308968058968, Quantity: 1628, ProfitUSDT: 6.5623, FeeUSDT: -0.752894, Time: at(13, 38, 16)},
	}
	ec, ok := exchangeCloseFor(tr, fills)
	if !ok {
		t.Fatal("want a close")
	}
	if ec.Fills != 1 || ec.Qty != 1628 || ec.EntryQty != 1628 || ec.ExitPrice != 0.9249308968058968 ||
		!ec.ClosedAt.Equal(at(13, 38, 16)) || !approx(ec.PnL, 6.5623, 1e-9) || !approx(ec.Fees, -1.052739, 1e-9) {
		t.Fatalf("got %+v", ec)
	}
	if ec.Outcome != "manual" {
		t.Errorf("outcome %q, want manual (a moved stop is nowhere near the journal stop)", ec.Outcome)
	}
	if r := journal.RealizedR(tr, ec.ExitPrice); !approx(r, 0.3374, 0.0005) {
		t.Errorf("R %.5f, want ~0.3374", r)
	}
}

// A TP1 leg already recorded is left out of the exit, but its quantity is
// kept apart so the "closing qty short of entry" warning still adds up.
//
//	remainder: SELL 5 @ 129.9 → exit 129.9; |129.9-130|/100 = 0.001 <= 0.002 → tp2
//	Qty 5 + LegQty 5 = EntryQty 10
func TestExchangeCloseForSkipsRecordedTP1Leg(t *testing.T) {
	tr := journal.Trade{
		Side: "long", Entry: 100, Stop: 90, TP1: 110, TP2: 130, OpenedAt: at(1, 0, 0),
		EntryOrderID: "e", TP1OrderID: "t1", TP1FillPrice: 110, TP1FillPct: 50,
	}
	fills := []bingx.FilledOrder{
		{OrderID: "e", Side: "BUY", AvgPrice: 100, Quantity: 10, Time: at(1, 5, 0)},
		{OrderID: "t1", Side: "SELL", AvgPrice: 110, Quantity: 5, Time: at(2, 0, 0)},
		{OrderID: "x", Side: "SELL", AvgPrice: 129.9, Quantity: 5, Time: at(3, 0, 0)},
	}
	ec, ok := exchangeCloseFor(tr, fills)
	if !ok || ec.Qty != 5 || ec.LegQty != 5 || ec.EntryQty != 10 || ec.ExitPrice != 129.9 || ec.Outcome != "tp2" {
		t.Fatalf("got %+v ok=%v", ec, ok)
	}
}

// Short side: BUY closes. The LONG leg of a hedge and a fill from before the
// trade opened are both ignored. The order type, when reported, decides the
// outcome: |105.3-105|/100 = 0.003 is outside the 0.2% guess tolerance, so
// only STOP_MARKET makes it "stop".
func TestExchangeCloseForShortUsesTypeAndIgnoresOtherLeg(t *testing.T) {
	tr := journal.Trade{Side: "short", Entry: 100, Stop: 105, TP2: 90, OpenedAt: at(1, 0, 0)}
	fills := []bingx.FilledOrder{
		{OrderID: "old", Side: "BUY", PositionSide: "SHORT", AvgPrice: 90, Quantity: 3, Time: at(0, 30, 0)},
		{OrderID: "e", Side: "SELL", PositionSide: "SHORT", AvgPrice: 100, Quantity: 2, Time: at(1, 1, 0)},
		{OrderID: "hedge", Side: "BUY", PositionSide: "LONG", AvgPrice: 101, Quantity: 1, Time: at(1, 30, 0)},
		{OrderID: "s", Side: "BUY", PositionSide: "SHORT", Type: "STOP_MARKET", AvgPrice: 105.3, Quantity: 2, Time: at(2, 0, 0)},
	}
	ec, ok := exchangeCloseFor(tr, fills)
	if !ok || ec.Qty != 2 || ec.EntryQty != 2 || ec.ExitPrice != 105.3 || ec.Outcome != "stop" || ec.Fills != 1 {
		t.Fatalf("got %+v ok=%v", ec, ok)
	}
}

// Two closing fills are size-weighted: (1*10 + 3*14) / 4 = 52/4 = 13.
func TestExchangeCloseForWeightsFills(t *testing.T) {
	tr := journal.Trade{Side: "long", Entry: 12, Stop: 11, OpenedAt: at(1, 0, 0)}
	fills := []bingx.FilledOrder{
		{OrderID: "a", Side: "SELL", AvgPrice: 10, Quantity: 1, Time: at(3, 0, 0)},
		{OrderID: "b", Side: "SELL", AvgPrice: 14, Quantity: 3, Time: at(2, 0, 0)},
	}
	ec, ok := exchangeCloseFor(tr, fills)
	if !ok || ec.ExitPrice != 13 || !ec.ClosedAt.Equal(at(3, 0, 0)) || ec.Fills != 2 {
		t.Fatalf("got %+v ok=%v", ec, ok)
	}
}

func TestExchangeCloseForNoClosingFill(t *testing.T) {
	tr := journal.Trade{Side: "long", Entry: 12, Stop: 11, OpenedAt: at(1, 0, 0)}
	fills := []bingx.FilledOrder{{OrderID: "e", Side: "BUY", AvgPrice: 12, Quantity: 1, Time: at(1, 5, 0)}}
	if ec, ok := exchangeCloseFor(tr, fills); ok {
		t.Fatalf("an open position has no close, got %+v", ec)
	}
}
