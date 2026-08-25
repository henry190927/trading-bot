package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/market"
)

// handleOpsAutotrade — GET /ops/autotrade. Read-only monitoring of the auto-executor:
// master state, per-symbol rules, and recent paper (or live) fires with a replayed
// outcome (filled? → hit tp / stop / no-fill, and netR). The live-arm switches
// (paper=false + AUTOTRADE_ENABLED) stay env/config-side by design — no toggle here
// (real-money arming shouldn't be a fumble-able UI button).
func (s *server) handleOpsAutotrade(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	cfg := autotrade.Load()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	fires := autotrade.ReadFires(60)

	// Resolve each fire's timeframe: prefer the stored TF, else the matching
	// rule for that symbol, else 1h. Then fetch one kline set per (symbol,TF)
	// group and replay every fire in it — a handful of API calls, not one per fire.
	tfFor := func(f autotrade.PaperFire) string {
		if strings.TrimSpace(f.TF) != "" {
			return f.TF
		}
		for _, r := range cfg.Rules {
			if r.Symbol == f.Symbol && strings.TrimSpace(r.TF) != "" {
				return r.TF
			}
		}
		return "1h"
	}
	klineCache := map[string][]market.Candle{}
	candlesFor := func(sym, tf string) []market.Candle {
		key := sym + "|" + tf
		if cs, ok := klineCache[key]; ok {
			return cs
		}
		var cs []market.Candle
		if ms, err := resolveWebSymbol(sym); err == nil {
			if got, err := s.client.Klines(ctx, ms, market.Timeframe(tf), 500); err == nil {
				cs = got
			}
		}
		klineCache[key] = cs
		return cs
	}

	type fireRow struct {
		When              string
		Symbol, Side, Why string
		TF                string
		Entry, Stop, TP   float64
		Margin            float64
		Lev               int
		Live              bool
		Status            string
		StatusClass       string
		NetR              float64
		Resolved          bool // has a realized netR (tp/stop)
	}
	var rows []fireRow
	var outs []autotrade.Outcome
	for _, f := range fires {
		tf := tfFor(f)
		out := autotrade.EvaluateFire(f, candlesFor(f.Symbol, tf), 3)
		outs = append(outs, out)
		cls := map[autotrade.OutcomeStatus]string{
			autotrade.OutTP: "b-green", autotrade.OutStop: "b-red",
			autotrade.OutNoFill: "b-dim", autotrade.OutOpen: "b-yellow",
		}[out.Status]
		rows = append(rows, fireRow{
			When: f.Time.In(tpe).Format("01/02 15:04"), Symbol: f.Symbol, TF: tf, Side: f.Side, Why: f.Why,
			Entry: f.Entry, Stop: f.Stop, TP: f.TP, Margin: f.Margin, Lev: f.Lev, Live: f.Live,
			Status: string(out.Status), StatusClass: cls, NetR: out.NetR,
			Resolved: out.Status == autotrade.OutTP || out.Status == autotrade.OutStop,
		})
	}
	sum := autotrade.Summarize(outs)

	envArmed := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOTRADE_ENABLED")), "true")

	c.HTML(http.StatusOK, "autotrade.html", gin.H{
		"Enabled":    cfg.Enabled,
		"Paper":      cfg.Paper,
		"EnvArmed":   envArmed,
		"LiveArmed":  cfg.Enabled && !cfg.Paper && envArmed,
		"MaxConc":    cfg.MaxConcurrentTotal,
		"MaxMargin":  cfg.MaxMarginTotalUSDT,
		"DailyHaltR": cfg.DailyLossHaltR,
		"Rules":      cfg.Rules,
		"Fires":      rows,
		"Sum":        sum,
		"ConfigPath": autotrade.Path(),
		"UpdatedUTC": time.Now().UTC().Format("2006-01-02 15:04 UTC"),
	})
}
