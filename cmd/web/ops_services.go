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
//  1. Nothing here asks the host. The monitor's state comes from its own
//     heartbeat file (ops_heartbeat.go), and the web service reports itself:
//     it is the process answering. This used to shell out to systemctl.
//  2. Nothing here changes a unit. Service control is an SSH job
//     (scripts/ops); the mute toggle writes monitorcfg, which the monitor
//     re-reads on every send, and never touches .env.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/monitorcfg"
	"github.com/henry190927/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

// webStarted is this process's start, for the web row's uptime.
var webStarted = time.Now()

type serviceHealth struct {
	Unit   string `json:"unit"`
	Active bool   `json:"active"`
	State  string `json:"state"` // active | stale | unknown
	Since  string `json:"since"` // human uptime, "" when not running
}

// serviceRows is the health card's two rows. The monitor's comes from its
// heartbeat; the web's is trivially active, since it is serving this request.
// Whether a unit is enabled at boot is a platform question now — `ops status`.
func serviceRows(hb hbView, now time.Time) []serviceHealth {
	mon := serviceHealth{Unit: "trading-monitor"}
	mon.Active, mon.State = monitorLiveness(hb)
	if mon.Active {
		mon.Since = hb.Uptime
	}
	web := serviceHealth{Unit: "trading-web", Active: true, State: "active", Since: humanSince(now.Sub(webStarted))}
	return []serviceHealth{mon, web}
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
	now := time.Now()
	hb := readHeartbeatView(now)
	on, muted, cfgErr := ntfyState()
	ntfy := gin.H{"on": on, "muted": muted}
	if cfgErr != nil {
		ntfy["error"] = cfgErr.Error()
	}
	zoneCfg := zone.ReadConfig()
	c.JSON(http.StatusOK, gin.H{
		"services":     serviceRows(hb, now),
		"ntfy":         ntfy,
		"zone_channel": gin.H{"enabled": zoneCfg.Enabled, "auto": zoneCfg.Auto, "tfs": zoneCfg.TFs},
		// What the monitor's loops say they are doing (ops_heartbeat.go).
		"heartbeat": hb,
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
