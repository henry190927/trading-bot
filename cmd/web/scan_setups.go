package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"

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
	// Armed distinguishes "auto is watching this and would take it" from
	// "this strategy WOULD set up here, but no rule exists so nothing fires".
	// Without the flag the page implied the system was watching the whole grid
	// when it only ever evaluated the 14 configured rules — 30 of the 44
	// (symbol x strategy) combinations were invisible, including every XAU and
	// LINK combination, because neither symbol has a rule at all.
	Armed bool
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	if s.client == nil {
		s.renderScan(c, nil)
		return
	}

	cands := scanGrid(cfg)
	rows := make([]scanRow, len(cands))

	// Evaluated concurrently with a bounded pool. Each candidate costs a few
	// Klines calls, and the grid is ~3x the rule count, so sequential would
	// have turned a 3.8s page into a timeout. The cap keeps BingX's rate limit
	// out of it; output order is preserved by index so the page is stable
	// across reloads.
	const workers = 6
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, cand := range cands {
		sym, ok := webScanSymbol(cand.rule.Symbol)
		if !ok {
			rows[i] = scanRow{Symbol: cand.rule.Symbol, TF: cand.rule.TF, Strategy: cand.rule.Strategy}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, cand scanCandidate, sym market.Symbol) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = s.evalScanRow(ctx, cand, sym)
		}(i, cand, sym)
	}
	wg.Wait()

	scanCacheMu.Lock()
	scanCacheOut, scanCacheAt = rows, time.Now()
	scanCacheMu.Unlock()
	s.renderScan(c, rows)
}

// scanCandidate is one (symbol, strategy) cell of the grid, plus whether auto
// is actually armed for it.
type scanCandidate struct {
	rule  autotrade.Rule
	armed bool
}

// scanStrategies is the set autostrat.EvalAutoTrigger can dispatch on. Kept
// here rather than derived from the config, because the whole point is to
// evaluate combinations the config does NOT contain.
var scanStrategies = []string{"range-edge", "engine", "sweep-reject", "htf-snr"}

// scanGrid returns every configured+enabled rule, followed by a synthesized
// rule for each (symbol, strategy) pair the config does not cover.
//
// Synthesized rules borrow StopPct / CooldownBars / Side from a real rule of
// the same strategy, so an unarmed row is evaluated the way that strategy is
// ACTUALLY configured rather than against numbers invented here. range-edge is
// the one strategy that reads StopPct, so getting it from a template matters.
func scanGrid(cfg autotrade.Config) []scanCandidate {
	tmpl := map[string]autotrade.Rule{}
	covered := map[string]bool{}
	out := []scanCandidate{}
	for _, r := range cfg.Rules {
		if !r.Enabled {
			continue
		}
		out = append(out, scanCandidate{rule: r, armed: true})
		covered[r.Symbol+"|"+r.Strategy] = true
		if _, ok := tmpl[r.Strategy]; !ok {
			tmpl[r.Strategy] = r
		}
	}
	for _, short := range uiSymbols {
		if _, ok := webScanSymbol(short); !ok {
			continue
		}
		for _, strat := range scanStrategies {
			if covered[short+"|"+strat] {
				continue
			}
			r, ok := tmpl[strat]
			if !ok {
				// No configured rule of this strategy anywhere — fall back to
				// the shape the daemon's own defaults imply.
				r = autotrade.Rule{StopPct: 0.5, CooldownBars: 6}
			}
			r.Enabled = true
			r.Symbol = short
			r.Strategy = strat
			r.Side = "auto"
			if strings.TrimSpace(r.TF) == "" {
				r.TF = "1h"
			}
			out = append(out, scanCandidate{rule: r, armed: false})
		}
	}
	return out
}

// evalScanRow runs one candidate and shapes the row, including R:R.
func (s *server) evalScanRow(ctx context.Context, cand scanCandidate, sym market.Symbol) scanRow {
	r := cand.rule
	row := scanRow{Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, RuleSide: r.Side, Armed: cand.armed}
	trig := autostrat.EvalAutoTrigger(ctx, s.client, sym, r)
	if !trig.Fire {
		return row
	}
	row.Fire = true
	row.Side, row.Entry, row.Stop, row.TP, row.Market, row.Why = trig.Side, trig.Entry, trig.Stop, trig.TP, trig.Market, trig.Why
	row.RR = scanRR(trig.Side, trig.Entry, trig.Stop, trig.TP)
	row.Score = autostrat.FireScore100(ctx, s.client, sym, r.TF, trig.Side, trig.Entry)
	return row
}

// scanRR is reward-over-risk for a trigger. Extracted from the inline version,
// which computed `risk` from the LONG formula and then reassigned it inside the
// short branch — correct, but only by accident of statement order, and it
// silently returned 0 for a zero/negative risk without saying so.
func scanRR(side string, entry, stop, tp float64) float64 {
	var risk, reward float64
	if side == "short" {
		risk, reward = stop-entry, entry-tp
	} else {
		risk, reward = entry-stop, tp-entry
	}
	if risk <= 0 {
		return 0
	}
	return reward / risk
}

func (s *server) renderScan(c *gin.Context, rows []scanRow) {
	// Firing rows are split by whether auto is armed. A page that mixed them
	// would read as "the system will take these", when half of them are
	// setups nothing is watching.
	armedFiring := make([]scanRow, 0)
	unarmedFiring := make([]scanRow, 0)
	armedN := 0
	for _, r := range rows {
		if r.Armed {
			armedN++
		}
		if !r.Fire {
			continue
		}
		if r.Armed {
			armedFiring = append(armedFiring, r)
		} else {
			unarmedFiring = append(unarmedFiring, r)
		}
	}
	// Best score first — with 44 cells, ordering by quality is the difference
	// between a table and an answer.
	sort.SliceStable(armedFiring, func(i, j int) bool { return armedFiring[i].Score > armedFiring[j].Score })
	sort.SliceStable(unarmedFiring, func(i, j int) bool { return unarmedFiring[i].Score > unarmedFiring[j].Score })

	c.HTML(http.StatusOK, "scan_setups.html", gin.H{
		"Rows":          rows,
		"Firing":        armedFiring,
		"UnarmedFiring": unarmedFiring,
		"Watching":      armedN,
		"GridN":         len(rows),
		"UnarmedN":      len(rows) - armedN,
		"FiringN":       len(armedFiring),
		"UnarmedFireN":  len(unarmedFiring),
		"Recorded":      c.Query("recorded"),
		"RecErr":        c.Query("err"),
		"UpdatedUTC":    time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04 UTC+8"),
	})
}

// webScanSymbol resolves the friendly ticker to a BingX symbol for the scan
// (mirrors the daemon's autoSymbols map).
func webScanSymbol(short string) (market.Symbol, bool) {
	m := map[string]market.Symbol{
		"BTC": market.BTCUSDT, "ETH": market.ETHUSDT, "XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
		"SNDK": market.SNDKUSDT, "NVDA": market.NVDAUSDT,
		"SPCX": market.SPCXUSDT, "MSTR": market.MSTRUSDT, "APP": market.APPUSDT,
		"SOL": market.SOLUSDT, "LINK": market.LINKUSDT, "SUI": market.SUIUSDT, "NEAR": market.NEARUSDT, "HYPE": market.HYPEUSDT,
	}
	sym, ok := m[short]
	return sym, ok
}
