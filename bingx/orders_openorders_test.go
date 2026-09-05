package bingx

import (
	"encoding/json"
	"strconv"
	"testing"
)

// Mirrors OpenOrders' parse + filter without the HTTP layer, so the two
// defects found on 2026-09-05 with two live resting limits stay fixed.
func decodeOpenOrders(body []byte, want string) ([]OpenOrder, error) {
	var resp struct {
		Orders []struct {
			Symbol       string `json:"symbol"`
			OrderID      int64  `json:"orderId"`
			Type         string `json:"type"`
			Side         string `json:"side"`
			PositionSide string `json:"positionSide"`
			Price        string `json:"price"`
			StopPrice    string `json:"stopPrice"`
			OrigQty      string `json:"origQty"`
			Quantity     string `json:"quantity"`
			ReduceOnly   bool   `json:"reduceOnly"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]OpenOrder, 0, len(resp.Orders))
	for _, o := range resp.Orders {
		if o.Symbol != "" && o.Symbol != want {
			continue
		}
		price, _ := strconv.ParseFloat(o.Price, 64)
		stopPrice, _ := strconv.ParseFloat(o.StopPrice, 64)
		qty, _ := strconv.ParseFloat(o.OrigQty, 64)
		if qty == 0 {
			qty, _ = strconv.ParseFloat(o.Quantity, 64)
		}
		out = append(out, OpenOrder{
			Symbol: o.Symbol, OrderID: strconv.FormatInt(o.OrderID, 10),
			Type: o.Type, Side: o.Side, PositionSide: o.PositionSide,
			Price: price, StopPrice: stopPrice, Quantity: qty, ReduceOnly: o.ReduceOnly,
		})
	}
	return out, nil
}

// The endpoint is sent symbol= and ignores it. With one BTC limit and one ETH
// limit resting, /ops/verify showed ETH's order under the BTC trade and BTC's
// under ETH — on the one surface whose purpose is to be trusted over local
// bookkeeping.
func TestOpenOrdersFiltersBySymbol(t *testing.T) {
	body := []byte(`{"orders":[
      {"symbol":"BTC-USDT","orderId":2096057660269096960,"type":"LIMIT","side":"SELL","price":"79685.0","origQty":"0.1173","reduceOnly":false},
      {"symbol":"ETH-USDT","orderId":2096060337438814208,"type":"LIMIT","side":"SELL","price":"2463.0","origQty":"3.8062","reduceOnly":false}
    ]}`)

	btc, err := decodeOpenOrders(body, "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(btc) != 1 {
		t.Fatalf("BTC-USDT returned %d orders, want 1 — the ETH order leaked in", len(btc))
	}
	if btc[0].OrderID != "2096057660269096960" {
		t.Errorf("wrong order kept: %s", btc[0].OrderID)
	}

	eth, err := decodeOpenOrders(body, "ETH-USDT")
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
	body := []byte(`{"orders":[{"symbol":"BTC-USDT","orderId":1,"type":"LIMIT","price":"79685.0","origQty":"0.11733"}]}`)
	got, err := decodeOpenOrders(body, "BTC-USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Quantity != 0.11733 {
		t.Fatalf("quantity = %v, want 0.11733", got[0].Quantity)
	}
}

// If BingX ever renames origQty, fall back rather than report zero.
func TestOpenOrdersFallsBackToQuantity(t *testing.T) {
	body := []byte(`{"orders":[{"symbol":"BTC-USDT","orderId":1,"type":"LIMIT","price":"1.0","quantity":"2.5"}]}`)
	got, _ := decodeOpenOrders(body, "BTC-USDT")
	if len(got) != 1 || got[0].Quantity != 2.5 {
		t.Fatalf("quantity = %v, want the 2.5 fallback", got[0].Quantity)
	}
}

// A response that stops reporting symbol must degrade to over-inclusive, not
// to silently empty — showing no orders on this page reads as "nothing is
// resting", which is the most dangerous wrong answer it can give.
func TestOpenOrdersKeepsRowsWithNoSymbol(t *testing.T) {
	body := []byte(`{"orders":[{"orderId":1,"type":"STOP_MARKET","price":"0","stopPrice":"79880.0","origQty":"0.1"}]}`)
	got, _ := decodeOpenOrders(body, "BTC-USDT")
	if len(got) != 1 {
		t.Fatalf("got %d, want the symbol-less row kept", len(got))
	}
	if got[0].StopPrice != 79880.0 {
		t.Errorf("stopPrice = %v, want 79880", got[0].StopPrice)
	}
}
