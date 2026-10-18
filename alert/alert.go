// Package alert turns "is something wrong right now?" into incidents: an
// alert fires, can be acknowledged, and resolves — each step recorded.
//
// It replaces a status page. A page answers only when someone looks; an
// incident says when it started, how long it lasted, and whether anyone saw
// it. The bracket guard stalling for four minutes at 03:00 is invisible on a
// page by 03:05, and is exactly what this keeps.
//
// The package knows nothing about the monitor, the exchange or the UI. A
// caller evaluates its rules on a cadence and hands the Engine the conditions
// that are true this round (Observe); the Engine owns everything after that:
//
//   - dedup: one incident per Key, however many rounds it stays true;
//   - hysteresis: a condition must hold FireAfter rounds to fire and be
//     absent ResolveAfter rounds to resolve, so a value hovering on a
//     threshold does not flap;
//   - persistence: every change is appended to a JSONL log, and the state is
//     rebuilt from it on start (event sourcing, at the size of one file).
//
// See docs/alerts.md for the rules the web service evaluates.
package alert

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Severity decides where an alert is delivered, not only how it is coloured.
type Severity string

const (
	Critical Severity = "critical" // money can be lost while this is true
	Warning  Severity = "warning"  // something is degraded; nothing is unprotected
	Info     Severity = "info"     // worth having in the history, not worth a badge
)

// Condition is one rule's verdict for this round: "this is wrong right now".
//
// Key is the dedup identity — "loop_stalled:bracket", not "bracket stalled
// for 132s". Detail may change every round (a growing age); Key must not.
type Condition struct {
	Key      string
	Severity Severity
	Title    string
	Detail   string
}

// Alert is one incident.
type Alert struct {
	ID       int64     `json:"id"`
	Key      string    `json:"key"`
	Severity Severity  `json:"severity"`
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	FiredAt  time.Time `json:"fired_at"`
	AckedAt  time.Time `json:"acked_at,omitzero"`
	// ResolvedAt is zero while the condition still holds. A notice (an Info
	// one-shot) is born resolved.
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// Open reports whether the condition behind the alert still holds.
func (a Alert) Open() bool { return a.ResolvedAt.IsZero() }

// Unread is what the bell counts: warning or critical, not yet acknowledged.
// Resolved alerts still count — an incident that started and ended while
// nobody was looking is the one most worth being told about.
func (a Alert) Unread() bool { return a.Severity != Info && a.AckedAt.IsZero() }

// Event is one line of the log: what happened, and the alert as it stood
// right after. Carrying the whole snapshot (not a diff) makes replay
// "last snapshot per ID wins", which cannot get the order of fields wrong.
type Event struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"` // fire | ack | resolve | notice
	Alert Alert     `json:"alert"`
}

// Change is handed to the OnChange hook after an alert fires or resolves —
// the place escalation (ntfy) attaches. Acks are not passed: nobody needs a
// push telling them they just pressed a button.
type Change struct {
	Kind  string // fire | resolve | notice
	Alert Alert
}

// Engine holds the incident state. Every method is safe for concurrent use.
type Engine struct {
	// FireAfter and ResolveAfter are consecutive rounds. Set before the first
	// Observe; Open defaults both to 2.
	FireAfter    int
	ResolveAfter int
	// OnChange, when set, is called after the state change is recorded,
	// outside the lock, so it may block (a network push) without stalling
	// readers.
	OnChange func(Change)
	// Logf receives write errors. A full disk must not stop alerting: the
	// state stays correct in memory and only durability is lost.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	path    string
	now     func() time.Time
	byID    map[int64]*Alert
	open    map[string]int64 // key -> id of its open alert
	hits    map[string]int   // consecutive rounds a not-yet-open key was true
	misses  map[string]int   // consecutive rounds an open key was false
	nextID  int64
	version int64
}

// Open loads the log at path (a missing file is an empty history) and
// returns an Engine that appends to it. Lines that do not parse are skipped
// and counted rather than failing the start: a damaged history must not
// leave the system without alerting.
func Open(path string) (e *Engine, skipped int, err error) {
	e = &Engine{
		FireAfter: 2, ResolveAfter: 2,
		path: path, now: time.Now,
		byID: map[int64]*Alert{}, open: map[string]int64{},
		hits: map[string]int{}, misses: map[string]int{},
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return e, 0, nil
	}
	if err != nil {
		return e, 0, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Alert.ID == 0 {
			skipped++
			continue
		}
		a := ev.Alert
		e.byID[a.ID] = &a
		if a.ID > e.nextID {
			e.nextID = a.ID
		}
	}
	for id, a := range e.byID {
		if a.Open() {
			e.open[a.Key] = id
		}
	}
	e.version = int64(len(e.byID))
	return e, skipped, sc.Err()
}

