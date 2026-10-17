// Service health + notification mute for /ops.
//
// WHY: the daemons' real state was invisible from the UI. Twice now a
// deploy-monitor restart left the monitor running with ntfy ON without the
// trader realising (→ push spam), and answering "is anything actually
// running right now?" meant SSHing in to read three systemctl units and
// grepping .env. This panel answers it from the phone.
//
// Two deliberate constraints:
//
//  1. Status reads use PLAIN systemctl, no sudo. `is-active` / `is-enabled` /
//     `show` are unprivileged D-Bus queries, so the health card works without
//     widening the scoped sudoers grant — which matters because trading-web is
//     NOT in that grant and could not otherwise be reported on.
//  2. Nothing here changes a unit. Service control is an SSH job
//     (scripts/ops); the mute toggle writes monitorcfg, which the monitor
//     re-reads on every send, and never touches .env.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/monitorcfg"
	"github.com/henry190927/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

// reportedServices are the units the health card covers, in display order.
// trading-web is included for visibility only (see constraint 2 above).
var reportedServices = []string{"trading-monitor", "trading-web"}

type serviceHealth struct {
	Unit     string `json:"unit"`
	Active   bool   `json:"active"`
	State    string `json:"state"`   // active | inactive | failed | unknown
	Enabled  string `json:"enabled"` // enabled | disabled | static | unknown
	Since    string `json:"since"`   // human uptime, "" when not running
	ReadOnly bool   `json:"read_only"`
}

// readServiceHealth queries one unit unprivileged. A failing systemctl (unit
// absent, dbus unavailable) degrades to state "unknown" rather than erroring
// the whole panel — one missing unit must not hide the other rows.
func readServiceHealth(unit string) serviceHealth {
	h := serviceHealth{Unit: unit, State: "unknown", Enabled: "unknown", ReadOnly: unit == "trading-web"}

	// Three systemd reads on a page-render path. Bounded together: without a
	// cap a wedged dbus hangs the panel with no upper limit, and 5s is orders
	// of magnitude above what these queries take.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output(); err == nil || len(out) > 0 {
		h.State = strings.TrimSpace(string(out))
		h.Active = h.State == "active"
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "is-enabled", unit).Output(); err == nil || len(out) > 0 {
		if s := strings.TrimSpace(string(out)); s != "" {
			h.Enabled = s
		}
	}
	// ActiveEnterTimestamp is when the unit last went active — uptime for a
	// running unit, and the thing that reveals "something restarted it".
	if h.Active {
		out, _ := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveEnterTimestamp", "--value", unit).Output()
		if ts := strings.TrimSpace(string(out)); ts != "" {
			// systemd format: "Mon 2026-09-01 17:45:02 CST"
			for _, layout := range []string{"Mon 2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 -0700"} {
				if t, err := time.Parse(layout, ts); err == nil {
					h.Since = humanSince(time.Since(t))
					break
				}
			}
		}
	}
	return h
}

