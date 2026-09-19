package bingx

import (
	"encoding/json"
	"math"
	"testing"
)

// The payload below is verbatim from BingX on 2026-09-19 (fields trimmed to
// the ones parseHistory reads). It is the shape /trade/fillHistory has always
// returned and that parseHistory rejected as "unrecognised" until this test
// existed — BTC, XAU, XAG and SUI all reported "no fill history" while their
// trades sat inside a discarded payload.
//
// It is also trade-level: the 0.0615 BTC entry of 2026-09-17 arrives as three
// rows under one orderId.
const fillHistoryReal = `{"fill_history_orders":[
 {"symbol":"BTC-USDT","qty":"0.0615","quoteQty":"4701.3921","role":"taker","commission":"-2.350696","price":"76445.4","orderId":"2100656013975973888","filledTime":"2026-09-18T02:39:57.000+08:00","realisedPNL":"2.79210000","side":"SELL","positionSide":"LONG"},
 {"symbol":"BTC-USDT","qty":"0.0100","quoteQty":"764.0000","role":"maker","commission":"-0.152800","price":"76400.0","orderId":"2100473513072881664","filledTime":"2026-09-17T15:57:43.000+08:00","realisedPNL":"0.00000000","side":"BUY","positionSide":"LONG"},
 {"symbol":"BTC-USDT","qty":"0.0452","quoteQty":"3453.2800","role":"maker","commission":"-0.690656","price":"76400.0","orderId":"2100473513072881664","filledTime":"2026-09-17T15:57:31.000+08:00","realisedPNL":"0.00000000","side":"BUY","positionSide":"LONG"},
 {"symbol":"BTC-USDT","qty":"0.0063","quoteQty":"481.3200","role":"maker","commission":"-0.096264","price":"76400.0","orderId":"2100473513072881664","filledTime":"2026-09-17T15:57:31.000+08:00","realisedPNL":"0.00000000","side":"BUY","positionSide":"LONG"}
],"total":4}`

func TestParseHistoryReadsFillHistoryShape(t *testing.T) {
	got, err := parseHistory(json.RawMessage(fillHistoryReal), "/openApi/swap/v2/trade/fillHistory")
	if err != nil {
		t.Fatalf("real fillHistory payload rejected: %v", err)
	}
	// Three rows share an orderId, so four rows are two orders — not four.
	if len(got) != 2 {
		t.Fatalf("got %d orders, want 2 (the entry's 3 partial fills fold into one)", len(got))
	}

	var entry, exit FilledOrder
	for _, f := range got {
		switch f.OrderID {
		case "2100473513072881664":
			entry = f
		case "2100656013975973888":
			exit = f
		}
	}
	if entry.OrderID == "" || exit.OrderID == "" {
		t.Fatalf("missing an order: %+v", got)
	}

	// 0.0100 + 0.0452 + 0.0063. Counting the partials separately would put
	// three trades in the journal and triple-count the position.
	if math.Abs(entry.Quantity-0.0615) > 1e-9 {
		t.Errorf("entry qty = %v, want 0.0615", entry.Quantity)
	}
	if math.Abs(entry.AvgPrice-76400.0) > 1e-6 {
		t.Errorf("entry avgPrice = %v, want 76400", entry.AvgPrice)
	}
	// -0.152800 + -0.690656 + -0.096264. This is the number /ops/fills already
	// reported via the allOrders path, so the two endpoints must agree.
	if math.Abs(entry.FeeUSDT-(-0.93972)) > 1e-9 {
		t.Errorf("entry fee = %v, want -0.93972 (sum of the three partials)", entry.FeeUSDT)
	}
	// Time is the LAST execution — when the order finished, not when it began.
	if h, m, s := entry.Time.Hour(), entry.Time.Minute(), entry.Time.Second(); s != 43 || m != 57 {
		t.Errorf("entry time = %02d:%02d:%02d, want the last fill at 15:57:43 (+08:00)", h, m, s)
	}
	if math.Abs(exit.ProfitUSDT-2.7921) > 1e-9 {
		t.Errorf("exit realisedPNL = %v, want 2.7921", exit.ProfitUSDT)
	}
	if exit.Side != "SELL" || exit.PositionSide != "LONG" {
		t.Errorf("exit side/positionSide = %s/%s, want SELL/LONG", exit.Side, exit.PositionSide)
	}
}

