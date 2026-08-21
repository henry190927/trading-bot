package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/macro"
)

// /today — the "attention feed" home. Answers the morning questions on one
// page: can the engine trade (macro blackout)? how are my open trades doing
// (live R)? is the daemon alive? what setups need a decision? Deliberately
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

	// Open trades + live R (light: nil views → fetches only open symbols).
	openTrades := s.buildOpenTradeCards(ctx, nil)

	// Daemon health.
	daemons := make([]gin.H, 0, 3)
	for _, svc := range []string{"trading-bot", "trading-monitor", "trading-web"} {
		st := readDaemonStatus(svc)
		daemons = append(daemons, gin.H{"name": svc, "active": st.Active, "state": st.State})
	}

	// Pending setups needing an outcome decision.
	var pending []Setup
	if all, err := readSetups(); err == nil {
		for _, su := range all {
			if su.Outcome == "" {
				pending = append(pending, su)
			}
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].ID > pending[j].ID })
		if len(pending) > 8 {
			pending = pending[:8]
		}
	}

	c.HTML(http.StatusOK, "today.html", gin.H{
		"MacroActive": activeBO,
		"BONext":      boNext,
		"OpenTrades":  openTrades,
		"Daemons":     daemons,
		"Pending":     pending,
		"NowTPE":      now.In(tpe).Format("2006-01-02 15:04"),
	})
}
