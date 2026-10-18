package main

import (
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/alert"
	"github.com/henry190927/trading-bot/heartbeat"
	"github.com/henry190927/trading-bot/macro"
)

var alNow = time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)

// healthy is a fresh heartbeat with a bracket loop that ticked 10s ago and a
// live price feed. Each test breaks one thing.
func healthy() alertInput {
	return alertInput{
		now: alNow,
		beat: heartbeat.Beat{
			WrittenAt: alNow.Add(-5 * time.Second),
			StartedAt: alNow.Add(-time.Hour),
			Loops: map[string]heartbeat.Loop{
				"bracket":    {Running: true, EverySec: 30, LastTick: alNow.Add(-10 * time.Second)},
				"confluence": {Running: true, EverySec: 60, LastTick: alNow.Add(-20 * time.Second)},
			},
		},
		hub: hubStats{Running: true, WSUp: true, WSLastAgeS: 1},
	}
}

func keys(cs []alert.Condition) map[string]alert.Severity {
	m := map[string]alert.Severity{}
	for _, c := range cs {
		m[c.Key] = c.Severity
	}
	return m
}

func TestHealthyFiresNothing(t *testing.T) {
	firing, held := evaluateAlerts(healthy())
	if len(firing) != 0 || len(held) != 0 {
		t.Fatalf("firing=%v held=%v, want none", firing, held)
	}
}

// Stall threshold is max(3 × every, 2m): bracket every 30s → 2m (90s < 2m),
// confluence every 60s → 3m. At 150s bracket is stalled, at 150s confluence
// is not.
func TestStalledBracketIsCriticalOthersWarning(t *testing.T) {
	in := healthy()
	in.beat.Loops["bracket"] = heartbeat.Loop{Running: true, EverySec: 30, LastTick: alNow.Add(-150 * time.Second)}
	in.beat.Loops["confluence"] = heartbeat.Loop{Running: true, EverySec: 60, LastTick: alNow.Add(-150 * time.Second)}
	got := keys(must(evaluateAlerts(in)))
	if got["loop_stalled:bracket"] != alert.Critical {
		t.Errorf("bracket at 150s: %v, want critical", got)
	}
	if _, ok := got["loop_stalled:confluence"]; ok {
		t.Errorf("confluence at 150s (threshold 180s) should not fire: %v", got)
	}

	in.beat.Loops["confluence"] = heartbeat.Loop{Running: true, EverySec: 60, LastTick: alNow.Add(-181 * time.Second)}
	if got := keys(must(evaluateAlerts(in))); got["loop_stalled:confluence"] != alert.Warning {
		t.Errorf("confluence at 181s: %v, want warning", got)
	}
}

// An idle or switched-off loop is configuration, not a fault.
func TestOffLoopIsNotAnAlert(t *testing.T) {
	in := healthy()
	in.beat.Loops["autoexec"] = heartbeat.Loop{Running: false, Reason: "AUTOEXEC off"}
	if firing, _ := evaluateAlerts(in); len(firing) != 0 {
		t.Fatalf("off loop fired: %v", firing)
	}
}

// Monitor down: one critical, and the loops are held rather than judged —
// a stale file would otherwise report every loop as stalled.
func TestMonitorDownInhibitsLoops(t *testing.T) {
	for name, in := range map[string]alertInput{
		"stale":   func() alertInput { in := healthy(); in.beat.WrittenAt = alNow.Add(-61 * time.Second); return in }(),
		"missing": func() alertInput { in := healthy(); in.beat, in.beatErr = heartbeat.Beat{}, fs.ErrNotExist; return in }(),
		"corrupt": func() alertInput { in := healthy(); in.beatErr = fmt.Errorf("decode: unexpected EOF"); return in }(),
	} {
		// Every loop is long past its threshold too.
		for k, l := range in.beat.Loops {
			l.LastTick = alNow.Add(-time.Hour)
			in.beat.Loops[k] = l
		}
		firing, held := evaluateAlerts(in)
		got := keys(firing)
		if len(got) != 1 || got["monitor_down"] != alert.Critical {
			t.Errorf("%s: firing=%v, want only monitor_down critical", name, got)
		}
		if len(held) != 1 || held[0] != "loop_stalled:" {
			t.Errorf("%s: held=%v, want [loop_stalled:]", name, held)
		}
	}
}

