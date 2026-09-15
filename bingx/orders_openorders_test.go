package bingx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/henry190927/trading-bot/market"
)

// decodeOpenOrders drives the REAL OpenOrders against a fake exchange.
//
// It used to be a hand-written MIRROR of the parse-and-filter loop. That is
// worse than no test: on 2026-09-15 /ops/orders was added, called OpenOrders
// with an empty symbol to mean "the whole book", and got back an empty list
// because the real filter drops every row whose symbol does not match — while
// every one of these tests stayed green, because the mirror had the same bug
// and neither was the code that ran. A copy of the logic can only ever confirm
// itself.
func decodeOpenOrders(t *testing.T, body string, want market.Symbol) ([]OpenOrder, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// signedRequest unwraps {code,msg,data}; the fixtures are the data.
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":`+body+`}`)
	}))
	t.Cleanup(srv.Close)
	c := New("test-key", "test-secret")
	c.Host = srv.URL
	return c.OpenOrders(context.Background(), want)
}

// The endpoint is sent symbol= and ignores it. With one BTC limit and one ETH
// limit resting, /ops/verify showed ETH's order under the BTC trade and BTC's
// under ETH — on the one surface whose purpose is to be trusted over local
// bookkeeping.
func TestOpenOrdersFiltersBySymbol(t *testing.T) {
	body := `{"orders":[
      {"symbol":"BTC-USDT","orderId":2096057660269096960,"type":"LIMIT","side":"SELL","price":"79685.0","origQty":"0.1173","reduceOnly":false},
      {"symbol":"ETH-USDT","orderId":2096060337438814208,"type":"LIMIT","side":"SELL","price":"2463.0","origQty":"3.8062","reduceOnly":false}
    ]}`

	btc, err := decodeOpenOrders(t, body, "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(btc) != 1 {
		t.Fatalf("BTC-USDT returned %d orders, want 1 — the ETH order leaked in", len(btc))
	}
	if btc[0].OrderID != "2096057660269096960" {
		t.Errorf("wrong order kept: %s", btc[0].OrderID)
	}

	eth, err := decodeOpenOrders(t, body, "ETH-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(eth) != 1 || eth[0].Price != 2463.0 {
		t.Errorf("ETH-USDT got %d orders (first price %v), want 1 @ 2463", len(eth), eth[0].Price)
	}
}

// Quantity comes back as origQty. Reading only "quantity" made every row show
// qty 0.00000, so a real order looked like a zero-size one.
func TestOpenOrdersReadsOrigQty(t *testing.T) {
	body := `{"orders":[{"symbol":"BTC-USDT","orderId":1,"type":"LIMIT","price":"79685.0","origQty":"0.11733"}]}`
	got, err := decodeOpenOrders(t, body, "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Quantity != 0.11733 {
		t.Fatalf("quantity = %v, want 0.11733", got[0].Quantity)
	}
}

// If BingX ever renames origQty, fall back rather than report zero.
func TestOpenOrdersFallsBackToQuantity(t *testing.T) {
	body := `{"orders":[{"symbol":"BTC-USDT","orderId":1,"type":"LIMIT","price":"1.0","quantity":"2.5"}]}`
	got, _ := decodeOpenOrders(t, body, "BTC-USDT")
	if len(got) != 1 || got[0].Quantity != 2.5 {
		t.Fatalf("quantity = %v, want the 2.5 fallback", got[0].Quantity)
	}
}

// A response that stops reporting symbol must degrade to over-inclusive, not
// to silently empty — showing no orders on this page reads as "nothing is
// resting", which is the most dangerous wrong answer it can give.
func TestOpenOrdersKeepsRowsWithNoSymbol(t *testing.T) {
	body := `{"orders":[{"orderId":1,"type":"STOP_MARKET","price":"0","stopPrice":"79880.0","origQty":"0.1"}]}`
	got, _ := decodeOpenOrders(t, body, "BTC-USDT")
	if len(got) != 1 {
		t.Fatalf("got %d, want the symbol-less row kept", len(got))
	}
	if got[0].StopPrice != 79880.0 {
		t.Errorf("stopPrice = %v, want 79880", got[0].StopPrice)
	}
}

// An EMPTY symbol means "the whole book". /ops/orders needs exactly this: the
// endpoint ignores its symbol parameter anyway, so one call answers for every
// symbol, and asking per-symbol would be N signed calls for N copies of the
// same response.
//
// Shipped without it on 2026-09-15 and the page rendered an empty book while a
// real ETH limit was resting on the exchange — the filter dropped every row
// because none of them equalled "".
func TestOpenOrdersEmptySymbolReturnsEverything(t *testing.T) {
	body := `{"orders":[
      {"symbol":"BTC-USDT","orderId":1,"type":"LIMIT","side":"SELL","price":"79685.0","origQty":"0.1173"},
      {"symbol":"ETH-USDT","orderId":2,"type":"LIMIT","side":"BUY","price":"2400.0","origQty":"3.12"}
    ]}`
	got, err := decodeOpenOrders(t, body, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d orders for the empty symbol, want both — an account with "+
			"resting orders must not read as an empty book", len(got))
	}
	// A named symbol must still filter, or this fix would have traded one bug
	// for the cross-attribution bug the filter exists to prevent.
	if one, _ := decodeOpenOrders(t, body, "ETH-USDT"); len(one) != 1 || one[0].Price != 2400 {
		t.Errorf("ETH-USDT returned %d rows, want just the 2400 limit", len(one))
	}
}

// BingX nests an order's own stop and take-profit INSIDE the row rather than
// listing them as separate resting orders, and their stopPrice is a JSON
// number while the row's own stopPrice is a string. Until 2026-09-15 they were
// not parsed at all, which made a protected resting entry indistinguishable
// from a naked one: the entry appeared, its stop existed nowhere.
//
// Fixture mirrors the shape the exchange returns for such an order.
func TestOpenOrdersParsesBundledStopAndTP(t *testing.T) {
	body := `{"orders":[{"symbol":"ETH-USDT","orderId":1234567890123456789,"type":"LIMIT",
      "side":"BUY","positionSide":"LONG","price":"2400.00","origQty":"3.12","reduceOnly":false,
      "stopLoss":{"price":0,"quantity":0,"stopPrice":2384,"type":"STOP_MARKET"},
      "takeProfit":{"price":0,"quantity":0,"stopPrice":2440,"type":"TAKE_PROFIT_MARKET"}}]}`
	got, err := decodeOpenOrders(t, body, "ETH-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].BundledStop != 2384 {
		t.Errorf("BundledStop = %v, want 2384", got[0].BundledStop)
	}
	if got[0].BundledTP != 2440 {
		t.Errorf("BundledTP = %v, want 2440", got[0].BundledTP)
	}
	// The row's own stopPrice is absent here and must stay 0 — conflating the
	// two would make an entry look like a stop order.
	if got[0].StopPrice != 0 {
		t.Errorf("StopPrice = %v, want 0 (the bundled trigger is a different field)", got[0].StopPrice)
	}

	// An order with no bundled stop reads as zero, which is what /ops/orders
	// calls "naked".
	naked := `{"orders":[{"symbol":"ETH-USDT","orderId":3,"type":"LIMIT","price":"2400.0","origQty":"1"}]}`
	n, _ := decodeOpenOrders(t, naked, "ETH-USDT")
	if len(n) != 1 || n[0].BundledStop != 0 {
		t.Errorf("missing stopLoss should read 0, got %v", n)
	}
}
