// Package earnings is a pure fundamental-data provider for company earnings
// dates. It is deliberately decoupled from the trading engine: it imports no
// signal/market/engine code and knows nothing about strategies, so it can
// serve two independent consumers —
//
//   A. the technical system — as an L0 "earnings blackout" gate scoped to a
//      symbol (ActiveAt), sitting next to the macro blackout in signal.Evaluate;
//   B. a standalone spot / buy-hold fundamental indicator — as event awareness
//      (NextUpcoming) across many symbols, with no engine/bias/backtest.
//
// Unlike the macro package (global events, embedded at compile time), earnings
// events are PER-SYMBOL and loaded at RUNTIME from a JSON file that a daily
// cron refreshes (see docs/fundamental_f3_earnings_spec.md). Times are UTC.
package earnings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Default blackout windows (minutes) applied when an event omits them (<=0),
// keyed by the "when" tag. amc (after market close) needs a long tail to cover
// the overnight gap the 24/7 synthetic prints; bmo (before market open) needs a
// long lead. The zero-tag fallback is symmetric and modest.
const (
	defAMCBefore = 120
	defAMCAfter  = 960 // 16h — covers the after-hours drop → next-session gap
	defBMOBefore = 720 // 12h — covers the pre-open lead-in
	defBMOAfter  = 240
	defBefore    = 120
	defAfter     = 240
)

// Event is one earnings release with its per-symbol blackout window.
type Event struct {
	Symbol        string    `json:"symbol"`        // bare ticker, upper-case (e.g. "SNDK")
	DatetimeUTC   time.Time `json:"datetime_utc"`  // release time (UTC)
	When          string    `json:"when"`          // "amc" | "bmo" | "" (unknown)
	BeforeMinutes int       `json:"blackout_before_min"`
	AfterMinutes  int       `json:"blackout_after_min"`
}

// Window returns the [start, end) of e's blackout span.
func (e Event) Window() (start, end time.Time) {
	start = e.DatetimeUTC.Add(-time.Duration(e.BeforeMinutes) * time.Minute)
	end = e.DatetimeUTC.Add(time.Duration(e.AfterMinutes) * time.Minute)
	return start, end
}

// Contains reports whether t is inside e's blackout window (inclusive start,
// exclusive end — matching macro's convention).
func (e Event) Contains(t time.Time) bool {
	start, end := e.Window()
	return !t.Before(start) && t.Before(end)
}

// applyDefaults fills a zero/negative window from the "when" tag so a sparse
// JSON entry still gets a real blackout rather than a zero-width one.
func (e *Event) applyDefaults() {
	if e.BeforeMinutes > 0 && e.AfterMinutes > 0 {
		return
	}
	b, a := defBefore, defAfter
	switch strings.ToLower(e.When) {
	case "amc":
		b, a = defAMCBefore, defAMCAfter
	case "bmo":
		b, a = defBMOBefore, defBMOAfter
	}
	if e.BeforeMinutes <= 0 {
		e.BeforeMinutes = b
	}
	if e.AfterMinutes <= 0 {
		e.AfterMinutes = a
	}
}

// Calendar is a hot-reloadable set of earnings events. Safe for concurrent use.
// Consumers hold one Calendar (or use the package Default) and query it by
// symbol; a background refresher swaps in new data via LoadFile/LoadJSON.
type Calendar struct {
	mu      sync.RWMutex
	events  []Event
	updated time.Time // "updated_utc" from the file (when the data was fetched)
	modTime time.Time // mtime of the last file loaded (for MaybeReload)
	path    string
}

// NewCalendar returns an empty Calendar. An empty calendar reports no
// blackouts and no upcoming events for every symbol — never panics.
func NewCalendar() *Calendar { return &Calendar{} }

type fileShape struct {
	UpdatedUTC string `json:"updated_utc"`
	Events     []struct {
		Symbol        string `json:"symbol"`
		DatetimeUTC   string `json:"datetime_utc"`
		When          string `json:"when"`
		BeforeMinutes int    `json:"blackout_before_min"`
		AfterMinutes  int    `json:"blackout_after_min"`
	} `json:"events"`
}

