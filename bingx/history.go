package bingx

// Filled-order history. READ ONLY — every function here is a signed GET and
// none of them can create, modify or cancel an order.
//
// Why it exists: closing a trade by hand on the phone leaves the journal
// depending on the trader's memory of the price. #63/#64 were closed over a
// weekend at 79,805.5 and 2,493.65 with a reversal in between, and
// reconstructing that from recollection is how an R baseline quietly drifts
// from what actually happened. The exchange knows; ask it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"myFirstGo/trading-bot/market"
)

// FilledOrder is one execution as the exchange recorded it.
type FilledOrder struct {
	OrderID      string
	Symbol       string
	Side         string  // BUY / SELL
	PositionSide string  // LONG / SHORT / BOTH
	Type         string  // LIMIT / MARKET / STOP_MARKET / ...
	AvgPrice     float64 // the number the journal needs
	Price        float64 // the order's limit price, when it had one
	Quantity     float64
	ProfitUSDT   float64 // realised PnL on this fill, when reported
	FeeUSDT      float64
	Status       string
	Time         time.Time
	Source       string // which endpoint answered — see OrderHistory
}

// historyPaths are the candidates tried in order. BingX's docs and its live
// behaviour have disagreed here before, so rather than hardcode one guess and
// ship a silent empty list, every path is tried and the one that answers is
// recorded on each row. A wrong guess then shows up as a named error instead
// of as "you had no trades".
var historyPaths = []string{
	"/openApi/swap/v2/trade/allFillOrders",
	"/openApi/swap/v2/trade/fillHistory",
	"/openApi/swap/v2/trade/allOrders",
}

// OrderHistory returns the filled orders for sym between start and end.
//
// Errors from every candidate path are joined, so a caller sees which
// endpoints were tried and what each said — the failure mode this replaces is
// an empty slice that reads like "no trades".
func (c *Client) OrderHistory(ctx context.Context, sym market.Symbol, start, end time.Time) ([]FilledOrder, error) {
	var errs []string
	for _, p := range historyPaths {
		q := url.Values{}
		q.Set("symbol", string(sym))
		q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
		q.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
		q.Set("limit", "500")

		raw, err := c.SignedGetRaw(ctx, p, q)
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		out, perr := parseHistory(raw, p)
		if perr != nil {
			errs = append(errs, p+": "+perr.Error())
			continue
		}
		if len(out) == 0 {
			// A path that answers with an empty list is not proof of no
			// trades — it may be the wrong path answering politely. Keep
			// trying, and only report empty if every path agrees.
			errs = append(errs, p+": answered with 0 rows")
			continue
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
		return out, nil
	}
	return nil, fmt.Errorf("no fill history: %v", errs)
}

// parseHistory copes with the two shapes BingX uses — {"orders":[...]} and
// {"fill_orders":[...]} — plus a bare array, and with numbers arriving as
// either JSON numbers or strings.
func parseHistory(raw json.RawMessage, source string) ([]FilledOrder, error) {
	var envelope struct {
		Orders     []json.RawMessage `json:"orders"`
		FillOrders []json.RawMessage `json:"fill_orders"`
	}
	rows := []json.RawMessage(nil)
	if err := json.Unmarshal(raw, &envelope); err == nil {
		switch {
		case len(envelope.Orders) > 0:
			rows = envelope.Orders
		case len(envelope.FillOrders) > 0:
			rows = envelope.FillOrders
		}
	}
	if rows == nil {
		// Try a bare array before giving up.
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("unrecognised shape: %s", clip(raw, 160))
		}
	}

	out := make([]FilledOrder, 0, len(rows))
	for _, r := range rows {
		var o struct {
			OrderID      json.Number `json:"orderId"`
			Symbol       string      `json:"symbol"`
			Side         string      `json:"side"`
			PositionSide string      `json:"positionSide"`
			Type         string      `json:"type"`
			AvgPrice     json.Number `json:"avgPrice"`
			Price        json.Number `json:"price"`
			ExecutedQty  json.Number `json:"executedQty"`
			OrigQty      json.Number `json:"origQty"`
			Profit       json.Number `json:"profit"`
			Commission   json.Number `json:"commission"`
			Fee          json.Number `json:"fee"`
			Status       string      `json:"status"`
			UpdateTime   json.Number `json:"updateTime"`
			Time         json.Number `json:"time"`
			FilledTime   string      `json:"filledTime"`
		}
		if err := json.Unmarshal(r, &o); err != nil {
			continue
		}
		qty := num(o.ExecutedQty)
		if qty == 0 {
			qty = num(o.OrigQty)
		}
		fee := num(o.Commission)
		if fee == 0 {
			fee = num(o.Fee)
		}
		ts := num(o.UpdateTime)
		if ts == 0 {
			ts = num(o.Time)
		}
		f := FilledOrder{
			OrderID: o.OrderID.String(), Symbol: o.Symbol, Side: o.Side,
			PositionSide: o.PositionSide, Type: o.Type,
			AvgPrice: num(o.AvgPrice), Price: num(o.Price), Quantity: qty,
			ProfitUSDT: num(o.Profit), FeeUSDT: fee, Status: o.Status,
			Source: source,
		}
		if ts > 0 {
			f.Time = time.UnixMilli(int64(ts))
		} else if o.FilledTime != "" {
			if t, err := time.Parse(time.RFC3339, o.FilledTime); err == nil {
				f.Time = t
			}
		}
		// Only rows that actually executed are useful for a journal.
		if f.Quantity > 0 && f.AvgPrice > 0 {
			out = append(out, f)
		}
	}
	return out, nil
}

func num(n json.Number) float64 {
	v, err := n.Float64()
	if err != nil {
		return 0
	}
	return v
}

func clip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
