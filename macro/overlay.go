package macro

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

// Runtime overlay for the curated blackout list.
//
// macro/events.json is EMBEDDED, so every change to it needs a rebuild and a
// redeploy. That is the right default for a committed, reviewed calendar — but
// it is the wrong shape for the events that actually caught us out: a Fed
// governor's speech is announced days ahead, moves the tape hard (Waller,
// 2026-09-03, BTC +1.8% in the hour), and nobody wants to cut a release to
// blackout one evening.
//
// The overlay is a plain JSON file read at runtime, merged over the embedded
// list, and hot-reloaded on mtime like the earnings calendar (same pattern,
// deliberately — a second mechanism for "editable config file" would be one
// too many).
//
// Design rules, each of which exists because of a bug seen this month:
//
//   - **Fail safe.** A missing overlay is normal, not an error. A MALFORMED
//     overlay keeps the embedded list and reports the problem; it must never
//     be able to empty the blackout calendar, because "no blackouts" fails
//     silently in the dangerous direction.
//   - **No silent drops.** An unparseable entry is counted and surfaced, not
//     skipped quietly. A calendar that quietly ignored the line you just added
//     is worse than one that refuses it loudly.
//   - **Observable.** OverlayStatus() reports path / count / mtime / errors so
//     /ops and /calendar can prove the file was actually picked up. An overlay
//     you cannot verify is indistinguishable from a typo.
//   - **Cheap to read.** signal.Evaluate calls ActiveAt on EVERY evaluation
//     (backtests: thousands of times), so reads never touch the filesystem —
//     only the watcher goroutine does.

// DefaultOverlayPath sits next to the other runtime-editable config.
const DefaultOverlayPath = "/opt/trading/macro-overlay.json"

// OverlayPath honours MACRO_OVERLAY_PATH so tests and local runs can redirect.
func OverlayPath() string {
	if p := strings.TrimSpace(os.Getenv("MACRO_OVERLAY_PATH")); p != "" {
		return p
	}
	return DefaultOverlayPath
}