// Observe applies one evaluation round. firing is every condition that is
// true now; anything open and not in it counts toward resolving.
//
// held lists key prefixes the caller could not judge this round. The typical
// case is inhibition: when the monitor is down its loops cannot be stalled or
// healthy — they are unknown — so their counters must neither advance nor
// reset. Without this, a monitor outage would resolve every loop alert and
// then re-fire them all when it came back.
func (e *Engine) Observe(firing []Condition, held []string) {
	isHeld := func(key string) bool {
		for _, p := range held {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}

	var changes []Change
	e.mu.Lock()
	now := e.now()
	seen := make(map[string]bool, len(firing))
	for _, c := range firing {
		seen[c.Key] = true
		if id, ok := e.open[c.Key]; ok {
			delete(e.misses, c.Key)
			// The detail is refreshed in memory only. Logging it would write
			// a line every round for as long as the incident lasts.
			e.byID[id].Detail = c.Detail
			continue
		}
		e.hits[c.Key]++
		if e.hits[c.Key] < e.FireAfter {
			continue
		}
		delete(e.hits, c.Key)
		e.nextID++
		a := &Alert{ID: e.nextID, Key: c.Key, Severity: c.Severity, Title: c.Title, Detail: c.Detail, FiredAt: now}
		e.byID[a.ID] = a
		e.open[c.Key] = a.ID
		e.record(now, "fire", *a)
		changes = append(changes, Change{Kind: "fire", Alert: *a})
	}
	for key := range e.hits {
		if !seen[key] && !isHeld(key) {
			delete(e.hits, key)
		}
	}
	for key, id := range e.open {
		if seen[key] || isHeld(key) {
			continue
		}
		e.misses[key]++
		if e.misses[key] < e.ResolveAfter {
			continue
		}
		delete(e.misses, key)
		delete(e.open, key)
		a := e.byID[id]
		a.ResolvedAt = now
		e.record(now, "resolve", *a)
		changes = append(changes, Change{Kind: "resolve", Alert: *a})
	}
	e.mu.Unlock()
	e.emit(changes)
}

// Notice records a one-shot event — "the monitor restarted" — that has no
// condition to wait on. It is born resolved and skips hysteresis.
func (e *Engine) Notice(key string, sev Severity, title, detail string) {
	e.mu.Lock()
	now := e.now()
	e.nextID++
	a := &Alert{ID: e.nextID, Key: key, Severity: sev, Title: title, Detail: detail, FiredAt: now, ResolvedAt: now}
	e.byID[a.ID] = a
	e.record(now, "notice", *a)
	e.mu.Unlock()
	e.emit([]Change{{Kind: "notice", Alert: *a}})
}

// Ack marks alerts as seen; id 0 acks every unread one. Acknowledging does
// not resolve: an acked alert stays open until its condition clears, it just
// stops counting on the bell.
func (e *Engine) Ack(id int64) (n int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	ack := func(a *Alert) {
		if !a.Unread() {
			return
		}
		a.AckedAt = now
		e.record(now, "ack", *a)
		n++
	}
	if id != 0 {
		a, ok := e.byID[id]
		if !ok {
			return 0, fmt.Errorf("alert %d not found", id)
		}
		ack(a)
		return n, nil
	}
	for _, a := range e.byID {
		ack(a)
	}
	return n, nil
}

// Snapshot is what the UI reads.
type Snapshot struct {
	// Version changes whenever anything was recorded, so a poller can skip
	// re-rendering when it has not.
	Version      int64   `json:"version"`
	Unread       int     `json:"unread"`
	CriticalOpen int     `json:"critical_open"`
	Alerts       []Alert `json:"alerts"` // open first, then newest first
}

// Snapshot returns open alerts plus the newest `recent` closed ones.
func (e *Engine) Snapshot(recent int) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Version: e.version, Alerts: []Alert{}}
	var closed []Alert
	for _, a := range e.byID {
		if a.Unread() {
			s.Unread++
		}
		if a.Open() {
			if a.Severity == Critical {
				s.CriticalOpen++
			}
			s.Alerts = append(s.Alerts, *a)
		} else {
			closed = append(closed, *a)
		}
	}
	newestFirst := func(xs []Alert) {
		sort.Slice(xs, func(i, j int) bool { return xs[i].ID > xs[j].ID })
	}
	newestFirst(s.Alerts)
	newestFirst(closed)
	if len(closed) > recent {
		closed = closed[:recent]
	}
	s.Alerts = append(s.Alerts, closed...)
	return s
}

// record appends one event. Called with mu held.
func (e *Engine) record(at time.Time, kind string, a Alert) {
	e.version++
	if e.path == "" {
		return
	}
	data, err := json.Marshal(Event{At: at, Kind: kind, Alert: a})
	if err == nil {
		err = appendLine(e.path, data)
	}
	if err != nil && e.Logf != nil {
		e.Logf("alert: write %s: %v", e.path, err)
	}
}

func (e *Engine) emit(changes []Change) {
	if e.OnChange == nil {
		return
	}
	for _, c := range changes {
		e.OnChange(c)
	}
}

// appendLine writes one line with O_APPEND. A single write of a line this
// size is not torn by a concurrent appender on a local filesystem, and this
// process is the only writer anyway.
func appendLine(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close() // the write error is the one worth reporting
		return err
	}
	return f.Close()
}

// Path is the alert log (env ALERTS_PATH or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("ALERTS_PATH")); p != "" {
		return p
	}
	return "/opt/trading/alerts.jsonl"
}
