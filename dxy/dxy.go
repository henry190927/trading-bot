// Package dxy fetches U.S. Dollar Index (DXY) candles from Yahoo Finance and
// classifies the current trend. Used as a macro veto on XAU/XAG signals:
// precious metals have a strong inverse relationship with the dollar, so
// long-XAU into a strengthening DXY is fighting macro flow and historically
// burns through stops.
//
// Data source: query1.finance.yahoo.com chart endpoint. No API key required.
// Free, anonymous, rate-limited but generous enough for one fetch per minute.
package dxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"myFirstGo/trading/market"
)

// Yahoo symbol for the ICE U.S. Dollar Index. NYICDX is the cash index;
// DX-Y.NYB is the more reliable ticker historically.
const yahooSymbol = "DX-Y.NYB"
const yahooBaseURL = "https://query1.finance.yahoo.com/v8/finance/chart"

// Trend classifies the current DXY direction. Up = USD strengthening
// (bearish for XAU/XAG). Down = USD weakening (bullish for XAU/XAG).
type Trend int

const (
	Flat Trend = iota
	Up
	Down
)

func (t Trend) String() string {
	switch t {
	case Up:
		return "DXY-up"
	case Down:
		return "DXY-down"
	}
	return "DXY-flat"
}

// Fetch pulls DXY candles for the given Yahoo interval ("1h", "4h", "1d") and
// lookback range ("60d", "6mo", "1y"). Candles are returned in chronological
// order with Volume = 0 (Yahoo often omits DXY volume).
func Fetch(ctx context.Context, interval, rng string) ([]market.Candle, error) {
	url := fmt.Sprintf("%s/%s?interval=%s&range=%s", yahooBaseURL, yahooSymbol, interval, rng)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo refuses requests with the default Go user-agent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trading-bot/1.0)")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dxy fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("dxy fetch: status %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Open  []float64 `json:"open"`
						High  []float64 `json:"high"`
						Low   []float64 `json:"low"`
						Close []float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Chart.Result) == 0 || len(data.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, errors.New("dxy: empty result")
	}

	r := data.Chart.Result[0]
	q := r.Indicators.Quote[0]
	out := make([]market.Candle, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		// Yahoo emits null entries for missing data points; we drop them.
		if i >= len(q.Open) || q.Open[i] == 0 || q.Close[i] == 0 {
			continue
		}
		out = append(out, market.Candle{
			OpenTime: time.Unix(ts, 0).UTC(),
			Open:     q.Open[i],
			High:     q.High[i],
			Low:      q.Low[i],
			Close:    q.Close[i],
		})
	}
	return out, nil
}

// Classify returns the trend at the END of the candle series — close vs the
// 20-bar SMA, qualified by the slope of that SMA over the last 5 bars.
// Needs at least 25 candles.
func Classify(candles []market.Candle) Trend {
	if len(candles) < 25 {
		return Flat
	}
	last := len(candles) - 1
	var sma20, sma20Past float64
	for i := last - 19; i <= last; i++ {
		sma20 += candles[i].Close
	}
	for i := last - 24; i <= last-5; i++ {
		sma20Past += candles[i].Close
	}
	sma20 /= 20.0
	sma20Past /= 20.0

	close := candles[last].Close
	slopeUp := sma20 > sma20Past
	slopeDown := sma20 < sma20Past

	switch {
	case close > sma20 && slopeUp:
		return Up
	case close < sma20 && slopeDown:
		return Down
	}
	return Flat
}

// TrendAt returns the DXY trend that would have been observable at time t,
// using only candles whose OpenTime is ≤ t. Used by the backtest to avoid
// look-ahead bias.
func TrendAt(candles []market.Candle, t time.Time) Trend {
	cutoff := -1
	for i := len(candles) - 1; i >= 0; i-- {
		if !candles[i].OpenTime.After(t) {
			cutoff = i + 1
			break
		}
	}
	if cutoff < 25 {
		return Flat
	}
	return Classify(candles[:cutoff])
}
