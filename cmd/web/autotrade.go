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
	// Live mark price per symbol (same source as the dashboard's open-position
	// cards) so an open trade's floating R and any tp/stop cross reflect the
	// current price, not the last closed candle. Falls back to last close.
	liveCache := map[string]float64{}
	liveFor := func(sym, tf string) float64 {
		if p, ok := liveCache[sym]; ok {
			return p
		}
		var p float64
		if ms, err := resolveWebSymbol(sym); err == nil {
			if fr, ferr := s.client.FundingRate(ctx, ms); ferr == nil && fr.MarkPrice > 0 {
				p = fr.MarkPrice
			}
		}
		if p == 0 { // fall back to the last closed candle
			if cs := candlesFor(sym, tf); len(cs) > 0 {
				p = cs[len(cs)-1].Close
			}
		}
		liveCache[sym] = p
		return p
	}

	type fireRow struct {
		When              string
		Symbol, Side, Why string
		TF                string
		Strategy          string
		Entry, Stop, TP   float64
		Score             float64 // validator /100 structural-fit at fire time (0 = legacy/unscored)
		Cur               float64 // live mark price
		CurUp             bool    // current price is on the profitable side of entry
		Margin            float64
		Lev               int
		Live              bool
		Status            string
		StatusClass       string
		NetR              float64
		UnrealR           float64
		Absorbed          int  // raw re-fires collapsed into this position
		Resolved          bool // has a realized netR (tp/stop)
		Open              bool // filled, still running — show unrealized R
	}

	// Normalise older records that predate the strategy field, then dedup the raw
	// fire log into realistic one-position-per-rule trades (a persistent setup that
	// re-fired every bar is ONE position, not N). fires come newest-first; DedupFires
	// wants oldest-first.
	oldest := make([]autotrade.PaperFire, len(fires))
	for i, f := range fires {
		if f.Strategy == "" {
			if strings.HasPrefix(f.Why, "engine") {
				f.Strategy = "engine"
			} else {
				f.Strategy = "range-edge"
			}
		}
		oldest[len(fires)-1-i] = f
	}
	// Cooldown is per-RULE, but the outcome replay below is one pass over all
	// fires, so it needs a single value. Taking rules[0] silently presented one
	// rule's setting as global — fine while every rule agrees, misleading the
	// moment one doesn't. Use the max (the most conservative dedup) and report
	// whether the rules actually agree so the panel can say so.
	cooldown, cooldownUniform := 6, true
	for i, r := range cfg.Rules {
		if r.CooldownBars <= 0 {
			continue
		}
		if i == 0 || r.CooldownBars == cooldown {
			cooldown = r.CooldownBars
			continue
		}
		cooldownUniform = false
		if r.CooldownBars > cooldown {
			cooldown = r.CooldownBars
		}
	}
	// Read the SAME depth the executor does (it uses 500) — the display list
	// above is capped at 60, and computing the book off that shorter slice
	// could truncate today's realized R and disagree with what gates a trade.
	book, unscored := autotrade.BuildBook(autotrade.ReadFires(500), candlesFor, time.Now().UTC())

	resolve := func(f autotrade.PaperFire) autotrade.Outcome {
		tf := tfFor(f)
		return autotrade.EvaluateFireLive(f, candlesFor(f.Symbol, tf), 6, liveFor(f.Symbol, tf))
	}
	positions := autotrade.DedupFires(oldest, 6, cooldown, time.Hour, resolve)

	var rows []fireRow
	var outs []autotrade.Outcome
	clsFor := map[autotrade.OutcomeStatus]string{
		autotrade.OutTP: "b-green", autotrade.OutStop: "b-red",
		autotrade.OutNoFill: "b-dim", autotrade.OutOpen: "b-yellow",
		autotrade.OutPending: "b-blue",
	}
	for i := len(positions) - 1; i >= 0; i-- { // newest-first for display
		p := positions[i]
		f, out := p.Fire, p.Outcome
		outs = append(outs, out)
		cur := liveFor(f.Symbol, tfFor(f))
		curUp := (f.Side == "long" && cur >= f.Entry) || (f.Side == "short" && cur <= f.Entry)
		rows = append(rows, fireRow{
			When: f.Time.In(tpe).Format("01/02 15:04"), Symbol: f.Symbol, TF: tfFor(f), Strategy: f.Strategy, Side: f.Side, Why: f.Why,
			Entry: f.Entry, Stop: f.Stop, TP: f.TP, Score: f.Score, Cur: cur, CurUp: curUp, Margin: f.Margin, Lev: f.Lev, Live: f.Live,
			Status: string(out.Status), StatusClass: clsFor[out.Status], NetR: out.NetR, UnrealR: out.UnrealR,
			Absorbed: p.Absorbed,
			Resolved: out.Status == autotrade.OutTP || out.Status == autotrade.OutStop,
			Open:     out.Status == autotrade.OutOpen,
		})
	}
	sum := autotrade.Summarize(outs)

	envArmed := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTOTRADE_ENABLED")), "true")

	c.HTML(http.StatusOK, "autotrade.html", gin.H{
		"Enabled":   cfg.Enabled,
		"Paper":     cfg.Paper,
		"EnvArmed":  envArmed,
		"LiveArmed": cfg.Enabled && !cfg.Paper && envArmed,
		// Computed with autotrade.BuildBook — the SAME function the executor
		// gates on — so the panel cannot drift from what actually blocks a
		// trade. candlesFor is already the panel's cached kline source.
		"Book":            book,
		"BookUnscored":    unscored,
		"CapsEnforced":    true,
		"Cooldown":        cooldown,
		"CooldownUniform": cooldownUniform,
		"MaxConc":         cfg.MaxConcurrentTotal,
		"MaxMargin":       cfg.MaxMarginTotalUSDT,
		"DailyHaltR":      cfg.DailyLossHaltR,
		"Rules":           cfg.Rules,
		"Fires":           rows,
		"Sum":             sum,
		"ConfigPath":      autotrade.Path(),
		"UpdatedUTC":      time.Now().In(tpe).Format("2006-01-02 15:04 UTC+8"),
	})
}
