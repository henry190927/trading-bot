// Monitor heartbeat for /ops and /today.
//
// The monitor writes its own state (package heartbeat); this file only judges
// how fresh that statement is. It is the web service's ONLY view of the
// monitor: it replaced `systemctl is-active` and the inferred loop card
// (monitorLoops, which re-derived the monitor's gating from .env) after the
// three ran side by side from 2026-10-17 17:54 to 2026-10-18 — 103 samples
// ten minutes apart, 0 disagreements. Before that, testing the side-by-side
// had already caught the inferred card wrong twice.
//
// With this, the web service asks the host nothing at all, which is what lets
// it run in a container.
package main

import (
	"errors"
	"io/fs"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
)

type hbLoopView struct {
	Name     string `json:"name"`
	State    string `json:"state"` // run | idle | stalled | off
	EverySec int    `json:"every_sec,omitempty"`
	AgeSec   int    `json:"age_sec"` // seconds since last tick; -1 when it never ticked
	Reason   string `json:"reason,omitempty"`
}

type hbView struct {
	Present bool         `json:"present"` // a heartbeat file exists and parses
	Alive   bool         `json:"alive"`   // written within heartbeat.MaxAge
	AgeSec  int          `json:"age_sec"`
	PID     int          `json:"pid,omitempty"`
	Uptime  string       `json:"uptime,omitempty"`
	Error   string       `json:"error,omitempty"`
	Loops   []hbLoopView `json:"loops"`
}

// hbLoopState folds a loop's self-report into one display state. Stalled
// wins over idle: an idle loop still ticks every cycle, so one that stopped
// ticking is stuck regardless of what its config says.
func hbLoopState(l heartbeat.Loop, now time.Time) string {
	switch {
	case !l.Running:
		return "off"
	case l.Stalled(now):
		return "stalled"
	case l.Idle:
		return "idle"
	default:
		return "run"
	}
}

// buildHeartbeatView turns a read of the heartbeat file into what the page
// shows. Pure, so the rules are testable without a file.
func buildHeartbeatView(b heartbeat.Beat, readErr error, now time.Time) hbView {
	v := hbView{Loops: []hbLoopView{}}
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		v.Error = "沒有心跳檔 — monitor 沒在跑,或從未啟動"
		return v
	case readErr != nil:
		v.Error = readErr.Error()
		return v
	}
	v.Present = true
	v.Alive = b.Alive(now)
	v.AgeSec = int(now.Sub(b.WrittenAt) / time.Second)
	v.PID = b.PID
	if !b.StartedAt.IsZero() {
		v.Uptime = humanSince(now.Sub(b.StartedAt))
	}
	for _, name := range b.Names() {
		l := b.Loops[name]
		lv := hbLoopView{Name: name, State: hbLoopState(l, now), EverySec: l.EverySec, AgeSec: -1, Reason: l.Reason}
		if !l.LastTick.IsZero() {
			lv.AgeSec = int(now.Sub(l.LastTick) / time.Second)
		}
		v.Loops = append(v.Loops, lv)
	}
	return v
}

// readHeartbeatView reads the monitor's heartbeat file now.
func readHeartbeatView(now time.Time) hbView {
	b, err := heartbeat.Read(heartbeat.Path())
	return buildHeartbeatView(b, err, now)
}

// monitorLiveness maps the heartbeat onto the unit-state words the cards
// already use. "stale" is new: the file exists but is older than
// heartbeat.MaxAge, i.e. the monitor died or is wedged — systemd's "active"
// could never say the second half.
func monitorLiveness(v hbView) (active bool, state string) {
	switch {
	case !v.Present:
		return false, "unknown"
	case v.Alive:
		return true, "active"
	default:
		return false, "stale"
	}
}
