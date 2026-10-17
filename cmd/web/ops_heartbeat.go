// Heartbeat view for the /ops health card.
//
// TEMPORARY SIDE-BY-SIDE. The monitor now writes a self-reported heartbeat
// (package heartbeat). For one day /ops shows it next to the existing
// inferred card (monitorLoops, which re-derives the monitor's gating from
// .env) and counts where the two disagree. Once they have agreed for a day,
// the inferred card and the systemctl status read are removed and this
// becomes the only source.
//
// A disagreement is not automatically a bug in either. monitorLoops reports
// what the monitor will do on its NEXT start (it reads .env); the heartbeat
// reports what the running process is doing NOW. An .env edit that has not
// been followed by a restart shows up here as a mismatch, which is exactly
// the kind of drift this view exists to surface.
package main

import (
	"errors"
	"io/fs"
	"sort"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
)

type hbLoopView struct {
	Name     string `json:"name"`
	State    string `json:"state"` // run | idle | stalled | off | missing
	EverySec int    `json:"every_sec,omitempty"`
	AgeSec   int    `json:"age_sec"` // seconds since last tick; -1 when it never ticked
	Reason   string `json:"reason,omitempty"`

	// Inferred is what monitorLoops says; nil for loops it does not cover.
	// Agree is nil when there is nothing to compare, or the heartbeat is
	// too old to be trusted.
	Inferred *bool `json:"inferred_on,omitempty"`
	Agree    *bool `json:"agree,omitempty"`
}

type hbView struct {
	Present bool   `json:"present"` // a heartbeat file exists and parses
	Alive   bool   `json:"alive"`   // written within heartbeat.MaxAge
	AgeSec  int    `json:"age_sec"`
	PID     int    `json:"pid,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Error   string `json:"error,omitempty"`

	// Service level: does systemd's view of the unit match the heartbeat's?
	SystemdActive bool `json:"systemd_active"`
	AgreeSystemd  bool `json:"agree_systemd"`

	Loops      []hbLoopView `json:"loops"`
	Mismatches int          `json:"mismatches"`
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

// buildHeartbeatView compares the monitor's heartbeat with the inferred loop
// card and with systemd. Pure, so the comparison rules are testable without
// a file or a unit.
func buildHeartbeatView(b heartbeat.Beat, readErr error, inferred []monitorLoop, systemdActive bool, now time.Time) hbView {
	v := hbView{SystemdActive: systemdActive, Loops: []hbLoopView{}}
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		v.Error = "沒有心跳檔 — monitor 還是舊版,或從未啟動"
	case readErr != nil:
		v.Error = readErr.Error()
	default:
		v.Present = true
		v.Alive = b.Alive(now)
		v.AgeSec = int(now.Sub(b.WrittenAt) / time.Second)
		v.PID = b.PID
		if !b.StartedAt.IsZero() {
			v.Uptime = humanSince(now.Sub(b.StartedAt))
		}
	}
	v.AgreeSystemd = v.Alive == systemdActive

	seen := map[string]bool{}
	add := func(name string, inf *bool) {
		seen[name] = true
		lv := hbLoopView{Name: name, State: "missing", AgeSec: -1, Inferred: inf}
		if l, ok := b.Loops[name]; ok && v.Present {
			lv.State = hbLoopState(l, now)
			lv.EverySec = l.EverySec
			lv.Reason = l.Reason
			if !l.LastTick.IsZero() {
				lv.AgeSec = int(now.Sub(l.LastTick) / time.Second)
			}
		}
		// Compare only when there is an opinion to compare against and the
		// heartbeat is fresh enough to stand behind its own answer.
		if inf != nil && v.Alive {
			agree := *inf == (lv.State == "run")
			lv.Agree = &agree
			if !agree {
				v.Mismatches++
			}
		}
		v.Loops = append(v.Loops, lv)
	}

	// The inferred card's order first, so the two columns line up with what
	// the page used to show; heartbeat-only loops after, alphabetically.
	for _, m := range inferred {
		on := m.On
		add(m.Name, &on)
	}
	extra := make([]string, 0, len(b.Loops))
	for name := range b.Loops {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		add(name, nil)
	}
	return v
}