// OverlayEvent is one runtime entry. Same shape as the embedded events plus
// two controls.
type OverlayEvent struct {
	Name          string `json:"name"`
	DatetimeUTC   string `json:"datetime_utc"`
	BeforeMinutes int    `json:"blackout_before_min"`
	AfterMinutes  int    `json:"blackout_after_min"`
	Enabled       *bool  `json:"enabled,omitempty"` // nil = enabled
	// Suppress removes an EMBEDDED event by name instead of adding one, so a
	// wrong committed date can be neutralised without a redeploy.
	Suppress bool   `json:"suppress,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Status is the observability view of the overlay.
type Status struct {
	Path     string    `json:"path"`
	Present  bool      `json:"present"`
	Added    int       `json:"added"`
	Suppress int       `json:"suppressed"`
	Skipped  int       `json:"skipped"` // entries that failed to parse
	ModTime  time.Time `json:"mod_time"`
	Err      string    `json:"err,omitempty"`
	Problems []string  `json:"problems,omitempty"`
}

var (
	ovMu       sync.RWMutex
	ovEvents   []Event         // parsed, enabled, additive entries
	ovSuppress map[string]bool // lowercased embedded names to drop
	ovStatus   Status
	ovModTime  time.Time
	ovLoaded   bool
)

// LoadOverlay reads and replaces the overlay from path.
//
// A parse failure leaves the PREVIOUS overlay in place rather than clearing
// it: a half-saved file being written by an editor must not briefly disable
// every ad-hoc blackout.
func LoadOverlay(path string) error {
	st := Status{Path: path}

	fi, statErr := os.Stat(path)
	if statErr != nil {
		// Absent is the normal case, not a failure.
		ovMu.Lock()
		ovEvents, ovSuppress = nil, nil
		ovStatus = st
		ovModTime, ovLoaded = time.Time{}, true
		ovMu.Unlock()
		return nil
	}
	st.Present = true
	st.ModTime = fi.ModTime()

	raw, err := os.ReadFile(path)
	if err != nil {
		st.Err = err.Error()
		ovMu.Lock()
		ovStatus = st
		ovMu.Unlock()
		return fmt.Errorf("macro: read overlay %s: %w", path, err)
	}
	var wrap struct {
		Events []OverlayEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		// Try a bare array too — it is the shape people write by hand.
		if err2 := json.Unmarshal(raw, &wrap.Events); err2 != nil {
			st.Err = err.Error()
			ovMu.Lock()
			ovStatus = st
			ovMu.Unlock()
			return fmt.Errorf("macro: parse overlay %s: %w", path, err)
		}
	}

	var evts []Event
	sup := map[string]bool{}
	for i, oe := range wrap.Events {
		if oe.Enabled != nil && !*oe.Enabled {
			continue
		}
		if oe.Suppress {
			if n := strings.ToLower(strings.TrimSpace(oe.Name)); n != "" {
				sup[n] = true
				st.Suppress++
			} else {
				st.Skipped++
				st.Problems = append(st.Problems, fmt.Sprintf("entry %d: suppress needs a name", i))
			}
			continue
		}
		if strings.TrimSpace(oe.Name) == "" {
			st.Skipped++
			st.Problems = append(st.Problems, fmt.Sprintf("entry %d: missing name", i))
			continue
		}
		t, perr := parseOverlayTime(oe.DatetimeUTC)
		if perr != nil {
			st.Skipped++
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", oe.Name, perr))
			continue
		}
		before, after := oe.BeforeMinutes, oe.AfterMinutes
		if before <= 0 {
			before = 60
		}
		if after <= 0 {
			after = 60
		}
		evts = append(evts, Event{Name: oe.Name, DatetimeUTC: t, BeforeMinutes: before, AfterMinutes: after})
	}
	st.Added = len(evts)
	sort.Slice(evts, func(i, j int) bool { return evts[i].DatetimeUTC.Before(evts[j].DatetimeUTC) })

	ovMu.Lock()
	ovEvents, ovSuppress = evts, sup
	ovStatus = st
	ovModTime, ovLoaded = fi.ModTime(), true
	ovMu.Unlock()
	return nil
}

// parseOverlayTime accepts RFC3339 and a couple of hand-written forms, because
// this file is edited by a person. A bare form without a zone is read as UTC —
// stated here because guessing local time is how a blackout lands 8 hours off
// (which is exactly what a bare datetime did to the journal on 2026-09-03).
func parseOverlayTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("missing datetime_utc")
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad datetime_utc %q (want RFC3339, e.g. 2026-09-04T12:30:00Z)", s)
}

// MaybeReloadOverlay reloads only when the file's mtime moved. Returns whether
// anything was re-read.
func MaybeReloadOverlay(path string) (bool, error) {
	fi, err := os.Stat(path)
	ovMu.RLock()
	loaded, prev, prevPath := ovLoaded, ovModTime, ovStatus.Path
	ovMu.RUnlock()
	if err != nil {
		// Went away (or never existed). Only act on a transition.
		if loaded && prevPath == path && prev.IsZero() {
			return false, nil
		}
		return true, LoadOverlay(path)
	}
	if loaded && prevPath == path && fi.ModTime().Equal(prev) {
		return false, nil
	}
	return true, LoadOverlay(path)
}

// OverlayStatus reports what the overlay is doing right now.
func OverlayStatus() Status {
	ovMu.RLock()
	defer ovMu.RUnlock()
	return ovStatus
}

// overlaySnapshot returns the current additive events and suppression set.
func overlaySnapshot() ([]Event, map[string]bool) {
	ovMu.RLock()
	defer ovMu.RUnlock()
	return ovEvents, ovSuppress
}

// LoadOverlayAndWatch loads the overlay and hot-reloads it on mtime change.
// Mirrors earnings.LoadDefaultAndWatch, including the "log the initial state
// even when the file is absent" behaviour — a gate whose config never loaded
// should say so at startup rather than look like a gate with nothing to do.
func LoadOverlayAndWatch(ctx context.Context, path string, interval time.Duration, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := LoadOverlay(path); err != nil {
		logf("macro: overlay initial load %s: %v (embedded calendar still active)", path, err)
	} else if st := OverlayStatus(); st.Present {
		logf("macro: overlay loaded from %s — %d added, %d suppressed, %d skipped", path, st.Added, st.Suppress, st.Skipped)
		for _, p := range st.Problems {
			logf("macro: overlay problem — %s", p)
		}
	} else {
		logf("macro: no overlay at %s (embedded calendar only)", path)
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
				changed, err := MaybeReloadOverlay(path)
				if err != nil {
					logf("macro: overlay reload %s: %v (previous overlay retained)", path, err)
					continue
				}
				if !changed {
					continue
				}
				st := OverlayStatus()
				logf("macro: overlay reloaded — %d added, %d suppressed, %d skipped", st.Added, st.Suppress, st.Skipped)
				for _, p := range st.Problems {
					logf("macro: overlay problem — %s", p)
				}
			}
		}
	}()
}
