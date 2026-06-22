package bingx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"myFirstGo/trading-bot/market"
)

// dryRunResult builds a deterministic-shape mock for DryRun branches so
// the caller can treat it like a real OrderResult without nil-checking.
// The orderId carries the symbol + a timestamp so log lines are easy to
// correlate with the form submission that triggered them.
func dryRunResult(sym market.Symbol, side, typ string, price, qty float64) *OrderResult {
	id := fmt.Sprintf("DRY-RUN-%s-%d", sym, time.Now().UnixNano())
	log.Printf("[DRY-RUN] would have sent %s %s %s sym=%s qty=%g price=%g (orderId=%s)",
		typ, side, "(no http)", sym, qty, price, id)
	return &OrderResult{
		OrderID: id, Symbol: string(sym), Side: side, Type: typ,
		Price: price, Quantity: qty, Status: "DRY_RUN",
	}
}

// OrderResult is the (trimmed) response from a successful place-order call.
//
// BingX returns the same orderId twice in the response — `orderId` as a
// JSON number (int64 in practice), `orderID` as a JSON string. Either
// would suffice on its own, but Go's json package applies case-insensitive
// fallback when matching tags, and once it tries to assign the number
// to a string field it errors out before getting to the string variant.
// Pin the shape via custom UnmarshalJSON so both keys are accepted and
// the string form (or number-formatted-as-string) is what we expose.
type OrderResult struct {
	OrderID  string
	Symbol   string
	Side     string
	Type     string
	Price    float64
	Quantity float64
	Status   string
}

// orderResultRaw mirrors the actual BingX response shape. Two distinct
// fields for the dual-typed orderId/orderID so neither collides via
// case-insensitive matching.
type orderResultRaw struct {
	OrderIDNum json.Number `json:"orderId"`
	OrderIDStr string      `json:"orderID"`
	Symbol     string      `json:"symbol"`
	Side       string      `json:"side"`
	Type       string      `json:"type"`
	Price      float64     `json:"price"`
	Quantity   float64     `json:"quantity"`
	Status     string      `json:"status"`
}

func (o *OrderResult) UnmarshalJSON(data []byte) error {
	var raw orderResultRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	o.OrderID = raw.OrderIDStr
	if o.OrderID == "" {
		o.OrderID = raw.OrderIDNum.String()
	}
	o.Symbol = raw.Symbol
	o.Side = raw.Side
	o.Type = raw.Type
	o.Price = raw.Price
	o.Quantity = raw.Quantity
	o.Status = raw.Status
	return nil
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

	if c.DryRun {
		return dryRunResult(sym, orderSide, "LIMIT_REDUCE_ONLY", price, qty), nil
	}

	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", orderSide)
	q.Set("type", "LIMIT")
	q.Set("price", strconv.FormatFloat(price, 'f', -1, 64))
	q.Set("quantity", strconv.FormatFloat(qty, 'f', -1, 64))
	q.Set("timeInForce", "GTC")
	if hedgeMode {
		// Hedge mode: positionSide must be the LONG/SHORT we're reducing.
		// BingX REJECTS reduceOnly=true in hedge mode (code=109400) because
		// positionSide alone tells the exchange which leg this order is
		// against — sending both is redundant and treated as invalid.
		if posSide == "long" {
			q.Set("positionSide", "LONG")
		} else {
			q.Set("positionSide", "SHORT")
		}
	} else {
		// One-way mode: positionSide=BOTH (implicit), reduceOnly is the
		// only thing keeping the order from over-closing into a new
		// counter-position.
		q.Set("reduceOnly", "true")
	}

	var resp rawOrderResp
	if err := c.signedRequest(ctx, "POST", PathOrder, q, &resp); err != nil {
		return nil, err
	}
	return &resp.Order, nil
}

