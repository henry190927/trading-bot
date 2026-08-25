package finnhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// NewsItem is one company-news headline from Finnhub's /company-news endpoint
// (free tier). Datetime is unix seconds.
type NewsItem struct {
	Headline string `json:"headline"`
	URL      string `json:"url"`
	Source   string `json:"source"`
	Summary  string `json:"summary"`
	Datetime int64  `json:"datetime"`
}

// FetchNews returns recent company news for a US ticker, newest first, capped
// to `limit`. from/to are "2006-01-02" dates. Mirrors FetchSymbol's request
// shape; the endpoint returns a bare JSON array.
func (c *Client) FetchNews(ctx context.Context, ticker, from, to string, limit int) ([]NewsItem, error) {
	if strings.TrimSpace(c.Token) == "" {
		return nil, fmt.Errorf("finnhub: empty token (set FINNHUB_KEY)")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(ticker))
	q.Set("from", from)
	q.Set("to", to)
	q.Set("token", c.Token)
	endpoint := base + "/company-news?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("finnhub: build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("finnhub: GET company-news %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("finnhub: company-news %s: HTTP %d", ticker, resp.StatusCode)
	}
	var items []NewsItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("finnhub: decode news %s: %w", ticker, err)
	}
	// Newest first, drop empty headlines, cap.
	sort.Slice(items, func(i, j int) bool { return items[i].Datetime > items[j].Datetime })
	out := make([]NewsItem, 0, limit)
	for _, it := range items {
		if strings.TrimSpace(it.Headline) == "" {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// NewsWindow returns from/to date strings for the last n days (UTC), the shape
// FetchNews expects.
func NewsWindow(now time.Time, days int) (from, to string) {
	return now.AddDate(0, 0, -days).Format("2006-01-02"), now.Format("2006-01-02")
}
