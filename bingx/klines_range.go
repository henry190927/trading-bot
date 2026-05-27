package bingx

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

// KlinesRange fetches all candles in [start, end] by paging backward from `end`.
// BingX caps a single request at 1440 bars, so this is the only way to get
// multi-month history at intraday timeframes.
//
// Pacing: small sleep between pages to stay under public-endpoint rate limits.
func (c *Client) KlinesRange(ctx context.Context, sym market.Symbol, tf market.Timeframe, start, end time.Time) ([]market.Candle, error) {
	dur := tfDuration(tf)
	if dur == 0 {
		return nil, fmt.Errorf("bingx KlinesRange: unsupported timeframe %s", tf)
	}
	const pageSize = 1440
	var all []market.Candle
	cursor := end

	for cursor.After(start) {
		batch, err := c.klinesUntil(ctx, sym, tf, cursor, pageSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		// Move cursor to one bar before the earliest fetched bar.
		earliest := batch[0].OpenTime
		next := earliest.Add(-dur)
		if !next.Before(cursor) {
			// Defensive: avoid infinite loop on pathological responses.
			break
		}
		cursor = next
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}

	// De-dup by OpenTime, keep ascending order.
	sort.Slice(all, func(i, j int) bool { return all[i].OpenTime.Before(all[j].OpenTime) })
	out := all[:0]
	var prev int64
	for _, c := range all {
		ts := c.OpenTime.UnixMilli()
		if ts == prev {
			continue
		}
		if c.OpenTime.Before(start) || c.OpenTime.After(end) {
			continue
		}
		out = append(out, c)
		prev = ts
	}
	// Defensive: nearly always a no-op for historical ranges (end is in the
	// past), but trims a forming bar if the caller set end >= time.Now().
	out = dropForming(out, time.Now())
	return out, nil
}

func (c *Client) klinesUntil(ctx context.Context, sym market.Symbol, tf market.Timeframe, endT time.Time, limit int) ([]market.Candle, error) {
	q := url.Values{}
	q.Set("symbol", string(sym))
	q.Set("interval", string(tf))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("endTime", strconv.FormatInt(endT.UnixMilli(), 10))

	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.getJSON(ctx, PathKlines, q, &env); err != nil {
		return nil, fmt.Errorf("bingx KlinesRange: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("bingx KlinesRange: api code %d: %s", env.Code, env.Msg)
	}
	var raw []RawKline
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("bingx KlinesRange: decode: %w", err)
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
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	return out, nil
}
