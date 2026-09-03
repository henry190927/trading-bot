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
	Impact      string // High / Medium / Low / Holiday — as the FEED rates it
	Forecast    string
	Previous    string
	DatetimeUTC time.Time
	// Speaker marks a central-bank speech. Set independently of Impact,
	// because the feed's rating is unreliable for exactly these events (see
	// the note in Load) — the UI needs to be able to promote them past what
	// the feed claims.
	Speaker bool
}

// cbSpeakerMarkers are the phrases the feed uses for central-bank speech.
// Taken from live payloads rather than guessed: "FOMC Member Waller Speaks",
// "FOMC Member Hammack Speaks", "FOMC Member Goolsbee Speaks", "BOE Gov Bailey
// Speaks". Chair/President variants are included for the same reason they
// exist in the feed at all.
var cbSpeakerMarkers = []string{
	"fomc member", "fed chair", "fomc press conference",
	"boe gov", "ecb president", "boj gov", "snb chairman", "boc gov",
}

// IsCentralBankSpeaker reports whether a feed title is a central-bank speech.
//
// Matching on the TITLE rather than the impact rating is the point: the rating
// is what got these dropped. Deliberately narrow — it must not sweep in
// ordinary data releases, so it looks for the feed's speaker phrasing and not
// merely the word "Fed".
func IsCentralBankSpeaker(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return false
	}
	for _, m := range cbSpeakerMarkers {
		if strings.Contains(t, m) {
			return true
		}
	}
	// "... Speaks" / "... Testifies" covers named officials the marker list
	// does not enumerate, but only alongside a central-bank hint, so a
	// "Treasury Secretary Speaks" line does not silently become a Fed event.
	if strings.HasSuffix(t, " speaks") || strings.Contains(t, "testifies") {
		return strings.Contains(t, "fed") || strings.Contains(t, "fomc") ||
			strings.Contains(t, "ecb") || strings.Contains(t, "boe") ||
			strings.Contains(t, "boj") || strings.Contains(t, "central bank")
	}
	return false
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
		speaker := IsCentralBankSpeaker(r.Title)
		// Speaker events are kept regardless of the feed's impact rating.
		// ForexFactory calibrates "impact" for FX and tags "FOMC Member Waller
		// Speaks" as LOW — so on 2026-09-03 a Fed governor's remarks were
		// filtered out as noise while BTC put in a +1.8% hourly bar on 3-7x
		// volume. The rating is someone else's idea of what matters; for a
		// leveraged book, central-bank speech is not low impact.
		keep := (r.Country == "USD" && (high || med)) || high || speaker
		if !keep {
			continue
		}
		t, perr := time.Parse(time.RFC3339, r.Date)
		if perr != nil {
			continue
		}
		out = append(out, Event{
			Title: r.Title, Country: r.Country, Impact: imp, Speaker: speaker,
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
