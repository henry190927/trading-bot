package bingx

import (
	"context"
	"net/url"
	"strconv"

	"myFirstGo/trading-bot/market"
)

// Position is a (much) trimmed-down view of one BingX open position.
// We capture only what the journal/TP-placement flow needs.
type Position struct {
	Symbol     market.Symbol
	PositionSide string  // "LONG" | "SHORT" (hedge mode) | "BOTH" (one-way)
	Side         string  // "long" | "short" — normalized from PositionSide / quantity sign
	Quantity     float64 // signed in one-way mode; we always return abs here
	EntryPrice   float64
	Leverage     int
	MarginMode   string // "isolated" | "cross"
}

// rawPosition mirrors the BingX position payload. Fields are stringly-typed.
type rawPosition struct {
	Symbol           string `json:"symbol"`
	PositionId       string `json:"positionId"`
	PositionSide     string `json:"positionSide"` // LONG / SHORT / BOTH
	Isolated         bool   `json:"isolated"`
	PositionAmt      string `json:"positionAmt"` // signed in one-way; absolute in hedge
	AvailableAmt     string `json:"availableAmt"`
	UnrealizedProfit string `json:"unrealizedProfit"`
	RealisedProfit   string `json:"realisedProfit"`
	InitialMargin    string `json:"initialMargin"`
	AvgPrice         string `json:"avgPrice"`
	Leverage         int    `json:"leverage"`
}

// OpenPosition returns the currently open position for sym, or (nil, nil)
// if no position is open. Both hedge mode (positionSide=LONG/SHORT) and
// one-way mode (positionSide=BOTH with signed positionAmt) are handled.
//
// In hedge mode, a symbol can have two positions simultaneously (one
// LONG + one SHORT); we return the FIRST non-zero one and let the caller
// match by side if needed. For the TP1-placement flow the caller already
// knows the intended side from the journal, so they can match.
func (c *Client) OpenPositions(ctx context.Context, sym market.Symbol) ([]Position, error) {
	q := url.Values{}
	q.Set("symbol", string(sym))

	var raws []rawPosition
	if err := c.signedRequest(ctx, "GET", PathPositions, q, &raws); err != nil {
		return nil, err
	}

	out := make([]Position, 0, len(raws))
	for _, r := range raws {
		amt, _ := strconv.ParseFloat(r.PositionAmt, 64)
		if amt == 0 {
			continue
		}
		avgPx, _ := strconv.ParseFloat(r.AvgPrice, 64)
		p := Position{
			Symbol:       market.Symbol(r.Symbol),
			PositionSide: r.PositionSide,
			Quantity:     abs(amt),
			EntryPrice:   avgPx,
			Leverage:     r.Leverage,
		}
		switch r.PositionSide {
		case "LONG":
			p.Side = "long"
		case "SHORT":
			p.Side = "short"
		case "BOTH":
			// one-way mode: sign of positionAmt encodes direction.
			if amt > 0 {
				p.Side = "long"
			} else {
				p.Side = "short"
			}
		}
		if r.Isolated {
			p.MarginMode = "isolated"
		} else {
			p.MarginMode = "cross"
		}
		out = append(out, p)
	}
	return out, nil
}

// FindOpenPosition returns the open position matching `side` ("long"/"short")
// for sym, or nil if no matching position. Convenience wrapper for the
// TP-placement flow where the journal already knows the intended side.
func (c *Client) FindOpenPosition(ctx context.Context, sym market.Symbol, side string) (*Position, error) {
	ps, err := c.OpenPositions(ctx, sym)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].Side == side {
			return &ps[i], nil
		}
	}
	return nil, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
