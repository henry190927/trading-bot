package bingx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"myFirstGo/trading-bot/market"
)

// FundingInfo carries the funding context for a perpetual symbol.
// Rate is a fraction per funding interval (e.g. 0.0001 = 1bp / 8h).
type FundingInfo struct {
	Symbol               market.Symbol
	MarkPrice            float64
	IndexPrice           float64
	Rate                 float64
	NextFundingTime      time.Time
	FundingIntervalHours int
}

// Client is the BingX REST client. Public endpoints (klines, depth, funding,
// open interest) need no auth; signed endpoints (positions, orders) need
// HMAC-SHA256 over the query string using APISecret.
type Client struct {
	APIKey    string
	APISecret string
	Host      string
	HTTP      *http.Client
}

func New(apiKey, apiSecret string) *Client {
	return &Client{
		APIKey:    apiKey,
		APISecret: apiSecret,
		Host:      HostSwap,
		HTTP:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Klines fetches OHLCV. BingX caps `limit` at 1440 per request.
// Endpoint: GET /openApi/swap/v3/quote/klines
func (c *Client) Klines(ctx context.Context, sym market.Symbol, tf market.Timeframe, limit int) ([]market.Candle, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 1440 {
		limit = 1440
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("interval", string(tf))
	q.Set("limit", strconv.Itoa(limit))

	// Decode in two passes: BingX returns data:{} (object) on errors and
	// data:[...] (array) on success, so we can't bind Data directly.
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.getJSON(ctx, PathKlines, q, &env); err != nil {
		return nil, fmt.Errorf("bingx klines: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("bingx klines: api code %d: %s", env.Code, env.Msg)
	}
	var raw []RawKline
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("bingx klines: decode data: %w", err)
	}

	dur := tfDuration(tf)
	out := make([]market.Candle, len(raw))
	for i, r := range raw {
		openT := time.UnixMilli(r.OpenTime)
		closeT := time.UnixMilli(r.CloseTime)
		if r.CloseTime == 0 && dur > 0 {
			closeT = openT.Add(dur - time.Millisecond)
		}
		out[i] = market.Candle{
			OpenTime:  openT,
			CloseTime: closeT,
			Open:      r.Open,
			High:      r.High,
			Low:       r.Low,
			Close:     r.Close,
			Volume:    r.Volume,
		}
	}
	// BingX returns newest first; flip to ascending by OpenTime so indicators
	// can consume it left-to-right.
	if len(out) > 1 && out[0].OpenTime.After(out[len(out)-1].OpenTime) {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	out = dropForming(out, time.Now())
	return out, nil
}

// Depth returns the top `limit` levels of the order book. Max 1000 per docs.
func (c *Client) Depth(ctx context.Context, sym market.Symbol, limit int) (market.Depth, error) {
	if limit <= 0 {
		limit = 100
	}
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("limit", strconv.Itoa(limit))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			T    int64       `json:"T"`
			Bids [][2]string `json:"bids"`
			Asks [][2]string `json:"asks"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, PathDepth, q, &env); err != nil {
		return market.Depth{}, fmt.Errorf("bingx depth: %w", err)
	}
	if env.Code != 0 {
		return market.Depth{}, fmt.Errorf("bingx depth: api code %d: %s", env.Code, env.Msg)
	}
	return market.Depth{
		Time: time.UnixMilli(env.Data.T),
		Bids: parseLevels(env.Data.Bids),
		Asks: parseLevels(env.Data.Asks),
	}, nil
}

// FundingRate returns the current funding context for a perpetual symbol.
// Endpoint: GET /openApi/swap/v2/quote/premiumIndex
func (c *Client) FundingRate(ctx context.Context, sym market.Symbol) (FundingInfo, error) {
	q := url.Values{}
	q.Set("symbol", string(sym))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Symbol               string  `json:"symbol"`
			MarkPrice            float64 `json:"markPrice,string"`
			IndexPrice           float64 `json:"indexPrice,string"`
			LastFundingRate      float64 `json:"lastFundingRate,string"`
			NextFundingTime      int64   `json:"nextFundingTime"`
			FundingIntervalHours int     `json:"fundingIntervalHours"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, PathFunding, q, &env); err != nil {
		return FundingInfo{}, fmt.Errorf("bingx funding: %w", err)
	}
	if env.Code != 0 {
		return FundingInfo{}, fmt.Errorf("bingx funding: api code %d: %s", env.Code, env.Msg)
	}
	return FundingInfo{
		Symbol:               sym,
		MarkPrice:            env.Data.MarkPrice,
		IndexPrice:           env.Data.IndexPrice,
		Rate:                 env.Data.LastFundingRate,
		NextFundingTime:      time.UnixMilli(env.Data.NextFundingTime),
		FundingIntervalHours: env.Data.FundingIntervalHours,
	}, nil
}

// OpenInterest returns total OI in quote currency (USDT) notional.
// Endpoint: GET /openApi/swap/v2/quote/openInterest
func (c *Client) OpenInterest(ctx context.Context, sym market.Symbol) (float64, error) {
	q := url.Values{}
	q.Set("symbol", string(sym))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenInterest float64 `json:"openInterest,string"`
			Symbol       string  `json:"symbol"`
			Time         int64   `json:"time"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, PathOpenInt, q, &env); err != nil {
		return 0, fmt.Errorf("bingx open interest: %w", err)
	}
	if env.Code != 0 {
		return 0, fmt.Errorf("bingx open interest: api code %d: %s", env.Code, env.Msg)
	}
	return env.Data.OpenInterest, nil
}

func parseLevels(pairs [][2]string) []market.DepthLevel {
	out := make([]market.DepthLevel, 0, len(pairs))
	for _, p := range pairs {
		price, err1 := strconv.ParseFloat(p[0], 64)
		qty, err2 := strconv.ParseFloat(p[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, market.DepthLevel{Price: price, Quantity: qty})
	}
	return out
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	endpoint := c.Host + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build req: %w", err)
	}
	if c.APIKey != "" {
		req.Header.Set("X-BX-APIKEY", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func tfDuration(tf market.Timeframe) time.Duration {
	switch tf {
	case market.TF1m:
		return time.Minute
	case market.TF5m:
		return 5 * time.Minute
	case market.TF15m:
		return 15 * time.Minute
	case market.TF1h:
		return time.Hour
	case market.TF4h:
		return 4 * time.Hour
	case market.TF1d:
		return 24 * time.Hour
	}
	return 0
}