// PlaceLimit submits a LIMIT order that OPENS a new position, optionally
// with a BingX-managed stop-loss and/or take-profit bundled on. Bundled
// SL/TP activate the instant the entry fills — closes the gap between
// fill and sweep-based reduce-only placement, so the position is never
// without a stop, and the runner-target TP2 is preset.
//
//   - side: "long" or "short" — direction of the position to open.
//   - qty:  base-unit position size (already floored to lot precision).
//   - price: limit entry price.
//   - stopPrice: stop trigger price. Pass 0 to skip bundled SL.
//   - tpPrice: take-profit trigger price (typically TP2, since BingX
//     bundles close 100% — partial TP1 is placed separately via sweep).
//     Pass 0 to skip bundled TP.
//   - hedgeMode: when true, positionSide=LONG/SHORT is sent (required by
//     hedge-mode accounts).
//
// reduceOnly is explicitly false for the entry. The bundled SL/TP on
// BingX side are automatically reduce-only (position-close triggers).
func (c *Client) PlaceLimit(ctx context.Context, sym market.Symbol, side string, qty, price, stopPrice, tpPrice float64, hedgeMode bool) (*OrderResult, error) {
	side = strings.ToLower(side)
	if side != "long" && side != "short" {
		return nil, fmt.Errorf("side must be long|short, got %q", side)
	}
	if qty <= 0 {
		return nil, fmt.Errorf("qty must be > 0, got %v", qty)
	}
	if price <= 0 {
		return nil, fmt.Errorf("price must be > 0, got %v", price)
	}

	orderSide := "BUY"
	if side == "short" {
		orderSide = "SELL"
	}

	if c.DryRun {
		return dryRunResult(sym, orderSide, "LIMIT_OPEN", price, qty), nil
	}

	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", orderSide)
	q.Set("type", "LIMIT")
	q.Set("price", strconv.FormatFloat(price, 'f', -1, 64))
	q.Set("quantity", strconv.FormatFloat(qty, 'f', -1, 64))
	q.Set("timeInForce", "GTC")
	if hedgeMode {
		if side == "long" {
			q.Set("positionSide", "LONG")
		} else {
			q.Set("positionSide", "SHORT")
		}
	}
	if stopPrice > 0 {
		// BingX accepts stopLoss as a JSON-encoded object. type=STOP_MARKET
		// closes the position at market when stopPrice is touched on the
		// MARK_PRICE feed (avoids wick-on-last-price stop hunts vs the
		// LAST_PRICE trigger).
		slJSON := fmt.Sprintf(`{"type":"STOP_MARKET","stopPrice":%s,"price":%s,"workingType":"MARK_PRICE"}`,
			strconv.FormatFloat(stopPrice, 'f', -1, 64),
			strconv.FormatFloat(stopPrice, 'f', -1, 64))
		q.Set("stopLoss", slJSON)
	}
	if tpPrice > 0 {
		// Bundled TP closes 100% of the position when tpPrice is touched.
		// Pair with the partial-TP1 reduce-only LIMIT (placed separately
		// via sweep) so TP1 scales out and bundled TP2 finishes the runner.
		tpJSON := fmt.Sprintf(`{"type":"TAKE_PROFIT_MARKET","stopPrice":%s,"price":%s,"workingType":"MARK_PRICE"}`,
			strconv.FormatFloat(tpPrice, 'f', -1, 64),
			strconv.FormatFloat(tpPrice, 'f', -1, 64))
		q.Set("takeProfit", tpJSON)
	}

	var resp rawOrderResp
	if err := c.signedRequest(ctx, "POST", PathOrder, q, &resp); err != nil {
		return nil, err
	}
	return &resp.Order, nil
}

