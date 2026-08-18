package finnhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Metrics is the subset of Finnhub /stock/metric "basic financials" we use for
// the fundamental (F1 quality / F2 valuation) scoring of consumer B, the
// standalone spot indicator. All fields are best-effort: Finnhub returns null
// for metrics it lacks, which decode to 0 here — the scorer treats 0/absent as
// "unknown" and abstains on that sub-factor rather than scoring it as bad.
type Metrics struct {
	Symbol string

	// F2 valuation
	PE         float64 // peTTM
	PS         float64 // psTTM
	PB         float64 // pbQuarterly
	Week52High float64 // 52WeekHigh
	Week52Low  float64 // 52WeekLow
	MarketCap  float64 // marketCapitalization (millions)

	// F1 growth
	RevGrowthYoY float64 // revenueGrowthTTMYoy (percent)
	EPSGrowthYoY float64 // epsGrowthTTMYoy (percent)

	// F1 profitability (percent)
	NetMargin   float64 // netProfitMarginTTM
	GrossMargin float64 // grossMarginTTM
	OpMargin    float64 // operatingMarginTTM
	ROE         float64 // roeTTM

	// F1 balance sheet
	CurrentRatio float64 // currentRatioQuarterly
	DebtToEquity float64 // totalDebt/totalEquityQuarterly
}

// FetchMetrics pulls /stock/metric?metric=all for one ticker and extracts the
// F1/F2 fields. Robust to Finnhub's null/mixed-type values (each field decoded
// defensively; missing → 0).
func (c *Client) FetchMetrics(ctx context.Context, ticker string) (*Metrics, error) {
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("finnhub: empty token (set FINNHUB_KEY)")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(ticker))
	q.Set("metric", "all")
	q.Set("token", c.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stock/metric?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("finnhub: build metric request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("finnhub: GET stock/metric %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("finnhub: stock/metric %s: HTTP %d", ticker, resp.StatusCode)
	}
	var raw struct {
		Symbol string                     `json:"symbol"`
		Metric map[string]json.RawMessage `json:"metric"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("finnhub: decode metric %s: %w", ticker, err)
	}
	return mapMetrics(strings.ToUpper(ticker), raw.Metric), nil
}

// mapMetrics extracts the fields we care about (pure; unit-tested).
func mapMetrics(sym string, m map[string]json.RawMessage) *Metrics {
	return &Metrics{
		Symbol:       sym,
		PE:           mFloat(m, "peTTM"),
		PS:           mFloat(m, "psTTM"),
		PB:           mFloat(m, "pbQuarterly"),
		Week52High:   mFloat(m, "52WeekHigh"),
		Week52Low:    mFloat(m, "52WeekLow"),
		MarketCap:    mFloat(m, "marketCapitalization"),
		RevGrowthYoY: mFloat(m, "revenueGrowthTTMYoy"),
		EPSGrowthYoY: mFloat(m, "epsGrowthTTMYoy"),
		NetMargin:    mFloat(m, "netProfitMarginTTM"),
		GrossMargin:  mFloat(m, "grossMarginTTM"),
		OpMargin:     mFloat(m, "operatingMarginTTM"),
		ROE:          mFloat(m, "roeTTM"),
		CurrentRatio: mFloat(m, "currentRatioQuarterly"),
		DebtToEquity: mFloat(m, "totalDebt/totalEquityQuarterly"),
	}
}

// mFloat pulls a float from the raw metric map, tolerating null / missing /
// string-encoded numbers (returns 0 on any of those).
func mFloat(m map[string]json.RawMessage, key string) float64 {
	raw, ok := m[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	// Some values may arrive as JSON strings; try that.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var sf float64
		if _, err := fmt.Sscanf(s, "%g", &sf); err == nil {
			return sf
		}
	}
	return 0
}
