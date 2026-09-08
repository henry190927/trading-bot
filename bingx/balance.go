package bingx

// Account balance. READ ONLY — one signed GET, no parameters, nothing mutable.
//
// This was inline in cmd/web's /ops/balance handler until sizing needed it too.
// A trade's risk cannot be stated without equity: notional alone says nothing,
// because 16,463u is a rounding error on a large account and a liquidation on a
// small one. So the read moved here, where the order-placement path can reach
// it as well as the panel.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Balance is the account-level view. Fields are pointers so "the exchange did
// not send this" stays distinguishable from "the exchange sent zero" — a
// balance of 0 is a real state (it happened on 2026-09-08) and must never be
// confused with a parse failure.
type Balance struct {
	Asset            string
	Balance          *float64
	Equity           *float64
	AvailableMargin  *float64
	UsedMargin       *float64
	UnrealizedProfit *float64
	RealisedProfit   *float64
	// Raw is the untouched payload. Echoed by /ops/balance so a response-shape
	// change is visible rather than silently zeroing every field.
	Raw json.RawMessage
}

// EquityOrZero returns equity, falling back to balance when the exchange omits
// equity, and 0 when neither is present. The bool reports whether a number was
// actually read — callers that gate on equity MUST check it, because treating
// "unknown" as 0 turns an API hiccup into an infinite computed leverage.
func (b Balance) EquityOrZero() (float64, bool) {
	if b.Equity != nil {
		return *b.Equity, true
	}
	if b.Balance != nil {
		return *b.Balance, true
	}
	return 0, false
}

// AccountBalance fetches the perpetual-swap account balance.
func (c *Client) AccountBalance(ctx context.Context) (Balance, error) {
	raw, err := c.SignedGetRaw(ctx, PathBalance, nil)
	if err != nil {
		return Balance{}, fmt.Errorf("bingx balance: %w", err)
	}
	return parseBalance(raw)
}

// parseBalance accepts the nested {"balance":{...}} shape BingX documents and
// a flat top-level object, so a shape change degrades to "some fields missing"
// instead of "everything is zero".
func parseBalance(raw json.RawMessage) (Balance, error) {
	type fields struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		Equity           string `json:"equity"`
		AvailableMargin  string `json:"availableMargin"`
		UsedMargin       string `json:"usedMargin"`
		UnrealizedProfit string `json:"unrealizedProfit"`
		RealisedProfit   string `json:"realisedProfit"`
	}
	var nested struct {
		Balance fields `json:"balance"`
	}
	var flat fields
	_ = json.Unmarshal(raw, &nested)
	_ = json.Unmarshal(raw, &flat)

	f := nested.Balance
	// The nested shape is preferred; fall back per-field so a flat response
	// still populates.
	if f.Balance == "" && f.Equity == "" {
		f = flat
	}
	if f.Asset == "" {
		f.Asset = flat.Asset
	}

	b := Balance{Asset: f.Asset, Raw: raw}
	b.Balance = optFloat(f.Balance)
	b.Equity = optFloat(f.Equity)
	b.AvailableMargin = optFloat(f.AvailableMargin)
	b.UsedMargin = optFloat(f.UsedMargin)
	b.UnrealizedProfit = optFloat(f.UnrealizedProfit)
	b.RealisedProfit = optFloat(f.RealisedProfit)
	if b.Balance == nil && b.Equity == nil {
		return b, fmt.Errorf("bingx balance: no balance or equity field in %s", clip(raw, 200))
	}
	return b, nil
}

// optFloat parses a stringly-typed number, returning nil for absent or
// unparseable input rather than 0.
func optFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}
