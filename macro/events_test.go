package macro

import (
	"testing"
	"time"
)

// TestEventsLoad pins that the embedded events.json parses cleanly at
// startup. A typo or invalid RFC3339 timestamp would otherwise silently
// disable the blackout gate (LoadError != nil but Evaluate keeps going
// as if there are no events).
func TestEventsLoad(t *testing.T) {
	if err := LoadError(); err != nil {
		t.Fatalf("events.json failed to load: %v", err)
	}
	if len(All()) == 0 {
		t.Fatalf("events.json parsed to empty event list — file shouldn't be empty")
	}
}

// TestEventContainsAndWindow exercises the [-before, +after] window
// semantics: open at start, closed at end (so "the second the event
// ends" we're no longer in blackout — avoids the gate hanging on
// indefinitely if an event's `after` is too generous).
func TestEventContainsAndWindow(t *testing.T) {
	e := Event{
		Name:          "test",
		DatetimeUTC:   time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC),
		BeforeMinutes: 60,
		AfterMinutes:  60,
	}
	start, end := e.Window()
	if !start.Equal(time.Date(2026, 7, 15, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("start = %v, want 11:30", start)
	}
	if !end.Equal(time.Date(2026, 7, 15, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want 13:30", end)
	}
	if !e.Contains(start) {
		t.Fatalf("start instant should be inside window")
	}
	if e.Contains(end) {
		t.Fatalf("end instant should NOT be inside window (exclusive)")
	}
	if e.Contains(start.Add(-time.Second)) {
		t.Fatalf("1s before start shouldn't be in window")
	}
	if !e.Contains(e.DatetimeUTC) {
		t.Fatalf("event instant itself must be in blackout")
	}
}
