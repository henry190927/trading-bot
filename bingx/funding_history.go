package bingx

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// FundingPoint is one historical funding-rate observation. BingX
// publishes a new value every fundingInterval (typically 8h for crypto
// perps, sometimes 4h for the NCCO synthetic metals).
type FundingPoint struct {
	Time time.Time
	Rate float64
}

// FundingRateHistory returns the most recent `limit` funding-rate
// observations for the symbol, oldest first. limit is capped at 1000
// by BingX. For an N-day backtest window with 8h intervals, you need
// ~N×3 points; the cap of 1000 covers ~333 days, plenty for our
// 60/90/120d backtests.
//
// Endpoint: GET /openApi/swap/v2/quote/fundingRate
func (c *Client) FundingRateHistory(ctx context.Context, sym market.Symbol, limit int) ([]FundingPoint, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("limit", strconv.Itoa(limit))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// BingX returns either {"data": [...]} or {"data": {"list": [...]}} on
		// different endpoints. funding history uses the flat array form.
		Data []struct {
			Symbol      string  `json:"symbol"`
			FundingRate float64 `json:"fundingRate,string"`
			FundingTime int64   `json:"fundingTime"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, PathFundingHistory, q, &env); err != nil {
		return nil, fmt.Errorf("bingx funding history: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("bingx funding history: api code %d: %s", env.Code, env.Msg)
	}
	out := make([]FundingPoint, 0, len(env.Data))
	for _, p := range env.Data {
		out = append(out, FundingPoint{
			Time: time.UnixMilli(p.FundingTime),
			Rate: p.FundingRate,
		})
	}
	// BingX returns newest-first; we want oldest-first for binary search.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// FundingAtTime returns the funding rate in effect at instant t. Given
// funding updates discretely every interval (8h), the "current" rate at
// any instant is the most-recently-published value with FundingTime ≤ t.
// Returns 0 (and false) if no point predates t — happens for the very
// start of a backtest before any funding data exists.
func FundingAtTime(history []FundingPoint, t time.Time) (float64, bool) {
	if len(history) == 0 {
		return 0, false
	}
	// Binary-search the rightmost point with Time ≤ t.
	lo, hi := 0, len(history)
	for lo < hi {
		mid := (lo + hi) / 2
		if history[mid].Time.After(t) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo == 0 {
		return 0, false
	}
	return history[lo-1].Rate, true
}