// humanSince renders a duration the way the ops card reads best: coarse and
// short ("16h", "3d 2h"), because the useful question is "has this been up
// since I last looked", not the exact seconds.
func humanSince(d time.Duration) string {
	if d < 0 {
		return ""
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// monitorLoop describes one goroutine inside cmd/monitor and whether the
// current config actually lets it do anything.
type monitorLoop struct {
	Name   string `json:"name"`
	On     bool   `json:"on"`
	Reason string `json:"reason"`
}

// monitorLoops mirrors the gating in cmd/monitor/main.go exactly. Keep the two
// in sync: the whole value of this card is that it doesn't lie about what's
// running.
//
//	runZoneAlerts    — always started, but returns immediately if NTFY_TOPIC == ""
//	runAutoExecutor  — always started, ntfy only decorates it
//	runMacroWarn     — always started, ntfy-dependent only (NOT gated by
//	                   MONITOR_ZONE_ONLY: a monthly pre-blackout heads-up is
//	                   not confluence noise, and it used to vanish with it)
//	structalert / confluence scan — skipped entirely when MONITOR_ZONE_ONLY=1,
//	                   and ntfy-dependent otherwise
//	runBracketGuard  — always started, off only on MONITOR_BRACKET=0 or a
//	                   missing API key. NOT gated by MONITOR_ZONE_ONLY and NOT
//	                   gated by ntfy: without push it still logs and still
//	                   places, which is degraded but not off.
func monitorLoops(zoneOnly, ntfyOn, zoneChannelOn, bracketOn, apiKeyOn bool, bracketMode string) []monitorLoop {
	pushOff := "NTFY_TOPIC 空白 → 這個 loop 直接 return"
	zoneOnlyOff := "MONITOR_ZONE_ONLY=1 → 沒啟動"

	loops := []monitorLoop{
		{Name: "autoexec", On: true, Reason: "自動執行器(paper) — 不依賴 ntfy,靜音時照跑"},
	}

	// zonealert has TWO gates, and the first version of this card only knew
	// about one: NTFY_TOPIC decides whether the goroutine starts at all, but
	// zone-config.json's `enabled` is re-read every cycle and short-circuits
	// the body (`if !cfg.Enabled { continue }`). With the channel master off
	// the loop is up and doing nothing — which this card reported as "ON" for
	// a full day. Report both gates.
	za := monitorLoop{Name: "zonealert", On: ntfyOn && zoneChannelOn, Reason: "樞紐區/突破警報 · 25s 輪詢現價"}
	switch {
	case !ntfyOn:
		za.Reason = pushOff
	case !zoneChannelOn:
		za.Reason = "zone-config.json enabled=false → loop 每輪短路,不發警報"
	}
	loops = append(loops, za)

	for _, n := range []struct{ name, desc string }{
		{"confluence", "多 TF 匯合掃描 · 收盤觸發"},
		{"structalert", "結構事件(BOS/CHoCH)警報"},
	} {
		l := monitorLoop{Name: n.name, On: !zoneOnly && ntfyOn, Reason: n.desc}
		switch {
		case zoneOnly:
			l.Reason = zoneOnlyOff
		case !ntfyOn:
			l.Reason = pushOff
		}
		loops = append(loops, l)
	}

	// macrowarn is deliberately NOT in the loop above: it runs in zone-only
	// mode too (see cmd/monitor/main.go). Its only gate is push.
	mw := monitorLoop{Name: "macrowarn", On: ntfyOn, Reason: "宏觀事件前置提醒 · blackout 前 45 分"}
	if !ntfyOn {
		mw.Reason = pushOff
	}
	loops = append(loops, mw)

	// bracket, like macrowarn, is not gated by MONITOR_ZONE_ONLY. Unlike
	// every other loop here it is not gated by ntfy either: with push muted
	// it still logs, and in place mode it still attaches the stop. So "on"
	// here means the goroutine is doing its job, and a muted-push caveat
	// rides in the reason instead of flipping the badge.
	bg := monitorLoop{Name: "bracket", On: bracketOn && apiKeyOn}
	switch {
	case !bracketOn:
		bg.Reason = "MONITOR_BRACKET=0 → 沒啟動"
	case !apiKeyOn:
		bg.Reason = "BINGX_API_KEY/SECRET 未設 → 直接 return"
	case bracketMode == "place":
		bg.Reason = "裸倉守衛 · 30s · mode=place(自動掛 journal 停損)"
	default:
		bg.Reason = "裸倉守衛 · 30s · mode=alert(只推播,StopAuto 單才自動掛)"
	}
	if bg.On && !ntfyOn {
		bg.Reason += " · NTFY 靜音 → 只有 log"
	}
	loops = append(loops, bg)

	return loops
}

// ntfyState reports whether push is configured (NTFY_TOPIC present in .env,
// read-only — the topic value itself is never returned) and whether it is
// muted at runtime (monitorcfg). err is a config read error: in that case
// muted is false, matching what the monitor does (it fails open).
func ntfyState() (on, muted bool, err error) {
	if data, rerr := os.ReadFile(envPath()); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "NTFY_TOPIC=") {
				on = strings.TrimSpace(strings.TrimPrefix(line, "NTFY_TOPIC=")) != ""
			}
		}
	}
	cfg, err := monitorcfg.Read(monitorcfg.Path())
	return on, cfg.PushMuted, err
}

