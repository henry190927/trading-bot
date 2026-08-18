// Package finnhub fetches the earnings calendar from Finnhub and renders it
// into the earnings.json shape the earnings package loads. It is kept in a
// SUBPACKAGE so the core earnings provider stays free of net/http and remains
// a pure, dependency-light data type. The API token is read from the
// FINNHUB_KEY env var by the caller (per-user, same pattern as GEMINI_API_KEY)
// and passed in — this package never touches os.Getenv or logs the token.
package finnhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is Finnhub's REST root. Overridable on Client for tests.
const DefaultBaseURL = "https://finnhub.io/api/v1"

// UTC anchor times synthesized from Finnhub's date-only + hour tag. Finnhub
// gives a calendar DATE and an "hour" bucket (bmo/amc/dmh), not a timestamp;
// we anchor to a representative UTC time per bucket. The blackout window
// (amc +16h etc, see earnings.applyDefaults) is wide enough to absorb the
// hour-level imprecision, so exact minutes don't matter.
var (
	anchorAMC   = dayTime{20, 5} // after US close (16:00 ET ≈ 20:00–21:00 UTC)
	anchorBMO   = dayTime{11, 0} // before US open (pre 09:30 ET)
	anchorOther = dayTime{16, 0} // during-hours / unknown
)

type dayTime struct{ h, m int }

// Client is a minimal Finnhub REST client.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client with a sane HTTP timeout. token must be non-empty
// (the caller reads FINNHUB_KEY); an empty token yields a client whose calls
// fail fast with a clear error rather than hitting the API unauthenticated.
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Event mirrors the earnings.Event JSON shape without importing the earnings
// package (keeps the dependency arrow one-way / absent). Emitted into the file.
type Event struct {
	Symbol        string `json:"symbol"`
	DatetimeUTC   string `json:"datetime_utc"`
	When          string `json:"when"`
	BeforeMinutes int    `json:"blackout_before_min"`
	AfterMinutes  int    `json:"blackout_after_min"`
}

// File is the earnings.json wrapper the earnings package parses.
type File struct {
	UpdatedUTC string  `json:"updated_utc"`
	Events     []Event `json:"events"`
}

// calResp is Finnhub's /calendar/earnings response (fields we use only).
type calResp struct {
	EarningsCalendar []struct {
		Date   string `json:"date"`   // "2026-08-27"
		Hour   string `json:"hour"`   // "bmo" | "amc" | "dmh" | ""
		Symbol string `json:"symbol"` // "NVDA"
	} `json:"earningsCalendar"`
}

// FetchSymbol pulls the earnings calendar for one ticker between from/to
// (YYYY-MM-DD) and maps each row to an Event with a synthesized UTC datetime.
// Rows with an unparseable date are skipped (best-effort), not fatal.
func (c *Client) FetchSymbol(ctx context.Context, ticker, from, to string) ([]Event, error) {
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
	endpoint := base + "/calendar/earnings?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("finnhub: build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("finnhub: GET calendar/earnings %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("finnhub: calendar/earnings %s: HTTP %d", ticker, resp.StatusCode)
	}
	var cr calResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("finnhub: decode %s: %w", ticker, err)
	}
	return mapRows(cr), nil
}

// mapRows converts a decoded response into Events (pure; unit-tested).
func mapRows(cr calResp) []Event {
	out := make([]Event, 0, len(cr.EarningsCalendar))
	for _, r := range cr.EarningsCalendar {
		dt, when, err := synthDatetime(r.Date, r.Hour)
		if err != nil {
			continue // skip unparseable date, don't fail the whole fetch
		}
		out = append(out, Event{
			Symbol:      strings.ToUpper(strings.TrimSpace(r.Symbol)),
			DatetimeUTC: dt.Format(time.RFC3339),
			When:        when,
			// Leave windows 0 → earnings.applyDefaults fills them from `when`.
		})
	}
	return out
}

// synthDatetime turns a Finnhub date ("2006-01-02") + hour bucket into a UTC
// timestamp and a normalized "when" tag.
func synthDatetime(date, hour string) (time.Time, string, error) {
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), time.UTC)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("bad date %q: %w", date, err)
	}
	when := strings.ToLower(strings.TrimSpace(hour))
	var a dayTime
	switch when {
	case "amc":
		a = anchorAMC
	case "bmo":
		a = anchorBMO
	default:
		a = anchorOther
		when = "" // normalize dmh/unknown to "" (symmetric default window)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), a.h, a.m, 0, 0, time.UTC), when, nil
}

// BuildFile assembles the earnings.json payload from events fetched across
// symbols, sorted by (datetime, symbol) for a stable diff.
func BuildFile(events []Event, updated time.Time) ([]byte, error) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].DatetimeUTC != events[j].DatetimeUTC {
			return events[i].DatetimeUTC < events[j].DatetimeUTC
		}
		return events[i].Symbol < events[j].Symbol
	})
	f := File{UpdatedUTC: updated.UTC().Format(time.RFC3339), Events: events}
	return json.MarshalIndent(f, "", "  ")
}

// WriteFileAtomic writes data to path via a temp file + rename so a reader
// never sees a half-written file. Caller is responsible for chowning back to
// ubuntu:ubuntu on the VPS after this (the web/monitor need write access).
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("finnhub: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("finnhub: rename: %w", err)
	}
	return nil
}
