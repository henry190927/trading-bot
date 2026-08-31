package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"myFirstGo/trading-bot/autostrat"
	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/market"

	"github.com/gin-gonic/gin"
)

// handleScanRecord records a firing scan row into the /setups forward-log — the
// same tracker N-struct setups use, so /setups is now multi-strategy. Outcome +
// RealOutcome backfill generically from entry/stop/target, so these strategy
// setups get the same win/loss/no-fill scoring.
func (s *server) handleScanRecord(c *gin.Context) {
	pf := func(k string) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(c.PostForm(k)), 64); return v }
	side := strings.ToLower(strings.TrimSpace(c.PostForm("side")))
	su := Setup{
		RecordedAt: time.Now(),
		Symbol:     strings.ToUpper(strings.TrimSpace(c.PostForm("symbol"))),
		TF:         strings.TrimSpace(c.PostForm("tf")),
		Strategy:   strings.TrimSpace(c.PostForm("strategy")),
		Dir:        side,
		StratSide:  side,
		StratFire:  true,
		Price:      pf("entry"),
		Entry:      pf("entry"),
		Stop:       pf("stop"),
		Target:     pf("tp"),
		Note:       fmt.Sprintf("scan · %s/100 · %s", strings.TrimSpace(c.PostForm("score")), strings.TrimSpace(c.PostForm("why"))),
	}
	if su.Symbol == "" || su.Entry <= 0 {
		c.Redirect(http.StatusSeeOther, "/ops/scan?err=1")
		return
	}
	id, err := appendSetup(su)
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/ops/scan?err=1")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/ops/scan?recorded=%d", id))
}

// scanRow is one strategy×symbol evaluation for the live "current setups" scan.
type scanRow struct {
	Symbol, TF, Strategy, RuleSide string
	Fire                           bool
	Side, Why                      string
	Entry, Stop, TP, Score, RR     float64
	Market                         bool
}

var (
	scanCacheMu  sync.Mutex
	scanCacheAt  time.Time
	scanCacheOut []scanRow
)

const scanCacheTTL = 20 * time.Second

// handleScanSetups runs every auto rule's trigger on the CURRENT closed bar and
// reports which would fire right now — the live "what could I enter" view across
// all four strategies (same autostrat logic the paper daemon uses). Cached 20s
// since each row costs a few Klines calls.
func (s *server) handleScanSetups(c *gin.Context) {
	scanCacheMu.Lock()
	if scanCacheOut != nil && time.Since(scanCacheAt) < scanCacheTTL {
		rows := scanCacheOut
		scanCacheMu.Unlock()
		s.renderScan(c, rows)
		return
	}
	scanCacheMu.Unlock()

	cfg := autotrade.Load()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()

	rows := make([]scanRow, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if !r.Enabled {
			continue
		}
		sym, ok := webScanSymbol(r.Symbol)
		if !ok || s.client == nil {
			continue
		}
		row := scanRow{Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, RuleSide: r.Side}
		trig := autostrat.EvalAutoTrigger(ctx, s.client, sym, r)
		if trig.Fire {
			row.Fire = true
			row.Side, row.Entry, row.Stop, row.TP, row.Market, row.Why = trig.Side, trig.Entry, trig.Stop, trig.TP, trig.Market, trig.Why
			if risk := trig.Entry - trig.Stop; row.Side == "short" {
				risk = trig.Stop - trig.Entry
				if risk > 0 {
					row.RR = (trig.Entry - trig.TP) / risk
				}
			} else if risk > 0 {
				row.RR = (trig.TP - trig.Entry) / risk
			}
			row.Score = autostrat.FireScore100(ctx, s.client, sym, r.TF, trig.Side, trig.Entry)
		}
		rows = append(rows, row)
	}

	scanCacheMu.Lock()
	scanCacheOut, scanCacheAt = rows, time.Now()
	scanCacheMu.Unlock()
	s.renderScan(c, rows)
}

func (s *server) renderScan(c *gin.Context, rows []scanRow) {
	firing := make([]scanRow, 0)
	for _, r := range rows {
		if r.Fire {
			firing = append(firing, r)
		}
	}
	c.HTML(http.StatusOK, "scan_setups.html", gin.H{
		"Rows":       rows,
		"Firing":     firing,
		"Watching":   len(rows),
		"FiringN":    len(firing),
		"Recorded":   c.Query("recorded"),
		"RecErr":     c.Query("err"),
		"UpdatedUTC": time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04 UTC+8"),
	})
}

// webScanSymbol resolves the friendly ticker to a BingX symbol for the scan
// (mirrors the daemon's autoSymbols map).
func webScanSymbol(short string) (market.Symbol, bool) {
	m := map[string]market.Symbol{
		"BTC": market.BTCUSDT, "ETH": market.ETHUSDT, "XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
		"SNDK": market.SNDKUSDT, "NVDA": market.NVDAUSDT,
		"SOL": market.SOLUSDT, "LINK": market.LINKUSDT, "SUI": market.SUIUSDT, "NEAR": market.NEARUSDT, "HYPE": market.HYPEUSDT,
	}
	sym, ok := m[short]
	return sym, ok
}
