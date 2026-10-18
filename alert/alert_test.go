package alert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)

// newTest returns an engine on a temp log with a clock the test advances.
func newTest(t *testing.T) (*Engine, string, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	e, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	e.now = func() time.Time { return now }
	return e, path, &now
}

var bracket = Condition{Key: "loop_stalled:bracket", Severity: Critical, Title: "bracket stalled", Detail: "130s"}

func openKeys(s Snapshot) []string {
	var ks []string
	for _, a := range s.Alerts {
		if a.Open() {
			ks = append(ks, a.Key)
		}
	}
	return ks
}

// Round by round, FireAfter = ResolveAfter = 2:
//
//	r1 true  → hits 1, nothing open
//	r2 true  → hits 2 → FIRE
//	r3 true  → still the same one alert
//	r4 false → misses 1, still open
//	r5 false → misses 2 → RESOLVE
func TestFireAndResolveNeedTwoRounds(t *testing.T) {
	e, _, now := newTest(t)
	var changes []Change
	e.OnChange = func(c Change) { changes = append(changes, c) }

	e.Observe([]Condition{bracket}, nil)
	if got := openKeys(e.Snapshot(10)); len(got) != 0 {
		t.Fatalf("r1: fired after one round: %v", got)
	}
	*now = now.Add(15 * time.Second)
	e.Observe([]Condition{bracket}, nil)
	if got := openKeys(e.Snapshot(10)); len(got) != 1 {
		t.Fatalf("r2: want 1 open, got %v", got)
	}
	e.Observe([]Condition{bracket}, nil)
	if n := len(e.Snapshot(10).Alerts); n != 1 {
		t.Fatalf("r3: want still 1 alert (dedup), got %d", n)
	}
	e.Observe(nil, nil)
	if got := openKeys(e.Snapshot(10)); len(got) != 1 {
		t.Fatalf("r4: resolved after one clear round: %v", got)
	}
	e.Observe(nil, nil)
	s := e.Snapshot(10)
	if got := openKeys(s); len(got) != 0 {
		t.Fatalf("r5: want resolved, still open: %v", got)
	}
	if len(changes) != 2 || changes[0].Kind != "fire" || changes[1].Kind != "resolve" {
		t.Fatalf("changes = %+v, want fire then resolve", changes)
	}
	if !s.Alerts[0].FiredAt.Equal(t0.Add(15 * time.Second)) {
		t.Errorf("FiredAt = %v, want the round it fired in", s.Alerts[0].FiredAt)
	}
}

// true, false, true, false … never two in a row: a flapping condition must
// never fire. Without the reset in Observe the hits would accumulate.
func TestFlappingNeverFires(t *testing.T) {
	e, _, _ := newTest(t)
	for i := 0; i < 6; i++ {
		if i%2 == 0 {
			e.Observe([]Condition{bracket}, nil)
		} else {
			e.Observe(nil, nil)
		}
	}
	if n := len(e.Snapshot(10).Alerts); n != 0 {
		t.Fatalf("flapping condition fired %d alerts", n)
	}
}

// While the monitor is down its loops are unknown, not healthy. A held key
// keeps its open alert and its counters through the outage.
func TestHeldKeysNeitherResolveNorReset(t *testing.T) {
	e, _, _ := newTest(t)
	e.Observe([]Condition{bracket}, nil)
	e.Observe([]Condition{bracket}, nil) // fired
	held := []string{"loop_stalled:"}
	for i := 0; i < 5; i++ {
		e.Observe(nil, held)
	}
	if got := openKeys(e.Snapshot(10)); len(got) != 1 {
		t.Fatalf("held alert resolved during the outage: open=%v", got)
	}
	// Outage over, loop healthy: the normal two clear rounds resolve it.
	e.Observe(nil, nil)
	e.Observe(nil, nil)
	if got := openKeys(e.Snapshot(10)); len(got) != 0 {
		t.Fatalf("want resolved after the outage, open=%v", got)
	}
}

func TestAckKeepsAlertOpenButClearsUnread(t *testing.T) {
	e, _, _ := newTest(t)
	e.Observe([]Condition{bracket}, nil)
	e.Observe([]Condition{bracket}, nil)
	s := e.Snapshot(10)
	if s.Unread != 1 || s.CriticalOpen != 1 {
		t.Fatalf("before ack: unread=%d critical=%d, want 1/1", s.Unread, s.CriticalOpen)
	}
	if n, err := e.Ack(s.Alerts[0].ID); err != nil || n != 1 {
		t.Fatalf("Ack = %d, %v", n, err)
	}
	s = e.Snapshot(10)
	if s.Unread != 0 || s.CriticalOpen != 1 {
		t.Fatalf("after ack: unread=%d critical=%d, want 0/1 (ack is not resolve)", s.Unread, s.CriticalOpen)
	}
	if _, err := e.Ack(999); err == nil {
		t.Error("Ack of an unknown id should fail")
	}
}

// An incident that started and ended while nobody looked still counts.
func TestResolvedButUnackedStillUnread(t *testing.T) {
	e, _, _ := newTest(t)
	e.Observe([]Condition{bracket}, nil)
	e.Observe([]Condition{bracket}, nil)
	e.Observe(nil, nil)
	e.Observe(nil, nil)
	if s := e.Snapshot(10); s.Unread != 1 || s.CriticalOpen != 0 {
		t.Fatalf("unread=%d critical=%d, want 1/0", s.Unread, s.CriticalOpen)
	}
	if n, _ := e.Ack(0); n != 1 {
		t.Fatalf("ack all = %d, want 1", n)
	}
}

func TestNoticeIsHistoryNotBadge(t *testing.T) {
	e, _, _ := newTest(t)
	e.Notice("monitor_restarted", Info, "monitor restarted", "")
	s := e.Snapshot(10)
	if len(s.Alerts) != 1 || s.Alerts[0].Open() || s.Unread != 0 {
		t.Fatalf("notice: %+v unread=%d, want one resolved alert and no badge", s.Alerts, s.Unread)
	}
}

// The log is the state: a fresh Engine on the same file sees the open alert,
// the ack, and keeps numbering after the last id.
func TestReplayRebuildsState(t *testing.T) {
	e, path, _ := newTest(t)
	e.Observe([]Condition{bracket}, nil)
	e.Observe([]Condition{bracket}, nil) // id 1, open
	e.Notice("web_started", Info, "web started", "")
	e.Ack(1)

	// One damaged line must not cost the history.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{not json\n")
	f.Close()

	e2, skipped, err := Open(path)
	if err != nil || skipped != 1 {
		t.Fatalf("Open: skipped=%d err=%v, want 1/nil", skipped, err)
	}
	s := e2.Snapshot(10)
	if got := openKeys(s); len(got) != 1 || got[0] != bracket.Key {
		t.Fatalf("open after replay = %v", got)
	}
	if s.Unread != 0 {
		t.Errorf("ack lost on replay: unread=%d", s.Unread)
	}
	e2.Notice("x", Info, "x", "")
	for _, a := range e2.Snapshot(10).Alerts {
		if a.Key == "x" && a.ID != 3 {
			t.Errorf("new id after replay = %d, want 3 (ids 1 and 2 are in the log)", a.ID)
		}
	}
	// And the replayed open alert still resolves normally.
	e2.Observe(nil, nil)
	e2.Observe(nil, nil)
	if got := openKeys(e2.Snapshot(10)); len(got) != 0 {
		t.Fatalf("replayed alert did not resolve: %v", got)
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), `"kind":"resolve"`); n != 1 {
		t.Errorf("resolve lines = %d, want 1", n)
	}
}
