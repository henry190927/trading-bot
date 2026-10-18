package main

import (
	"encoding/json"
	"errors"
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
			"bracket": {Running: true, EverySec: 30, LastTick: ago(200 * time.Second)},
		},
	}
	v := buildHeartbeatView(b, nil, hbNow)

	if !v.Present || !v.Alive || v.AgeSec != 5 || v.PID != 4242 || v.Uptime != "2h 0m" || v.Error != "" {
		t.Fatalf("header = %+v", v)
	}
	want := []struct {
		name, state string
		age         int
	}{
		{"autoexec", "idle", 30},
		{"bracket", "stalled", 200},
		{"confluence", "off", -1},
		{"zonealert", "run", 10},
	}
	if len(v.Loops) != len(want) {
		t.Fatalf("loops = %d, want %d", len(v.Loops), len(want))
	}
	for i, w := range want {
		if l := v.Loops[i]; l.Name != w.name || l.State != w.state || l.AgeSec != w.age {
			t.Errorf("loop %d = {%s %s %d}, want {%s %s %d}", i, l.Name, l.State, l.AgeSec, w.name, w.state, w.age)
		}
	}
	if active, state := monitorLiveness(v); !active || state != "active" {
		t.Errorf("liveness = %v %q, want true active", active, state)
	}
}

// Older than heartbeat.MaxAge: the monitor is down or wedged, whatever its
// last write said about the loops.
func TestBuildHeartbeatViewStale(t *testing.T) {
	v := buildHeartbeatView(heartbeat.Beat{WrittenAt: ago(61 * time.Second)}, nil, hbNow)
	if !v.Present || v.Alive || v.AgeSec != 61 {
		t.Fatalf("present:%v alive:%v age:%d, want true false 61", v.Present, v.Alive, v.AgeSec)
	}
	if active, state := monitorLiveness(v); active || state != "stale" {
		t.Errorf("liveness = %v %q, want false stale", active, state)
	}
}

func TestBuildHeartbeatViewNoFile(t *testing.T) {
	err := fmt.Errorf("open monitor-heartbeat.json: %w", fs.ErrNotExist)
	v := buildHeartbeatView(heartbeat.Beat{}, err, hbNow)
	if v.Present || v.Alive || !strings.Contains(v.Error, "沒有心跳檔") {
		t.Fatalf("view = %+v", v)
	}
	if active, state := monitorLiveness(v); active || state != "unknown" {
		t.Errorf("liveness = %v %q, want false unknown", active, state)
	}
	// [] not null, so the page can iterate without a guard.
	if b, _ := json.Marshal(v.Loops); string(b) != "[]" {
		t.Errorf("loops json = %s, want []", b)
	}
}

func TestBuildHeartbeatViewUnreadable(t *testing.T) {
	v := buildHeartbeatView(heartbeat.Beat{}, errors.New("heartbeat: decode: unexpected EOF"), hbNow)
	if v.Present || v.Error != "heartbeat: decode: unexpected EOF" {
		t.Fatalf("view = %+v", v)
	}
}

func TestServiceRows(t *testing.T) {
	fresh := hbView{Present: true, Alive: true, Uptime: "3h 5m"}
	rows := serviceRows(fresh, hbNow)
	if len(rows) != 2 || rows[0].Unit != "trading-monitor" || !rows[0].Active || rows[0].Since != "3h 5m" {
		t.Fatalf("monitor row = %+v", rows[0])
	}
	if rows[1].Unit != "trading-web" || !rows[1].Active || rows[1].State != "active" {
		t.Fatalf("web row = %+v", rows[1])
	}
	// A dead monitor carries no uptime: "up 3h" next to STALE would be a lie.
	stale := hbView{Present: true, Alive: false, Uptime: "3h 5m"}
	if r := serviceRows(stale, hbNow)[0]; r.Active || r.State != "stale" || r.Since != "" {
		t.Fatalf("stale monitor row = %+v", r)
	}
}
