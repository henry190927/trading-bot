package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"

	"github.com/henry190927/trading-bot/econcal"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/macro"
)

// /today — the "attention feed" home. Answers the morning questions on one
// page: can the engine trade (macro blackout)? how are my open trades doing
// (live R)? what setups need a decision? (Whether the daemons are alive is
// the alert bell's job now, on every page.) Deliberately
// LIGHT — no 4-symbol engine scan (that's /), just fast sources.
func (s *server) handleTodayPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	// Macro blackout — can the engine fire?
	activeBO := macro.ActiveAt(now)
	upcoming := macro.NextUpcoming(now, 96*time.Hour)
	var boNext gin.H
	if upcoming != nil {
		boNext = gin.H{
			"name":  upcoming.Name,
			"when":  upcoming.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			"hours": fmt.Sprintf("%.1f", upcoming.DatetimeUTC.Sub(now).Hours()),
		}
	}

	// What the curated table does NOT cover. On 2026-09-10 this hero read
	// "no macro events on the calendar" on a day an ECB rate hike and a US
	// PPI print landed 15 minutes apart — truthfully, because the table had
	// gone stale. An empty calendar is indistinguishable from a quiet day
	// unless the page says which is which, so name the gap here.
	//
	// Over-inclusive by choice (every High plus central-bank speakers, any
	// country): this is a heads-up list, not a gate, and for a warning the
	// safe failure direction is one row too many rather than one too few.
	var macroGaps []gin.H
	var liveCands []macro.Candidate
	for _, e := range econcal.All() {
		if e.Impact != "High" && !e.Speaker {
			continue
		}
		liveCands = append(liveCands, macro.Candidate{
			Name:        e.Title,
			Country:     e.Country,
			DatetimeUTC: e.DatetimeUTC,
		})
	}
	for _, g := range macro.Uncovered(liveCands, now, 48*time.Hour) {
		macroGaps = append(macroGaps, gin.H{
			"name":    g.Name,
			"country": g.Country,
			"when":    g.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			"hours":   fmt.Sprintf("%.1f", g.DatetimeUTC.Sub(now).Hours()),
		})
	}

	// Open trades + live R (light: nil views → fetches only open symbols).
	openTrades := s.buildOpenTradeCards(ctx, nil)

	// Pending 模擬單 (setups) still waiting for an outcome. The KPI shows the
	// full count; the list shows the newest eight.
	var pending []pendingRow
	pendingN := 0
	if all, err := readSetups(); err == nil {
		var open []Setup
		for _, su := range all {
			if su.Outcome == "" {
				open = append(open, su)
			}
		}
		pendingN = len(open)
		sort.Slice(open, func(i, j int) bool { return open[i].ID > open[j].ID })
		if len(open) > 8 {
			open = open[:8]
		}
		for _, su := range open {
			pending = append(pending, pendingRow{Setup: su, LeanKey: leanKey(su), At: su.RecordedAt.In(tpe).Format("01-02 15:04")})
		}
	}

	// Realized R today / this week, the same numbers the portfolio hero shows.
	var periods periodStats
	if trades, err := journal.ReadAll(""); err == nil {
		periods = buildPeriodStats(trades)
	}
	unread := 0
	if s.alerts != nil {
		unread = s.alerts.Snapshot(0).Unread
	}

	s.page(c, http.StatusOK, "today.html", gin.H{
		"MacroActive": activeBO,
		"BONext":      boNext,
		"MacroGaps":   macroGaps,
		"OpenTrades":  openTrades,
		"Pending":     pending,
		"PendingN":    pendingN,
		"Periods":     periods,
		"Unread":      unread,
	})
}

// pendingRow is one 模擬單 on the Today list.
type pendingRow struct {
	Setup
	LeanKey string // i18n key for the structural read
	At      string // recorded, UTC+8
}

// leanKey names a setup's structural read as a dictionary key. The stored
// Lean is display text written by the chart page in Chinese, so it cannot be
// shown in English; the event and trend fields it was derived from can.
// Same precedence as structLean in chart.html: event, then zone, then trend.
func leanKey(s Setup) string {
	switch s.Event {
	case "BOS-up":
		return "lean.bosUp"
	case "BOS-down":
		return "lean.bosDown"
	case "CHoCH-up":
		return "lean.chochUp"
	case "CHoCH-down":
		return "lean.chochDown"
	}
	if s.InZone {
		if s.Dir == "short" {
			return "lean.zoneDown"
		}
		return "lean.zoneUp"
	}
	switch s.Trend {
	case "HH-HL uptrend":
		return "lean.trendUp"
	case "LH-LL downtrend":
		return "lean.trendDown"
	}
	return "lean.neutral"
}

// handleAPITodayPositions — GET /api/today/positions. The /today 持倉 card's
// contents as an HTML fragment, rendered by the same positionsBody template
// as the page, for its 15s refresh.
func (s *server) handleAPITodayPositions(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Render(http.StatusOK, render.HTML{
		Template: s.pages[langOf(c)],
		Name:     "positionsBody",
		Data:     gin.H{"OpenTrades": s.buildOpenTradeCards(c.Request.Context(), nil)},
	})
}
