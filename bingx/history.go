package bingx

// Filled-order history. READ ONLY — every function here is a signed GET and
// none of them can create, modify or cancel an order.
//
// Why it exists: closing a trade by hand on the phone leaves the journal
// depending on someone's memory of the price. Two trades were closed over a
// weekend with a reversal in between, and reconstructing that from
// recollection is how an R baseline quietly drifts from what actually
// happened. The exchange knows; ask it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/henry190927/trading-bot/market"
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

// parseHistory copes with the THREE shapes BingX uses — {"orders":[...]},
// {"fill_orders":[...]} and {"fill_history_orders":[...]} — plus a bare array,
// and with numbers arriving as either JSON numbers or strings.
//
// The third was missing until 2026-09-19, which meant /trade/fillHistory —
// the endpoint named after this very feature — had never parsed once. It only
// went unnoticed because /trade/allOrders answers for some symbols, so BTC,
// XAU, XAG and SUI reported "no fill history" while their trades sat in a
// payload this function threw away as an unrecognised shape.
//
// That shape is also the only TRADE-level one: a single order is reported once
// per execution, so the 0.0615 BTC entry of 2026-09-17 arrives as three rows
// (0.0100 + 0.0452 + 0.0063) sharing one orderId. Rows are therefore folded by
// orderId — see foldByOrder — or every partial fill would be counted as its
// own trade and the fees summed over phantom orders.
func parseHistory(raw json.RawMessage, source string) ([]FilledOrder, error) {
	var envelope struct {
		Orders            []json.RawMessage `json:"orders"`
		FillOrders        []json.RawMessage `json:"fill_orders"`
		FillHistoryOrders []json.RawMessage `json:"fill_history_orders"`
	}
	rows := []json.RawMessage(nil)
	if err := json.Unmarshal(raw, &envelope); err == nil {
		switch {
		case len(envelope.Orders) > 0:
			rows = envelope.Orders
		case len(envelope.FillOrders) > 0:
			rows = envelope.FillOrders
		case len(envelope.FillHistoryOrders) > 0:
			rows = envelope.FillHistoryOrders
		}
	}
	if rows == nil {
		// Try a bare array before giving up.
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("unrecognised shape: %s", clip(raw, 160))
		}
	}

	out := make([]FilledOrder, 0, len(rows))
	quotes := make([]float64, 0, len(rows))
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

			// fill_history_orders spells the same facts differently: qty
			// rather than executedQty, a bare price rather than avgPrice,
			// realisedPNL rather than profit, and quoteQty — which is what
			// makes a volume-weighted average possible across partial fills.
			Qty         json.Number `json:"qty"`
			QuoteQty    json.Number `json:"quoteQty"`
			RealisedPNL json.Number `json:"realisedPNL"`
		}
		if err := json.Unmarshal(r, &o); err != nil {
			continue
		}
		qty := num(o.ExecutedQty)
		if qty == 0 {
			qty = num(o.OrigQty)
		}
		if qty == 0 {
			qty = num(o.Qty)
		}
		fee := num(o.Commission)
		if fee == 0 {
			fee = num(o.Fee)
		}
		profit := num(o.Profit)
		if profit == 0 {
			profit = num(o.RealisedPNL)
		}
		// avgPrice is absent on the trade-level shape; each row carries the
		// price it executed at, which IS the average for that row.
		avg := num(o.AvgPrice)
		if avg == 0 {
			avg = num(o.Price)
		}
		ts := num(o.UpdateTime)
		if ts == 0 {
			ts = num(o.Time)
		}
		f := FilledOrder{
			OrderID: o.OrderID.String(), Symbol: o.Symbol, Side: o.Side,
			PositionSide: o.PositionSide, Type: o.Type,
			AvgPrice: avg, Price: num(o.Price), Quantity: qty,
			ProfitUSDT: profit, FeeUSDT: fee, Status: o.Status,
			Source: source,
		}
		quote := num(o.QuoteQty)
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
			quotes = append(quotes, quote)
		}
	}
	return foldByOrder(out, quotes), nil
}

// foldByOrder collapses partial executions of one order into a single
// FilledOrder, preserving input order of first appearance.
//
// Only the trade-level shape splits an order across rows, but folding runs
// unconditionally: on an order-level payload each orderId appears once and
// this is the identity, which is cheaper than asking the caller to know which
// shape answered. An empty orderId cannot be grouped safely — two unrelated
// rows would merge — so those pass through untouched.
//
// Quantity, fee and realised PnL add. The price becomes the volume-weighted
// average via quoteQty when the payload supplies it, and otherwise stays the
// first row's — averaging prices unweighted across unequal fills would quietly
// misreport the entry a journal is reconciled against. Time takes the LAST
// execution: that is when the order was actually done.
func foldByOrder(in []FilledOrder, quotes []float64) []FilledOrder {
	type acc struct {
		idx   int
		quote float64
	}
	seen := make(map[string]*acc, len(in))
	out := make([]FilledOrder, 0, len(in))
	for i, f := range in {
		q := 0.0
		if i < len(quotes) {
			q = quotes[i]
		}
		if f.OrderID == "" {
			out = append(out, f)
			continue
		}
		a, ok := seen[f.OrderID]
		if !ok {
			out = append(out, f)
			seen[f.OrderID] = &acc{idx: len(out) - 1, quote: q}
			continue
		}
		p := &out[a.idx]
		p.Quantity += f.Quantity
		p.FeeUSDT += f.FeeUSDT
		p.ProfitUSDT += f.ProfitUSDT
		a.quote += q
		if f.Time.After(p.Time) {
			p.Time = f.Time
		}
		// A row that reports a type/status when the first did not is still
		// information about the same order.
		if p.Type == "" {
			p.Type = f.Type
		}
		if p.Status == "" {
			p.Status = f.Status
		}
	}
	for _, a := range seen {
		if a.quote > 0 && out[a.idx].Quantity > 0 {
			out[a.idx].AvgPrice = a.quote / out[a.idx].Quantity
		}
	}
	return out
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
