// Package macro provides a blackout-window gate so the signal engine
// can skip generating trade plans around known macro releases (CPI,
// FOMC, NFP, PPI). The motivation traces to a live XAG short
// stopped on a CPI-driven wick before mark price reverted past TP2):
// signals fired in the predicted direction were correct, but the path
// got dominated by macro-event volatility we hadn't accounted for.
//
// The event list is committed at macro/events.json and embedded into
// the binary. Update the JSON + redeploy to refresh the calendar; no
// runtime data source / scraping. Times are UTC; blackout_before_min
// and blackout_after_min carve a window around the event time during
// which Evaluate returns Flat with a "macro blackout" reason instead
// of a real plan.
package macro

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed events.json
var eventsJSON []byte

// Event is one macro release with its blackout window.
type Event struct {
	Name          string    `json:"name"`
	DatetimeUTC   time.Time `json:"datetime_utc"`
	BeforeMinutes int       `json:"blackout_before_min"`
	AfterMinutes  int       `json:"blackout_after_min"`
}

// Window returns the [start, end] of e's blackout span.
func (e Event) Window() (time.Time, time.Time) {
	start := e.DatetimeUTC.Add(-time.Duration(e.BeforeMinutes) * time.Minute)
	end := e.DatetimeUTC.Add(time.Duration(e.AfterMinutes) * time.Minute)
	return start, end
}

// Contains reports whether t falls inside e's blackout window
// (inclusive start, exclusive end — matching the way "active" events
// are conceptually "now").
func (e Event) Contains(t time.Time) bool {
	start, end := e.Window()
	return !t.Before(start) && t.Before(end)
}

var (
	loadOnce sync.Once
	allEvts  []Event
	loadErr  error
)

func loadEvents() {
	loadOnce.Do(func() {
		var wrap struct {
			Events []struct {
				Name          string `json:"name"`
				DatetimeUTC   string `json:"datetime_utc"`
				BeforeMinutes int    `json:"blackout_before_min"`
				AfterMinutes  int    `json:"blackout_after_min"`
			} `json:"events"`
		}
		if err := json.Unmarshal(eventsJSON, &wrap); err != nil {
			loadErr = fmt.Errorf("macro: parse events.json: %w", err)
			return
		}
		for _, raw := range wrap.Events {
			t, err := time.Parse(time.RFC3339, raw.DatetimeUTC)
			if err != nil {
				loadErr = fmt.Errorf("macro: parse %q datetime %q: %w", raw.Name, raw.DatetimeUTC, err)
				return
			}
			// Sensible defaults so a JSON entry missing the windows
			// still gets a reasonable blackout instead of zero (which
			// would treat the event as having no blackout at all).
			before := raw.BeforeMinutes
			if before <= 0 {
				before = 60
			}
			after := raw.AfterMinutes
			if after <= 0 {
				after = 60
			}
			allEvts = append(allEvts, Event{
				Name:          raw.Name,
				DatetimeUTC:   t,
				BeforeMinutes: before,
				AfterMinutes:  after,
			})
		}
		// All() documents sorted order; the file happens to be in order today,
		// which is not the same as guaranteeing it.
		sort.Slice(allEvts, func(i, j int) bool {
			return allEvts[i].DatetimeUTC.Before(allEvts[j].DatetimeUTC)
		})
	})
}

// All returns the effective event list: the embedded calendar merged with the
// runtime overlay (see overlay.go), sorted by datetime.
//
// Merging HERE rather than at each call site is deliberate — there are ten
// consumers across the engine, the monitor, /calendar, /today and the MCP
// server, and an overlay that only some of them honoured would be worse than
// no overlay at all.
//
// The returned slice is freshly built, so callers may not assume identity
// across calls; nobody did, and pinning that down is cheaper than a copy-on-
// write scheme for a list this small.
func All() []Event {
	loadEvents()
	add, sup := overlaySnapshot()
	if len(add) == 0 && len(sup) == 0 {
		return allEvts
	}
	out := make([]Event, 0, len(allEvts)+len(add))
	for _, e := range allEvts {
		if sup[strings.ToLower(strings.TrimSpace(e.Name))] {
			continue
		}
		out = append(out, e)
	}
	out = append(out, add...)
	sort.Slice(out, func(i, j int) bool { return out[i].DatetimeUTC.Before(out[j].DatetimeUTC) })
	return out
}

// Embedded returns ONLY the committed calendar, ignoring the overlay. Used by
// /calendar to show which entries are permanent and which are ad-hoc, so an
// overlay entry cannot be mistaken for a reviewed one.
func Embedded() []Event {
	loadEvents()
	return allEvts
}

// LoadError returns any error encountered when parsing events.json
// at startup. Callers can use this to surface a config issue
// instead of silently behaving as "no blackouts ever".
func LoadError() error {
	loadEvents()
	return loadErr
}

// ActiveAt returns the first event whose blackout window contains t,
// or nil if t is not currently in any blackout. Events are checked
// in file-order; since real-world blackouts shouldn't overlap, the
// first match is fine.
func ActiveAt(t time.Time) *Event {
	evts := All()
	for i := range evts {
		if evts[i].Contains(t) {
			return &evts[i]
		}
	}
	return nil
}

// NextUpcoming returns the soonest event whose blackout window STARTS
// after t, within `lookahead`. Used by the dashboard banner to give a
// heads-up before blackout begins (e.g. "CPI in 2h 14min — blackout
// starts in 1h 14min"). Returns nil if nothing in lookahead.
func NextUpcoming(t time.Time, lookahead time.Duration) *Event {
	evts := All()
	var best *Event
	for i := range evts {
		start, _ := evts[i].Window()
		if start.After(t) && start.Before(t.Add(lookahead)) {
			if best == nil || start.Before(best.DatetimeUTC.Add(-time.Duration(best.BeforeMinutes)*time.Minute)) {
				best = &evts[i]
			}
		}
	}
	return best
}