// handleOpsServices — GET /ops/services. The health card: unit states, what
// the monitor's goroutines are actually doing, and the push channel's state.
func (s *server) handleOpsServices(c *gin.Context) {
	svcs := make([]serviceHealth, 0, len(reportedServices))
	for _, u := range reportedServices {
		svcs = append(svcs, readServiceHealth(u))
	}
	on, muted, cfgErr := ntfyState()
	ntfy := gin.H{"on": on, "muted": muted}
	if cfgErr != nil {
		ntfy["error"] = cfgErr.Error()
	}
	zoneCfg := zone.ReadConfig()
	// Read from .env, not os.Getenv: this panel reports what the MONITOR
	// process will do on its next start, and that process reads the file.
	// The web process's own environment can differ.
	zoneOnly, bracketOn, bracketMode := false, true, "alert"
	if data, err := os.ReadFile(envPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "MONITOR_ZONE_ONLY="):
				zoneOnly = strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_ZONE_ONLY=")) == "1"
			case strings.HasPrefix(line, "MONITOR_BRACKET="):
				bracketOn = strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_BRACKET=")) != "0"
			case strings.HasPrefix(line, "MONITOR_BRACKET_MODE="):
				if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_BRACKET_MODE=")), "place") {
					bracketMode = "place"
				}
			}
		}
	}
	apiKeyOn := s.client != nil && s.client.APIKey != "" && s.client.APISecret != ""
	// A muted channel is, for every push-only loop, the same as no channel:
	// the loop runs but nothing it finds is sent.
	loops := monitorLoops(zoneOnly, on && !muted, zoneCfg.Enabled, bracketOn, apiKeyOn, bracketMode)
	monitorActive := false
	for _, h := range svcs {
		if h.Unit == "trading-monitor" {
			monitorActive = h.Active
		}
	}
	beat, beatErr := heartbeat.Read(heartbeat.Path())
	c.JSON(http.StatusOK, gin.H{
		"services":     svcs,
		"ntfy":         ntfy,
		"zone_only":    zoneOnly,
		"zone_channel": gin.H{"enabled": zoneCfg.Enabled, "auto": zoneCfg.Auto, "tfs": zoneCfg.TFs},
		"loops":        loops,
		// Self-reported loop state, compared against `loops` above for the
		// one-day side-by-side (see ops_heartbeat.go).
		"heartbeat": buildHeartbeatView(beat, beatErr, loops, monitorActive, time.Now()),
		// Price fan-out. drop_pct is the backpressure signal: broadcast
		// silently discards a stale pending snapshot for a slow client, which
		// is correct behaviour but was previously unobservable.
		"stream": s.hub.stats(),
		// Runtime macro overlay. Surfaced because an ad-hoc blackout you
		// cannot verify is indistinguishable from a typo in the file — and the
		// failure is silent in the dangerous direction ("no blackout").
		"macro_overlay": macro.OverlayStatus(),
	})
}

// handleOpsNtfy — POST /ops/ntfy?mute=1|0. Writes monitorcfg.PushMuted; the
// monitor re-reads it on every send, so nothing is restarted and .env is not
// touched. Muting keeps every loop running — only the push is skipped.
func (s *server) handleOpsNtfy(c *gin.Context) {
	mute := c.PostForm("mute") == "1"
	path := monitorcfg.Path()
	cfg, err := monitorcfg.Read(path)
	if err != nil {
		// Overwriting a file we could not parse would silently discard
		// whatever was in it. Make someone look at it first.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取 monitor-config 失敗,未改動: " + err.Error()})
		return
	}
	if cfg.PushMuted == mute {
		on, muted, _ := ntfyState()
		c.JSON(http.StatusOK, gin.H{"ok": true, "changed": false, "on": on, "muted": muted,
			"note": "已經是目標狀態,沒有改動"})
		return
	}
	cfg.PushMuted = mute
	if err := monitorcfg.Write(path, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "寫入 monitor-config 失敗: " + err.Error()})
		return
	}
	on, muted, _ := ntfyState()
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": true, "on": on, "muted": muted})
}
