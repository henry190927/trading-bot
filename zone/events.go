package zone

// Zone-alert events: the daemon's own record of what the zone channel did —
// fired, skipped a misconfigured zone, failed to push — for /ops to show.
//
// This replaces /ops grepping the monitor's journal for "zonealert" lines.
// The journal is the platform's log; this file is the product's record. It
// does not depend on the web process being allowed to read the host journal
// (it will not be, once the services run in containers), and it is not
// limited to however many zone lines happen to sit in the last few hundred
// journal lines.

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// Event kinds.
const (
	EventFired      = "fired"
	EventSkip       = "skip"
	EventPushFailed = "push_failed"
)

// Event is one line of the zone event log.
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Symbol string    `json:"symbol,omitempty"`
	TF     string    `json:"tf,omitempty"`
	Text   string    `json:"text"`
}

// EventsPath is the event log (env ZONE_EVENTS or default).
func EventsPath() string {
	if p := strings.TrimSpace(os.Getenv("ZONE_EVENTS")); p != "" {
		return p
	}
	return "/opt/trading/zone-events.jsonl"
}

// AppendEvent adds one event as a JSON line. The monitor is the only writer,
// and an O_APPEND write of one short line lands whole.
func AppendEvent(path string, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close() // the write error is the one worth returning
		return err
	}
	return f.Close()
}

// RecentEvents returns the last n events by time, oldest first.
//
// It reads the whole file and sorts by At rather than trusting line order,
// so a one-off backfill can be appended after live events without
// scrambling the view. That is affordable because the channel fires a few
// times a day; if it ever fires thousands, this wants a tail read instead.
//
// A line that does not parse is skipped and counted, never silently: the
// caller decides whether to surface it.
func RecentEvents(path string, n int) (events []Event, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil || e.At.IsZero() {
			skipped++
			continue
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	if n > 0 && len(events) > n {
		events = events[len(events)-n:]
	}
	return events, skipped, nil
}
