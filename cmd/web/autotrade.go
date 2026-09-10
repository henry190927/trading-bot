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
// autotradeLogDepth is how far back every number on /ops/autotrade reads —
// the same depth cmd/monitor's executor uses. autotradeDisplayRows caps the
// fires TABLE only; it must never bound the arithmetic.
const (
	autotradeLogDepth    = 500
	autotradeDisplayRows = 60
)

func (s *server) handleOpsAutotrade(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	cfg := autotrade.Load()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	// ONE read, at the depth the executor uses. This was ReadFires(60) — the
	// display table's row cap — and `sum` was derived from it while the equity
	// curve, the R histogram, the calendar and BuildBook all read 500. Same page,
	// same second, two different answers: measured 2026-09-09 the summary said
	// +4.58R over 50 closed trades while the portfolio views said +13.01R over
	// 79, because the log had grown to 92 fires. The row cap is a UI choice and
	// belongs at render time, not in the arithmetic.
	fires := autotrade.ReadFires(autotradeLogDepth)

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
	oldest := reverseNormalised(fires)
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
	// Same slice the summary and the portfolio views use — the executor's depth.
	// Computing the book off a shorter slice could truncate today's realized R
	// and disagree with what actually gates a trade.
	book, unscored := autotrade.BuildBook(fires, candlesFor, time.Now().UTC(), cooldown)

	resolve := func(f autotrade.PaperFire) autotrade.Outcome {
		tf := tfFor(f)
		return autotrade.EvaluateFireLive(f, candlesFor(f.Symbol, tf), 6, liveFor(f.Symbol, tf))
	}
	// ONE dedup pass feeds every number on the page. There used to be two — a
	// 60-fire pass for the table and summary and a 500-fire pass for the
	// portfolio views — and dedup is history-dependent (a dropped fire frees
	// the slot and resets the cooldown), so the two passes disagreed about the
	// same recent fires, not just about how many they covered.
	positions := autotrade.DedupFires(oldest, 6, cooldown, time.Hour, resolve)
	statTrades, statUnscoreable := rTradesFromPositions(positions)
	statOpenR, statOpenN := openUnrealR(positions)

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
		// Every position counts toward the summary; only the newest
		// autotradeDisplayRows of them become table rows.
		outs = append(outs, out)
		if len(rows) >= autotradeDisplayRows {
			continue
		}
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
		// Shown even when 0 (= unlimited/off). A cap that gates real trades
		// while being invisible on the panel that claims to show the caps is
		// the display-vs-reality shape this file has already been fixed for
		// once this week.
		"MaxSameSide": cfg.MaxSameSymbolSide,
		"Rules":       cfg.Rules,
		"Fires":       rows,
		"Sum":         sum,
		"Equity":      buildEquityCurve(statTrades),
		"Histogram":   buildRHistogram(statTrades),
		"Calendar":    buildDailyCalendar(statTrades, 42),
		"StatN":       len(statTrades),
		// perfPanels labels its distribution panel with .ClosedCount.
		"ClosedCount":     len(statTrades),
		"StatUnscoreable": statUnscoreable,
		"StatOpenR":       statOpenR,
		"StatOpenN":       statOpenN,
		"ConfigPath":      autotrade.Path(),
		"UpdatedUTC":      time.Now().In(tpe).Format("2006-01-02 15:04 UTC+8"),
	})
}

// reverseNormalised backfills the strategy field on records that predate it,
// then flips the newest-first log into the oldest-first order DedupFires
// needs. Shared by the display list and the portfolio views so the two can
// never normalise the same log differently.
func reverseNormalised(fires []autotrade.PaperFire) []autotrade.PaperFire {
	out := make([]autotrade.PaperFire, len(fires))
	for i, f := range fires {
		if f.Strategy == "" {
			if strings.HasPrefix(f.Why, "engine") {
				f.Strategy = "engine"
			} else {
				f.Strategy = "range-edge"
			}
		}
		out[len(fires)-1-i] = f
	}
	return out
}