func TestPriceFeed(t *testing.T) {
	in := healthy()
	in.hub = hubStats{Running: false, WSUp: false}
	firing, held := evaluateAlerts(in)
	if len(firing) != 0 || len(held) != 0 {
		t.Errorf("idle hub: firing=%v held=%v, want neither (no consumer, nothing to protect)", firing, held)
	}

	in.hub = hubStats{Running: true, WSUp: false, WSLastErr: "read: i/o timeout"}
	if got := keys(must(evaluateAlerts(in))); got["price_ws"] != alert.Warning {
		t.Errorf("ws down: %v", got)
	}

	in.hub = hubStats{Running: true, WSUp: true, WSLastAgeS: 31}
	if got := keys(must(evaluateAlerts(in))); got["price_ws"] != alert.Warning {
		t.Errorf("ws silent 31s: %v", got)
	}
	in.hub.WSLastAgeS = 30
	if got := keys(must(evaluateAlerts(in))); len(got) != 0 {
		t.Errorf("ws silent 30s is within bounds: %v", got)
	}
}

func TestMacroOverlay(t *testing.T) {
	in := healthy()
	in.overlay = macro.Status{Present: false}
	if firing, _ := evaluateAlerts(in); len(firing) != 0 {
		t.Errorf("absent overlay is normal: %v", firing)
	}
	in.overlay = macro.Status{Present: true, Skipped: 2, Problems: []string{"a", "b"}}
	if got := keys(must(evaluateAlerts(in))); got["macro_overlay"] != alert.Warning {
		t.Errorf("skipped entries: %v", got)
	}
}

// First sighting after the web starts is not a restart; a new started_at is.
// Three starts inside an hour trip the loop rule; the fourth an hour later
// sees only itself.
func TestRestartTracker(t *testing.T) {
	var rt restartTracker
	s0 := alNow.Add(-5 * time.Hour)
	if r, n := rt.observe(s0, alNow); r || n != 0 {
		t.Fatalf("first sighting: restarted=%v n=%d, want false 0", r, n)
	}
	if r, n := rt.observe(s0, alNow); r || n != 0 {
		t.Fatalf("same start: restarted=%v n=%d", r, n)
	}
	for i, want := range []int{1, 2, 3} {
		at := alNow.Add(time.Duration(i) * 10 * time.Minute)
		if r, n := rt.observe(at, at); !r || n != want {
			t.Fatalf("start %d: restarted=%v n=%d, want true %d", i+1, r, n, want)
		}
	}
	// Starts at +0, +10, +20. At +75m their ages are 75, 65, 55 minutes:
	// only the last is inside the hour.
	later := alNow.Add(75 * time.Minute)
	if r, n := rt.observe(alNow.Add(20*time.Minute), later); r || n != 1 {
		t.Fatalf("at +75m: restarted=%v n=%d, want false 1", r, n)
	}
}

func TestAlertPushes(t *testing.T) {
	crit := alert.Alert{Severity: alert.Critical, Key: "loop_stalled:bracket"}
	for _, tc := range []struct {
		c    alert.Change
		want bool
	}{
		{alert.Change{Kind: "fire", Alert: crit}, true},
		{alert.Change{Kind: "resolve", Alert: crit}, true},
		{alert.Change{Kind: "fire", Alert: alert.Alert{Severity: alert.Critical, Key: "monitor_down"}}, false},
		{alert.Change{Kind: "fire", Alert: alert.Alert{Severity: alert.Warning, Key: "price_ws"}}, false},
		{alert.Change{Kind: "notice", Alert: alert.Alert{Severity: alert.Info, Key: "web_started"}}, false},
	} {
		if got := alertPushes(tc.c); got != tc.want {
			t.Errorf("alertPushes(%s %s %s) = %v, want %v", tc.c.Kind, tc.c.Alert.Severity, tc.c.Alert.Key, got, tc.want)
		}
	}
}

func must(firing []alert.Condition, _ []string) []alert.Condition { return firing }
