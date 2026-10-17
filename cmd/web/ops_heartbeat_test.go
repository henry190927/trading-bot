package main

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
)

var hbNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return hbNow.Add(-d) }

func TestBuildHeartbeatViewFresh(t *testing.T) {
	b := heartbeat.Beat{
		PID:       4242,
		StartedAt: ago(2 * time.Hour),
		WrittenAt: ago(5 * time.Second),
		Loops: map[string]heartbeat.Loop{
			"zonealert":  {Running: true, EverySec: 25, LastTick: ago(10 * time.Second)},
			"autoexec":   {Running: true, Idle: true, EverySec: 60, LastTick: ago(30 * time.Second)},
			"confluence": {Running: false, Reason: "MONITOR_ZONE_ONLY=1"},
			// 3×30s = 90s is under the 2m floor, so the threshold is 120s.
			"bracket":  {Running: true, EverySec: 30, LastTick: ago(200 * time.Second)},
			"oisample": {Running: true, EverySec: 300, LastTick: ago(60 * time.Second)},
		},
	}
	inferred := []monitorLoop{
		{Name: "zonealert", On: true},
		{Name: "autoexec", On: true},
		{Name: "confluence", On: false},
		{Name: "bracket", On: true},
		{Name: "macrowarn", On: true}, // absent from the heartbeat
	}
	v := buildHeartbeatView(b, nil, inferred, true, hbNow)

	if !v.Present || !v.Alive || v.AgeSec != 5 || v.PID != 4242 || v.Uptime != "2h 0m" {
		t.Fatalf("header = present:%v alive:%v age:%d pid:%d uptime:%q", v.Present, v.Alive, v.AgeSec, v.PID, v.Uptime)
	}
	if !v.AgreeSystemd {
		t.Errorf("alive heartbeat + active unit should agree")
	}

	type want struct {
		state string
		age   int
		agree string // "y", "n", or "-" for nil
	}
	wants := []struct {
		name string
		want
	}{
		{"zonealert", want{"run", 10, "y"}},
		{"autoexec", want{"idle", 30, "n"}},
		{"confluence", want{"off", -1, "y"}},
		{"bracket", want{"stalled", 200, "n"}},
		{"macrowarn", want{"missing", -1, "n"}},
		{"oisample", want{"run", 60, "-"}}, // heartbeat-only: shown, not compared
	}
	if len(v.Loops) != len(wants) {
		t.Fatalf("loops = %d, want %d", len(v.Loops), len(wants))
	}
	for i, w := range wants {
		got := v.Loops[i]
		agree := "-"
		if got.Agree != nil {
			agree = map[bool]string{true: "y", false: "n"}[*got.Agree]
		}
		if got.Name != w.name || got.State != w.state || got.AgeSec != w.age || agree != w.agree {
			t.Errorf("loop %d = {%s %s age=%d agree=%s}, want {%s %s age=%d agree=%s}",
				i, got.Name, got.State, got.AgeSec, agree, w.name, w.state, w.age, w.agree)
		}
	}
	if v.Mismatches != 3 {
		t.Errorf("mismatches = %d, want 3 (autoexec, bracket, macrowarn)", v.Mismatches)
	}
}

// A heartbeat older than MaxAge cannot vouch for its loops, so nothing is
// compared — and it now disagrees with a unit systemd still calls active.
func TestBuildHeartbeatViewStale(t *testing.T) {
	b := heartbeat.Beat{
		WrittenAt: ago(61 * time.Second),
		Loops:     map[string]heartbeat.Loop{"zonealert": {Running: true, EverySec: 25, LastTick: ago(70 * time.Second)}},
	}
	v := buildHeartbeatView(b, nil, []monitorLoop{{Name: "zonealert", On: true}}, true, hbNow)
	if !v.Present || v.Alive || v.AgeSec != 61 {
		t.Fatalf("present:%v alive:%v age:%d, want true false 61", v.Present, v.Alive, v.AgeSec)
	}
	if v.AgreeSystemd {
		t.Errorf("stale heartbeat + active unit must be flagged as disagreeing")
	}
	if v.Loops[0].Agree != nil || v.Mismatches != 0 {
		t.Errorf("stale heartbeat compared anyway: agree=%v mismatches=%d", v.Loops[0].Agree, v.Mismatches)
	}
}

func TestBuildHeartbeatViewNoFile(t *testing.T) {
	err := fmt.Errorf("open monitor-heartbeat.json: %w", fs.ErrNotExist)
	v := buildHeartbeatView(heartbeat.Beat{}, err, []monitorLoop{{Name: "bracket", On: true}}, false, hbNow)
	if v.Present || v.Alive || !strings.Contains(v.Error, "沒有心跳檔") {
		t.Fatalf("present:%v alive:%v error:%q", v.Present, v.Alive, v.Error)
	}
	// Both sides say down: that is agreement, not an alarm.
	if !v.AgreeSystemd {
		t.Errorf("no heartbeat + inactive unit should agree")
	}
	if l := v.Loops[0]; l.State != "missing" || l.Agree != nil {
		t.Errorf("loop = %+v, want missing and uncompared", l)
	}
}