// LoadJSON parses the earnings JSON payload and atomically replaces the
// calendar's contents. Used directly by tests; LoadFile wraps it.
func (c *Calendar) LoadJSON(data []byte) error {
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("earnings: parse json: %w", err)
	}
	evts := make([]Event, 0, len(f.Events))
	for _, raw := range f.Events {
		sym := normalizeTicker(raw.Symbol)
		if sym == "" {
			return fmt.Errorf("earnings: event has empty/invalid symbol %q", raw.Symbol)
		}
		t, err := time.Parse(time.RFC3339, raw.DatetimeUTC)
		if err != nil {
			return fmt.Errorf("earnings: parse %q datetime %q: %w", raw.Symbol, raw.DatetimeUTC, err)
		}
		e := Event{
			Symbol:        sym,
			DatetimeUTC:   t.UTC(),
			When:          strings.ToLower(raw.When),
			BeforeMinutes: raw.BeforeMinutes,
			AfterMinutes:  raw.AfterMinutes,
		}
		e.applyDefaults()
		evts = append(evts, e)
	}
	// Stable order: by time then symbol — deterministic first-match in ActiveAt.
	sort.Slice(evts, func(i, j int) bool {
		if !evts[i].DatetimeUTC.Equal(evts[j].DatetimeUTC) {
			return evts[i].DatetimeUTC.Before(evts[j].DatetimeUTC)
		}
		return evts[i].Symbol < evts[j].Symbol
	})
	var updated time.Time
	if f.UpdatedUTC != "" {
		if u, err := time.Parse(time.RFC3339, f.UpdatedUTC); err == nil {
			updated = u.UTC()
		}
	}
	c.mu.Lock()
	c.events = evts
	c.updated = updated
	c.mu.Unlock()
	return nil
}

// LoadFile reads and parses path, replacing the calendar's contents. On any
// error the previous contents are left intact (stale-but-present beats empty,
// which would read as "no blackout ever").
func (c *Calendar) LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("earnings: read %s: %w", path, err)
	}
	if err := c.LoadJSON(data); err != nil {
		return err
	}
	if fi, statErr := os.Stat(path); statErr == nil {
		c.mu.Lock()
		c.modTime = fi.ModTime()
		c.path = path
		c.mu.Unlock()
	}
	return nil
}

// MaybeReload reloads path only if its mtime changed since the last load. Cheap
// to call on a timer from the daemon/web so a cron-written file is picked up
// without a restart. Returns (reloaded, err).
func (c *Calendar) MaybeReload(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("earnings: stat %s: %w", path, err)
	}
	c.mu.RLock()
	unchanged := c.path == path && fi.ModTime().Equal(c.modTime) && c.events != nil
	c.mu.RUnlock()
	if unchanged {
		return false, nil
	}
	if err := c.LoadFile(path); err != nil {
		return false, err
	}
	return true, nil
}

// ActiveAt returns the event whose blackout window contains t for the given
// symbol, or nil if none. sym may be a full synthetic ("NCSKSNDK2USD-USDT") or
// a bare ticker ("SNDK"); non-stock symbols (BTC/ETH/XAU/XAG and other
// synthetics) always return nil. This is consumer A's L0 gate.
func (c *Calendar) ActiveAt(sym string, t time.Time) *Event {
	tk := normalizeTicker(sym)
	if tk == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.events {
		if c.events[i].Symbol == tk && c.events[i].Contains(t) {
			e := c.events[i]
			return &e
		}
	}
	return nil
}

// NextUpcoming returns the soonest event for sym whose blackout window STARTS
// after t within lookahead, or nil. This is consumer B's event-awareness hook
// (e.g. "SNDK earnings in 3d") and consumer A's dashboard heads-up banner.
func (c *Calendar) NextUpcoming(sym string, t time.Time, lookahead time.Duration) *Event {
	tk := normalizeTicker(sym)
	if tk == "" {
		return nil
	}
	limit := t.Add(lookahead)
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best *Event
	var bestStart time.Time
	for i := range c.events {
		if c.events[i].Symbol != tk {
			continue
		}
		start, _ := c.events[i].Window()
		if start.After(t) && start.Before(limit) {
			if best == nil || start.Before(bestStart) {
				e := c.events[i]
				best = &e
				bestStart = start
			}
		}
	}
	return best
}