// PlaceStopMarket submits a reduce-only STOP_MARKET that closes the
// position when stopPrice is touched. Sized to the full live position
// (caller computes qty from OpenPosition).
//
//   - posSide: "long" or "short" — the side of the EXISTING POSITION
//     we want to close on stop.
//   - qty: base-unit size to close (typically the full position).
//   - stopPrice: trigger price.
func (c *Client) PlaceStopMarket(ctx context.Context, sym market.Symbol, posSide string, qty, stopPrice float64, hedgeMode bool) (*OrderResult, error) {
	posSide = strings.ToLower(posSide)
	if posSide != "long" && posSide != "short" {
		return nil, fmt.Errorf("posSide must be long|short, got %q", posSide)
	}
	if qty <= 0 {
		return nil, fmt.Errorf("qty must be > 0, got %v", qty)
	}
	if stopPrice <= 0 {
		return nil, fmt.Errorf("stopPrice must be > 0, got %v", stopPrice)
	}

	orderSide := "SELL"
	if posSide == "short" {
		orderSide = "BUY"
	}

	if c.DryRun {
		return dryRunResult(sym, orderSide, "STOP_MARKET_REDUCE_ONLY", stopPrice, qty), nil
	}

	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", orderSide)
	q.Set("type", "STOP_MARKET")
	q.Set("stopPrice", strconv.FormatFloat(stopPrice, 'f', -1, 64))
	q.Set("quantity", strconv.FormatFloat(qty, 'f', -1, 64))
	q.Set("workingType", "MARK_PRICE")
	if hedgeMode {
		// See PlaceReduceOnlyLimit: BingX rejects reduceOnly in hedge mode;
		// positionSide carries the close-side semantics.
		if posSide == "long" {
			q.Set("positionSide", "LONG")
		} else {
			q.Set("positionSide", "SHORT")
		}
	} else {
		q.Set("reduceOnly", "true")
	}

	var resp rawOrderResp
	if err := c.signedRequest(ctx, "POST", PathOrder, q, &resp); err != nil {
		return nil, err
	}
	return &resp.Order, nil
}

// CancelOrder cancels a single pending order by orderId. BingX's spec
// is DELETE on /trade/order with symbol + orderId in the query. Errors
// like "order not found" (already filled / already cancelled) are
// returned by BingX as code=80014/80020 and surface as Go errors here;
// callers usually want to tolerate them when sweeping a "best-effort
// unwind".
func (c *Client) CancelOrder(ctx context.Context, sym market.Symbol, orderID string) error {
	if orderID == "" {
		return fmt.Errorf("orderID is required")
	}
	if c.DryRun {
		log.Printf("[DRY-RUN] would have cancelled order %s on %s", orderID, sym)
		return nil
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("orderId", orderID)
	return c.signedRequest(ctx, "DELETE", PathCancelOrder, q, nil)
}

// MarketCloseReduceOnly submits a reduce-only MARKET order that closes
// (the requested qty of) the live position immediately. Used by the
// /journal/:id/unwind flow when the user wants to abandon a trade.
func (c *Client) MarketCloseReduceOnly(ctx context.Context, sym market.Symbol, posSide string, qty float64, hedgeMode bool) (*OrderResult, error) {
	posSide = strings.ToLower(posSide)
	if posSide != "long" && posSide != "short" {
		return nil, fmt.Errorf("posSide must be long|short, got %q", posSide)
	}
	if qty <= 0 {
		return nil, fmt.Errorf("qty must be > 0, got %v", qty)
	}
	orderSide := "SELL"
	if posSide == "short" {
		orderSide = "BUY"
	}
	if c.DryRun {
		return dryRunResult(sym, orderSide, "MARKET_REDUCE_ONLY", 0, qty), nil
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", orderSide)
	q.Set("type", "MARKET")
	q.Set("quantity", strconv.FormatFloat(qty, 'f', -1, 64))
	if hedgeMode {
		if posSide == "long" {
			q.Set("positionSide", "LONG")
		} else {
			q.Set("positionSide", "SHORT")
		}
	} else {
		q.Set("reduceOnly", "true")
	}
	var resp rawOrderResp
	if err := c.signedRequest(ctx, "POST", PathOrder, q, &resp); err != nil {
		return nil, err
	}
	return &resp.Order, nil
}

// SetLeverage updates the symbol's leverage on BingX. In hedge mode the
// side ("LONG" or "SHORT") matters; in one-way mode pass "BOTH". The
// call is idempotent — BingX accepts re-setting to the same value.
func (c *Client) SetLeverage(ctx context.Context, sym market.Symbol, side string, leverage int) error {
	side = strings.ToUpper(side)
	if side != "LONG" && side != "SHORT" && side != "BOTH" {
		return fmt.Errorf("side must be LONG|SHORT|BOTH, got %q", side)
	}
	if leverage < 1 || leverage > 500 {
		return fmt.Errorf("leverage must be 1-500, got %d", leverage)
	}
	if c.DryRun {
		log.Printf("[DRY-RUN] would have set leverage %s side=%s lev=%d", sym, side, leverage)
		return nil
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("side", side)
	q.Set("leverage", strconv.Itoa(leverage))
	return c.signedRequest(ctx, "POST", PathLeverage, q, nil)
}
