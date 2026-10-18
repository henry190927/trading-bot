// Push mute for /ops.
//
// WHY: twice a deploy-monitor restart left the monitor running with ntfy ON
// without the trader realising (→ push spam), and turning it off meant SSHing
// in to edit .env. The toggle writes monitorcfg, which the monitor re-reads on
// every send: nothing restarts, and .env is never touched.
//
// This file used to be the "系統狀態" health card as well. Service and loop
// health are alerts now (alerts.go): the page only answered when looked at.
package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/henry190927/trading-bot/monitorcfg"
	"github.com/henry190927/trading-bot/zone"

	"github.com/gin-gonic/gin"
)

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

// handleOpsServices — GET /ops/services. The push channel's state for the
// mute toggle, and the zone channel's for its card.
func (s *server) handleOpsServices(c *gin.Context) {
	on, muted, cfgErr := ntfyState()
	ntfy := gin.H{"on": on, "muted": muted}
	if cfgErr != nil {
		ntfy["error"] = cfgErr.Error()
	}
	zoneCfg := zone.ReadConfig()
	c.JSON(http.StatusOK, gin.H{
		"ntfy":         ntfy,
		"zone_channel": gin.H{"enabled": zoneCfg.Enabled, "auto": zoneCfg.Auto, "tfs": zoneCfg.TFs},
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