// UpdatedAt returns the "updated_utc" stamp from the loaded file (zero if
// unknown) — lets a consumer flag stale data.
func (c *Calendar) UpdatedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.updated
}

// Events returns a copy of all loaded events (windows already defaulted at
// load) — for display surfaces like the /calendar page.
func (c *Calendar) Events() []Event {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

// Symbols returns the distinct tickers present in the calendar, sorted.
func (c *Calendar) Symbols() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]struct{}{}
	out := []string{}
	for i := range c.events {
		if _, ok := seen[c.events[i].Symbol]; !ok {
			seen[c.events[i].Symbol] = struct{}{}
			out = append(out, c.events[i].Symbol)
		}
	}
	sort.Strings(out)
	return out
}

// Len reports the number of loaded events.
func (c *Calendar) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.events)
}

// --- symbol mapping (pure; both consumers use it) --------------------------

const (
	stockPrefix = "NCSK"
	stockSuffix = "2USD-USDT"
)

// IsStockSymbol reports whether sym is a US-stock synthetic perp
// (NCSK<TICKER>2USD-USDT). Metals/index/forex synthetics and BTC/ETH are not.
func IsStockSymbol(sym string) bool {
	s := strings.ToUpper(strings.TrimSpace(sym))
	return strings.HasPrefix(s, stockPrefix) && strings.HasSuffix(s, stockSuffix) &&
		len(s) > len(stockPrefix)+len(stockSuffix)
}

// UnderlyingTicker maps a stock-synthetic symbol to its bare ticker
// ("NCSKSNDK2USD-USDT" → "SNDK"); returns "" for non-stock synthetics.
func UnderlyingTicker(sym string) string {
	if !IsStockSymbol(sym) {
		return ""
	}
	s := strings.ToUpper(strings.TrimSpace(sym))
	return s[len(stockPrefix) : len(s)-len(stockSuffix)]
}

// normalizeTicker accepts either a full stock synthetic, a bare ticker, or a
// non-stock symbol, and returns the bare upper-case ticker or "" if the input
// is clearly not a company (a non-stock synthetic or a *-USDT pair like
// BTC-USDT). A plain word ("SNDK") is treated as a bare ticker so consumer B
// can query with tickers directly.
func normalizeTicker(sym string) string {
	s := strings.ToUpper(strings.TrimSpace(sym))
	if s == "" {
		return ""
	}
	if IsStockSymbol(s) {
		return UnderlyingTicker(s)
	}
	// Any other exchange-pair / synthetic form is not a company ticker.
	if strings.Contains(s, "-") || strings.HasSuffix(s, "USDT") {
		return ""
	}
	return s
}

// --- package-level default (app convenience) -------------------------------

var defaultCal = NewCalendar()

// Default returns the process-wide Calendar the app loads at startup and the
// refresher updates. Tests should construct their own via NewCalendar.
func Default() *Calendar { return defaultCal }

// --- app wiring: load Default + hot-reload watcher ------------------------

// Path returns the earnings.json location: EARNINGS_FILE if set, else the
// VPS default. Non-secret; safe to read from env.
func Path() string {
	if p := strings.TrimSpace(os.Getenv("EARNINGS_FILE")); p != "" {
		return p
	}
	return "/opt/trading/earnings.json"
}

// LoadDefaultAndWatch loads the package Default calendar from path once
// (best-effort: a missing/unreadable file leaves Default empty = the gate is a
// no-op, never fatal), then — if interval > 0 — spawns a goroutine that
// MaybeReloads on each tick until ctx is done, so a cron-rewritten file is
// picked up without a restart. logf may be nil.
func LoadDefaultAndWatch(ctx context.Context, path string, interval time.Duration, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := defaultCal.LoadFile(path); err != nil {
		logf("earnings: initial load %s: %v (gate inactive until file present)", path, err)
	} else {
		logf("earnings: loaded %d events from %s", defaultCal.Len(), path)
	}
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if reloaded, err := defaultCal.MaybeReload(path); err != nil {
					logf("earnings: reload %s: %v", path, err)
				} else if reloaded {
					logf("earnings: reloaded %d events from %s", defaultCal.Len(), path)
				}
			}
		}
	}()
}
