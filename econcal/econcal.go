// Package econcal pulls the upcoming US economic-data-release calendar (CPI, GDP,
// jobless claims, retail sales, …) with impact + forecast + previous — the FX-style
// data calendar the curated macro/events.json list doesn't carry. Source is the
// free ForexFactory weekly JSON (unofficial but widely used). It's a DISPLAY layer
// for /calendar; it does NOT drive the engine blackout (that stays on the curated
// macro list, which is deliberate — we don't want an unofficial feed silently
// gating live signals). Fetched with a cache + graceful fallback: if the feed is
// unreachable the calendar simply shows the curated events without this layer.
package econcal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const feedURL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"

// Event is one economic-data release.
type Event struct {
	Title       string
	Country     string
	Impact      string // High / Medium / Low / Holiday
	Forecast    string
	Previous    string
	DatetimeUTC time.Time
}

var (
	mu       sync.RWMutex
	cache    []Event
	loadedAt time.Time
	loadErr  error
)

type rawEvent struct {
	Title    string `json:"title"`
	Country  string `json:"country"`
	Date     string `json:"date"`
	Impact   string `json:"impact"`
	Forecast string `json:"forecast"`
	Previous string `json:"previous"`
}

// Load fetches + parses the feed and replaces the cache. Keeps only relevant
// releases (USD High/Medium, plus any High-impact globally) so the calendar stays
// signal-not-noise. Best-effort: on error the old cache is retained.
func Load(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "trading-bot/1.0")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		mu.Lock()
		loadErr = err
		mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	var raw []rawEvent
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		mu.Lock()
		loadErr = err
		mu.Unlock()
		return err
	}
	var out []Event
	for _, r := range raw {
		imp := strings.TrimSpace(r.Impact)
		high := strings.EqualFold(imp, "High")
		med := strings.EqualFold(imp, "Medium")
		keep := (r.Country == "USD" && (high || med)) || high
		if !keep {
			continue
		}
		t, perr := time.Parse(time.RFC3339, r.Date)
		if perr != nil {
			continue
		}
		out = append(out, Event{
			Title: r.Title, Country: r.Country, Impact: imp,
			Forecast: strings.TrimSpace(r.Forecast), Previous: strings.TrimSpace(r.Previous),
			DatetimeUTC: t.UTC(),
		})
	}
	mu.Lock()
	cache, loadedAt, loadErr = out, time.Now(), nil
	mu.Unlock()
	return nil
}

// All returns a copy of the cached events (empty if never loaded / feed down).
func All() []Event {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Event, len(cache))
	copy(out, cache)
	return out
}

// LoadAndWatch does an initial Load then refreshes every `every`. Errors are
// swallowed (logged by the caller-supplied logf) — the feed being down must never
// break /calendar.
func LoadAndWatch(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if err := Load(ctx); err != nil && logf != nil {
		logf("econcal: initial load: %v (calendar shows curated events only)", err)
	} else if logf != nil {
		logf("econcal: loaded %d data releases", len(All()))
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := Load(ctx); err != nil && logf != nil {
					logf("econcal: refresh: %v", err)
				}
			}
		}
	}()
}
