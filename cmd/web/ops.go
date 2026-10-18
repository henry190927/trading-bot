// Ops settings page: the runtime switches the trader flips from a phone —
// push mute, the zone-fade channel, the AI model — plus read-only account
// views. Tailscale-only + ufw + interface-bound socket.
//
// It used to be "Daemon control": live unit status, start/stop/restart and a
// journal tail, backed by a sudoers grant. All of that moved out — control to
// SSH (scripts/ops), health to in-app alerts (alerts.go) — so nothing here
// needs more than read/write on files in the data directory. .env is never
// written from the web.
package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

// envPath is the on-VPS path to the daemon .env file. Override via
// TRADING_ENV_PATH for local dev (where /opt/trading/.env doesn't exist).
func envPath() string {
	if p := os.Getenv("TRADING_ENV_PATH"); p != "" {
		return p
	}
	return "/opt/trading/.env"
}

// validTFs mirrors BingX's accepted interval set, restricted to values
// the strategy can sensibly run on.
var validTFs = []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"}

// handleOpsPage renders the settings page: push mute, account exposure, OI,
// the zone channel and the AI model. System health is not on it any more —
// that is the alert bell's job (alerts.go) — and service restarts and raw
// logs are an SSH job (scripts/ops), so the web service needs no host
// privileges for them.
func (s *server) handleOpsPage(c *gin.Context) {
	s.page(c, http.StatusOK, "ops.html", gin.H{
		"Zone":          zone.ReadConfig(),
		"ZoneTFList":    zone.ReadConfig().TFList(),
		"ZoneTFOptions": []string{"15m", "30m", "1h", "2h", "4h"},
	})
}

// handleOpsZones reports the zone-fade alert config (read from .env, so it
// matches what the monitor daemon actually uses) plus the currently-armed
// zones — auto ones derived live from AnalyzeStructure (same shared `zone`
// package the monitor fires from) plus any manual pins. Lets /ops show what
// the zone-alert channel is watching right now.
func (s *server) handleOpsZones(c *gin.Context) {
	cfg := zone.ReadConfig()
	ntfyOn, muted, _ := ntfyState() // presence only — never the topic value

	// "armed" must mean ARMED. runZoneAlerts skips any zone with an explicit
	// enabled:false, so listing those here overstated the channel — 13 rows
	// shown while 2 could fire. Retired zones are counted, not hidden.
	// A zone the daemon would refuse to evaluate (bad `confirm`) must not be
	// listed as plain ARMED either — zone.TriggerFault is the same check
	// runZoneAlerts uses, so the row carries the daemon's own verdict.
	type armedZone struct {
		zone.Zone
		Trigger string `json:"trigger"` // human label for the fire rule
		Fault   string `json:"fault,omitempty"`
	}
	wrap := func(z zone.Zone) armedZone {
		return armedZone{Zone: z, Trigger: zone.ConfirmWord(z.Confirm), Fault: zone.TriggerFault(z)}
	}
	armed := []armedZone{}
	var retired, faulted int
	for _, z := range zone.LoadManual() {
		if z.Enabled != nil && !*z.Enabled {
			retired++
			continue
		}
		a := wrap(z)
		if a.Fault != "" {
			faulted++
		}
		armed = append(armed, a)
	}
	if cfg.Enabled && cfg.Auto && s.client != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		defer cancel()
		for _, z := range zone.ComputeAuto(ctx, s.client, cfg.TFList()) {
			armed = append(armed, wrap(z))
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": cfg.Enabled,
		"auto":    cfg.Auto,
		"tfs":     cfg.TFs,
		"ntfy":    ntfyOn,
		"muted":   muted,
		"poll":    "25s",
		"armed":   armed,
		"retired": retired,
		"faulted": faulted,
	})
}

// handleOpsZoneConfig persists the zone-alert config. The monitor re-reads
// it each cycle, so changes apply live (no restart). Form sends enabled/auto
// as explicit "1"/"0" plus a tfs csv.
func (s *server) handleOpsZoneConfig(c *gin.Context) {
	// Merge with the current config — Start/Stop send only `enabled`, the
	// config form sends `auto` + `tfs[]`, so absent fields stay untouched.
	cfg := zone.ReadConfig()
	if v := c.PostForm("enabled"); v != "" {
		cfg.Enabled = v == "1"
	}
	if v := c.PostForm("auto"); v != "" {
		cfg.Auto = v == "1"
	}
	if tfArr := c.PostFormArray("tfs"); len(tfArr) > 0 {
		for _, p := range tfArr {
			if !contains(validTFs, strings.TrimSpace(p)) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tf: " + p})
				return
			}
		}
		cfg.TFs = strings.Join(tfArr, ",")
	}
	if strings.TrimSpace(cfg.TFs) == "" {
		cfg.TFs = "1h,2h"
	}
	if err := zone.WriteConfig(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "enabled": cfg.Enabled, "auto": cfg.Auto, "tfs": cfg.TFs})
}

// handleOpsZoneLogs returns the most recent zone-alert events from the
// monitor's event log (zone.EventsPath). It used to grep the monitor's
// journal, which needed the web process in the adm group and only ever saw
// the last few hundred journal lines.
func (s *server) handleOpsZoneLogs(c *gin.Context) {
	events, skipped, err := zone.RecentEvents(zone.EventsPath(), 40)
	if err != nil && !os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	tpe := time.FixedZone("Asia/Taipei", 8*3600)
	lines := make([]string, 0, len(events))
	for _, e := range events {
		lines = append(lines, e.At.In(tpe).Format("01-02 15:04:05")+"  "+e.Text)
	}
	c.JSON(http.StatusOK, gin.H{"lines": lines, "skipped": skipped})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
