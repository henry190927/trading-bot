// Package bingxvenue is the BingX MarketData adapter: the existing
// bingx.Client behind the venue interface. Read-only — orders keep going
// through bingx.Client directly, on curated symbols only.
package bingxvenue

import (
	"context"
	"strings"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/venue"
)

// Name is this venue's ID prefix.
const Name = "bingx"

// source is the slice of bingx.Client the adapter uses; a fake in tests.
type source interface {
	Contracts(ctx context.Context) ([]bingx.Contract, error)
	Klines(ctx context.Context, sym market.Symbol, tf market.Timeframe, limit int) ([]market.Candle, error)
}

// Adapter implements venue.MarketData and venue.Tradability.
type Adapter struct{ src source }

// New wraps a BingX client.
func New(c *bingx.Client) *Adapter { return &Adapter{src: c} }

func (a *Adapter) Venue() string { return Name }

// Timeframes: everything the BingX kline endpoint serves that this codebase
// names.
func (a *Adapter) Timeframes() []market.Timeframe {
	return []market.Timeframe{market.TF1m, market.TF5m, market.TF15m, market.TF30m,
		market.TF1h, market.TF2h, market.TF4h, market.TF1d}
}

// Klines is bingx.Client.Klines: closed bars only, oldest first.
func (a *Adapter) Klines(ctx context.Context, code string, tf market.Timeframe, limit int) ([]market.Candle, error) {
	return a.src.Klines(ctx, market.Symbol(strings.ToUpper(code)), tf, limit)
}

// IsTradable reports whether code is on the curated list (market/resolve.go).
func (a *Adapter) IsTradable(code string) bool {
	return market.Short(market.Symbol(strings.ToUpper(code))) != ""
}

// Instruments lists every contract with status 1 (trading). Status 25 —
// delisted / suspended — and anything else is left out (design §11.2).
func (a *Adapter) Instruments(ctx context.Context) ([]venue.Instrument, error) {
	rows, err := a.src.Contracts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]venue.Instrument, 0, len(rows))
	for _, c := range rows {
		if c.Status != bingx.ContractStatusTrading || c.Symbol == "" {
			continue
		}
		out = append(out, a.instrument(c))
	}
	return out, nil
}

func (a *Adapter) instrument(c bingx.Contract) venue.Instrument {
	code := strings.ToUpper(c.Symbol)
	quote := c.Currency
	if quote == "" {
		quote = "USDT"
	}
	return venue.Instrument{
		ID:             venue.MakeID(Name, code),
		Venue:          Name,
		Code:           code,
		Display:        display(c, quote),
		Asset:          c.Asset,
		Class:          classOf(code),
		Quote:          quote,
		PricePrecision: c.PricePrecision,
		Tradable:       a.IsTradable(code),
	}
}

// display is displayName without its "-USDT": "TSLA-USDT" → "TSLA",
// "GOLD(XAU)-USDT" → "GOLD(XAU)". Falls back to the code the same way.
func display(c bingx.Contract, quote string) string {
	name := c.DisplayName
	if name == "" {
		name = c.Symbol
	}
	return strings.TrimSuffix(name, "-"+quote)
}

// classOf reads the asset class off the contract code. BingX's synthetics
// carry it in a fixed prefix: NCSK stocks, NCFX FX, NCCO commodities, NCSI
// indices. Everything else is a crypto perpetual.
func classOf(code string) venue.Class {
	switch {
	case strings.HasPrefix(code, "NCSK"):
		return venue.ClassUSStock
	case strings.HasPrefix(code, "NCFX"):
		return venue.ClassFX
	case strings.HasPrefix(code, "NCCO"):
		return venue.ClassCommodity
	case strings.HasPrefix(code, "NCSI"):
		return venue.ClassIndex
	}
	return venue.ClassCrypto
}