// The real payload fills all three partials at the same price, so it cannot
// tell a volume-weighted average from a plain one. This can: an unweighted
// mean of 100 and 200 is 150, the weighted one is 175.
func TestParseHistoryWeightsAverageByQuoteQty(t *testing.T) {
	raw := `{"fill_history_orders":[
	 {"symbol":"X-USDT","qty":"1","quoteQty":"100","price":"100","commission":"-0.1","orderId":"901","filledTime":"2026-09-19T10:00:00.000+08:00","side":"BUY","positionSide":"LONG"},
	 {"symbol":"X-USDT","qty":"3","quoteQty":"600","price":"200","commission":"-0.3","orderId":"901","filledTime":"2026-09-19T10:00:01.000+08:00","side":"BUY","positionSide":"LONG"}
	]}`
	got, err := parseHistory(json.RawMessage(raw), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d orders, want 1", len(got))
	}
	if math.Abs(got[0].Quantity-4) > 1e-9 {
		t.Errorf("qty = %v, want 4", got[0].Quantity)
	}
	if math.Abs(got[0].AvgPrice-175) > 1e-9 {
		t.Errorf("avgPrice = %v, want 175 (700/4) — 150 means the fills were averaged unweighted", got[0].AvgPrice)
	}
	if math.Abs(got[0].FeeUSDT-(-0.4)) > 1e-9 {
		t.Errorf("fee = %v, want -0.4", got[0].FeeUSDT)
	}
}

// Folding runs on every shape, so the order-level ones must come through
// unchanged — one row in, one row out, price untouched.
func TestParseHistoryLeavesOrderLevelShapesAlone(t *testing.T) {
	for _, key := range []string{"orders", "fill_orders"} {
		raw := `{"` + key + `":[
		 {"orderId":"1","symbol":"ETH-USDT","side":"BUY","positionSide":"LONG","type":"MARKET","avgPrice":"2569.45","executedQty":"3.66","commission":"-4.702094","profit":"0","status":"FILLED","updateTime":1789741250000},
		 {"orderId":"2","symbol":"ETH-USDT","side":"SELL","positionSide":"LONG","type":"MARKET","avgPrice":"2588.83","executedQty":"3.66","commission":"-4.737559","profit":"70.9308","status":"FILLED","updateTime":1789747714000}
		]}`
		got, err := parseHistory(json.RawMessage(raw), key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if len(got) != 2 {
			t.Fatalf("%s: got %d, want 2", key, len(got))
		}
		if math.Abs(got[0].AvgPrice-2569.45) > 1e-9 || math.Abs(got[1].ProfitUSDT-70.9308) > 1e-9 {
			t.Errorf("%s: order-level row altered by folding: %+v", key, got)
		}
	}
}

// Rows with no orderId cannot be grouped — merging two unrelated executions
// under the empty key would invent a trade that never happened.
//
// Driven through foldByOrder rather than parseHistory on purpose: the row
// struct types orderId as json.Number, so a blank one fails to unmarshal and
// the row is dropped before folding ever sees it. Every payload observed from
// BingX carries a numeric orderId, so that drop is not a live concern — but
// the grouping itself must still be safe for any caller that reaches it.
func TestFoldByOrderDoesNotGroupBlankOrderIDs(t *testing.T) {
	in := []FilledOrder{
		{OrderID: "", Symbol: "A-USDT", Quantity: 1, AvgPrice: 10},
		{OrderID: "", Symbol: "A-USDT", Quantity: 2, AvgPrice: 20},
	}
	got := foldByOrder(in, []float64{10, 40})
	if len(got) != 2 {
		t.Errorf("blank orderIds merged into %d row(s), want 2 kept separate", len(got))
	}
}
