package bingx

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"myFirstGo/trading-bot/market"
)

// OrderResult is the (trimmed) response from a successful place-order call.
type OrderResult struct {
	OrderID  string  `json:"orderId"`
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Type     string  `json:"type"`
	Price    float64 `json:"price,string"`
	Quantity float64 `json:"quantity,string"`
	Status   string  `json:"status"`
}

// rawOrderResp mirrors BingX's place-order response shape:
//
//	{ "order": { ... } }
type rawOrderResp struct {
	Order OrderResult `json:"order"`
}

// PlaceReduceOnlyLimit submits a reduce-only LIMIT order that closes
// (or partially closes) an existing position.
//
//   - posSide: "long" or "short" — the side of the EXISTING POSITION we
//     want to reduce. The order's `side` field is the opposite (closing
//     a long means SELL; closing a short means BUY).
//   - qty: position size in base contracts (e.g. silver oz). Caller is
//     responsible for rounding to the symbol's lotSize.
//   - price: limit price.
//
// reduceOnly=true is hard-coded; this function CANNOT open a new position
// even if mis-called. That invariant is the entire point.
//
// For hedge-mode accounts, positionSide must be set to "LONG"/"SHORT" so
// BingX knows which leg to reduce. For one-way accounts, positionSide
// must be empty (or "BOTH"). We send both and let BingX accept one — but
// in practice BingX errors on the mismatch, so we instead require the
// caller to pass hedgeMode and route accordingly.
func (c *Client) PlaceReduceOnlyLimit(ctx context.Context, sym market.Symbol, posSide string, qty, price float64, hedgeMode bool) (*OrderResult, error) {
	posSide = strings.ToLower(posSide)
	if posSide != "long" && posSide != "short" {
		return nil, fmt.Errorf("posSide must be long|short, got %q", posSide)
	}
	if qty <= 0 {
		return nil, fmt.Errorf("qty must be > 0, got %v", qty)
	}
	if price <= 0 {
		return nil, fmt.Errorf("price must be > 0, got %v", price)
	}

	// Order side is the OPPOSITE of position side (we're closing).
	orderSide := "SELL"
	if posSide == "short" {
		orderSide = "BUY"
	}

	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", orderSide)
	q.Set("type", "LIMIT")
	q.Set("price", strconv.FormatFloat(price, 'f', -1, 64))
	q.Set("quantity", strconv.FormatFloat(qty, 'f', -1, 64))
	q.Set("reduceOnly", "true")
	q.Set("timeInForce", "GTC")
	if hedgeMode {
		// Hedge mode: positionSide must be the LONG/SHORT we're reducing.
		if posSide == "long" {
			q.Set("positionSide", "LONG")
		} else {
			q.Set("positionSide", "SHORT")
		}
	}

	var resp rawOrderResp
	if err := c.signedRequest(ctx, "POST", PathOrder, q, &resp); err != nil {
		return nil, err
	}
	return &resp.Order, nil
}
