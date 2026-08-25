package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/autotrade"
)

// handleOpsAutotrade — GET /ops/autotrade. Read-only monitoring of the auto-executor:
// master state, per-symbol rules, and recent paper (or live) fires. The live-arm
// switches (paper=false + AUTOTRADE_ENABLED) stay env/config-side by design — no
// toggle here (real-money arming shouldn't be a fumble-able UI button).
func (s *server) handleOpsAutotrade(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	cfg := autotrade.Load()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	type fireRow struct {
		When              string
		Symbol, Side, Why string
		Entry, Stop, TP   float64
		Margin            float64
		Lev               int
		Live              bool
	}
	var fires []fireRow
	for _, f := range autotrade.ReadFires(60) {
		fires = append(fires, fireRow{
			When: f.Time.In(tpe).Format("01/02 15:04"), Symbol: f.Symbol, Side: f.Side, Why: f.Why,
			Entry: f.Entry, Stop: f.Stop, TP: f.TP, Margin: f.Margin, Lev: f.Lev, Live: f.Live,
		})
	}

	envArmed := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOTRADE_ENABLED")), "true")

	c.HTML(http.StatusOK, "autotrade.html", gin.H{
		"Enabled":      cfg.Enabled,
		"Paper":        cfg.Paper,
		"EnvArmed":     envArmed,
		"LiveArmed":    cfg.Enabled && !cfg.Paper && envArmed,
		"MaxConc":      cfg.MaxConcurrentTotal,
		"MaxMargin":    cfg.MaxMarginTotalUSDT,
		"DailyHaltR":   cfg.DailyLossHaltR,
		"Rules":        cfg.Rules,
		"Fires":        fires,
		"ConfigPath":   autotrade.Path(),
		"UpdatedUTC":   time.Now().UTC().Format("2006-01-02 15:04 UTC"),
	})
}
