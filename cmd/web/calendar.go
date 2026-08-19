package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/macro"
)

// /calendar — the macro-event calendar (the SAME curated list that drives the
// engine's blackout gate, so what you see here is exactly what suppresses
// signals). High-impact US macro (CPI/FOMC/minutes/NFP/PPI/Jackson Hole),
// grouped by month, with each event's blackout window + past/active/upcoming.

type calEvent struct {
	Name    string
	Cat     string // fomc / minutes / cpi / ppi / nfp / jackson / other
	WhenUTC string
	WhenTPE string
	Window  string
	Status  string // past / active / upcoming
	Days    int    // days away (upcoming only)
}

type calMonth struct {
	Label  string
	Events []calEvent
}

func categorizeEvent(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "jackson"):
		return "jackson"
	case strings.Contains(n, "minutes"):
		return "minutes"
	case strings.Contains(n, "fomc"):
		return "fomc"
	case strings.Contains(n, "cpi"):
		return "cpi"
	case strings.Contains(n, "ppi"):
		return "ppi"
	case strings.Contains(n, "nfp"):
		return "nfp"
	}
	return "other"
}

func (s *server) handleCalendarPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	now := time.Now().UTC()
	tpe := time.FixedZone("Asia/Taipei", 8*3600)

	all := macro.All()
	sorted := make([]macro.Event, len(all))
	copy(sorted, all)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DatetimeUTC.Before(sorted[j].DatetimeUTC) })

	var months []calMonth
	curKey := ""
	for _, e := range sorted {
		start, end := e.Window()
		status := "upcoming"
		switch {
		case now.After(end):
			status = "past"
		case !now.Before(start) && now.Before(end):
			status = "active"
		}
		ev := calEvent{
			Name:    e.Name,
			Cat:     categorizeEvent(e.Name),
			WhenUTC: e.DatetimeUTC.Format("Jan 02 15:04"),
			WhenTPE: e.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			Window:  fmt.Sprintf("−%dm / +%dm", e.BeforeMinutes, e.AfterMinutes),
			Status:  status,
		}
		if status == "upcoming" {
			ev.Days = int(e.DatetimeUTC.Sub(now).Hours() / 24)
		}
		key := e.DatetimeUTC.Format("2006-01")
		if key != curKey {
			months = append(months, calMonth{Label: e.DatetimeUTC.Format("January 2006")})
			curKey = key
		}
		months[len(months)-1].Events = append(months[len(months)-1].Events, ev)
	}

	var activeName, nextName, nextWhen string
	var nextDays int
	if a := macro.ActiveAt(now); a != nil {
		activeName = a.Name
	}
	if nx := macro.NextUpcoming(now, 365*24*time.Hour); nx != nil {
		nextName = nx.Name
		nextWhen = nx.DatetimeUTC.In(tpe).Format("01/02 15:04")
		nextDays = int(nx.DatetimeUTC.Sub(now).Hours() / 24)
	}

	c.HTML(http.StatusOK, "calendar.html", gin.H{
		"Months":     months,
		"ActiveName": activeName,
		"NextName":   nextName,
		"NextWhen":   nextWhen,
		"NextDays":   nextDays,
		"UpdatedUTC": now.Format("2006-01-02 15:04 UTC"),
	})
}
