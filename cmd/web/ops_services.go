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
//  2. trading-web is REPORT-ONLY. No start/stop/restart is offered for it: it
//     is the process serving this page, so stopping it from here would kill
//     the surface holding the button, and it's outside the sudoers scope
//     anyway. Only the mute toggle restarts a unit, and only trading-monitor,
//     which the existing scoped grant already covers.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

// reportedServices are the units the health card covers, in display order.
// trading-web is included for visibility only (see constraint 2 above).
var reportedServices = []string{"trading-monitor", "trading-web", "trading-bot"}

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
// the whole panel — a missing trading-bot must not hide the monitor's row.
func readServiceHealth(unit string) serviceHealth {
	h := serviceHealth{Unit: unit, State: "unknown", Enabled: "unknown", ReadOnly: unit == "trading-web"}

	if out, err := exec.Command("systemctl", "is-active", unit).Output(); err == nil || len(out) > 0 {
		h.State = strings.TrimSpace(string(out))
		h.Active = h.State == "active"
	}
	if out, err := exec.Command("systemctl", "is-enabled", unit).Output(); err == nil || len(out) > 0 {
		if s := strings.TrimSpace(string(out)); s != "" {
			h.Enabled = s
		}
	}
	// ActiveEnterTimestamp is when the unit last went active — uptime for a
	// running unit, and the thing that reveals "something restarted it".
	if h.Active {
		out, _ := exec.Command("systemctl", "show", "-p", "ActiveEnterTimestamp", "--value", unit).Output()
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
//	macrowarn / structalert / confluence scan — skipped entirely when
//	MONITOR_ZONE_ONLY=1, and ntfy-dependent otherwise
func monitorLoops(zoneOnly, ntfyOn, zoneChannelOn bool) []monitorLoop {
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
		{"macrowarn", "宏觀事件前置提醒"},
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
	return loops
}

// ntfyState reports whether push is live, and whether it's live-off because
// this panel muted it (a stashed NTFY_TOPIC_MUTED) versus never configured.
// The topic value itself is NEVER returned — presence only, same rule as
// handleOpsZones.
func ntfyState() (on, muted bool) {
	data, err := os.ReadFile(envPath())
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "NTFY_TOPIC="):
			on = strings.TrimSpace(strings.TrimPrefix(line, "NTFY_TOPIC=")) != ""
		case strings.HasPrefix(line, "NTFY_TOPIC_MUTED="):
			muted = strings.TrimSpace(strings.TrimPrefix(line, "NTFY_TOPIC_MUTED=")) != ""
		}
	}
	return on, muted && !on
}

// toggleNtfyEnv rewrites .env content to mute or unmute push notifications.
//
// Muting moves the topic to NTFY_TOPIC_MUTED and blanks NTFY_TOPIC — the
// monitor reads NTFY_TOPIC, so a blank value disables every push loop, which
// is exactly the manual workflow this replaces. Stashing the value in the SAME
// file (rather than a .env.bak alongside it) is the point: the old backup file
// could be clobbered by an unrelated edit or lost to a deploy, and then the
// topic was gone.
//
// Pure string→string so the rewrite is unit-testable without touching /opt.
// Returns changed=false when already in the requested state (idempotent).
func toggleNtfyEnv(content string, mute bool) (out string, changed bool, err error) {
	lines := strings.Split(content, "\n")
	var topic, stashed string
	topicIdx, stashIdx := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "NTFY_TOPIC="):
			topic, topicIdx = strings.TrimSpace(strings.TrimPrefix(l, "NTFY_TOPIC=")), i
		case strings.HasPrefix(l, "NTFY_TOPIC_MUTED="):
			stashed, stashIdx = strings.TrimSpace(strings.TrimPrefix(l, "NTFY_TOPIC_MUTED=")), i
		}
	}

	if mute {
		if topic == "" {
			return content, false, nil // already silent
		}
		lines[topicIdx] = "NTFY_TOPIC="
		if stashIdx >= 0 {
			lines[stashIdx] = "NTFY_TOPIC_MUTED=" + topic
		} else {
			// Keep the stash adjacent to the key it shadows so the file stays
			// readable when someone opens it over SSH.
			lines = append(lines[:topicIdx+1],
				append([]string{"NTFY_TOPIC_MUTED=" + topic}, lines[topicIdx+1:]...)...)
		}
		return strings.Join(lines, "\n"), true, nil
	}

	if topic != "" {
		return content, false, nil // already pushing
	}
	if stashed == "" {
		return content, false, fmt.Errorf("沒有可還原的 NTFY_TOPIC(沒有 stash) — 請手動設定 .env")
	}
	// The stash line always goes away: leaving it behind would let a later
	// mute resurrect a stale topic if the live one had since been hand-edited.
	// With no NTFY_TOPIC= line to restore into, the stash line simply becomes
	// it — which also keeps the key in its original position in the file.
	if topicIdx >= 0 {
		lines[topicIdx] = "NTFY_TOPIC=" + stashed
		lines = append(lines[:stashIdx], lines[stashIdx+1:]...)
	} else {
		lines[stashIdx] = "NTFY_TOPIC=" + stashed
	}
	return strings.Join(lines, "\n"), true, nil
}

// handleOpsServices — GET /ops/services. The health card: unit states, what
// the monitor's goroutines are actually doing, and the push channel's state.
func (s *server) handleOpsServices(c *gin.Context) {
	svcs := make([]serviceHealth, 0, len(reportedServices))
	for _, u := range reportedServices {
		svcs = append(svcs, readServiceHealth(u))
	}
	on, muted := ntfyState()
	zoneCfg := zone.ReadConfig()
	zoneOnly := false
	if data, err := os.ReadFile(envPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MONITOR_ZONE_ONLY=") {
				zoneOnly = strings.TrimSpace(strings.TrimPrefix(line, "MONITOR_ZONE_ONLY=")) == "1"
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"services":     svcs,
		"ntfy":         gin.H{"on": on, "muted": muted},
		"zone_only":    zoneOnly,
		"zone_channel": gin.H{"enabled": zoneCfg.Enabled, "auto": zoneCfg.Auto, "tfs": zoneCfg.TFs},
		"loops":        monitorLoops(zoneOnly, on, zoneCfg.Enabled),
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

// handleOpsNtfy — POST /ops/ntfy?mute=1|0. Rewrites .env then restarts
// trading-monitor so the change takes effect (the monitor reads NTFY_TOPIC
// once at startup when building its notifier sinks — unlike zone config,
// this one genuinely needs the restart).
func (s *server) handleOpsNtfy(c *gin.Context) {
	mute := c.PostForm("mute") == "1"

	data, err := os.ReadFile(envPath())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取 .env 失敗: " + err.Error()})
		return
	}
	out, changed, err := toggleNtfyEnv(string(data), mute)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !changed {
		on, muted := ntfyState()
		c.JSON(http.StatusOK, gin.H{"ok": true, "changed": false, "on": on, "muted": muted,
			"note": "已經是目標狀態,沒有改動"})
		return
	}
	// 0600: the file holds exchange + AI credentials. Preserve the mode
	// explicitly rather than inheriting whatever umask the service runs with.
	if err := os.WriteFile(envPath(), []byte(out), 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "寫入 .env 失敗: " + err.Error()})
		return
	}
	if err := systemctlAction("trading-monitor", "restart"); err != nil {
		// .env is already written, so report the partial state honestly —
		// the setting will apply on the monitor's next restart either way.
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": ".env 已更新,但重啟 trading-monitor 失敗: " + err.Error(),
		})
		return
	}
	on, muted := ntfyState()
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": true, "on": on, "muted": muted})
}
