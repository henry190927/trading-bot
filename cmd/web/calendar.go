package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bls"
	"github.com/henry190927/trading-bot/earnings"
	"github.com/henry190927/trading-bot/econcal"
	"github.com/henry190927/trading-bot/macro"
)

// /calendar — the macro-event calendar (the SAME curated list that drives the
// engine's blackout gate, so what you see here is exactly what suppresses
// signals). High-impact US macro (CPI/FOMC/minutes/NFP/PPI/Jackson Hole),
// grouped by month, with each event's blackout window + past/active/upcoming.

type calEvent struct {
	Name     string
	Cat      string // fomc / minutes / cpi / ppi / nfp / jackson / earnings / data / other
	WhenUTC  string
	WhenTPE  string
	Window   string
	Status   string // past / active / upcoming
	Days     int    // days away (upcoming only)
	Impact   string // data releases only: High / Medium
	Forecast string // data releases only
	Previous string // data releases only
	// Actual is the RELEASED figure, read from BLS (package bls) rather than
	// from the forecast feed — that feed's `actual` field is empty on every
	// row, published or not. Blank covers three different states the template
	// must not conflate: series not mapped, month not released yet, no cache.
	// Surprise is set only when the actual and the forecast both parse.
	Actual   string
	Surprise string // hot / cool / inline
	// Speaker promotes central-bank speech past the feed's own rating. The
	// feed calls "FOMC Member Waller Speaks" LOW impact; rendering it as low
	// priority is how it got overlooked on 2026-09-03.
	Speaker bool
}

type calMonth struct {
	Label  string
	Events []calEvent
}

// surpriseOf compares a released figure against its forecast string.
//
// Returns "" when the forecast does not parse, which is the normal case for
// speeches and policy statements — an empty column beats a verdict computed
// from something that was never a number.
//
// The band is a tenth of the quoted unit (0.1pp for percentages, 1K for
// payrolls) because BLS publishes CPI and PPI to one decimal place: a gap
// smaller than that is the rounding, not a surprise.
func surpriseOf(actual float64, forecast string) string {
	f := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(forecast), "%"))
	if strings.HasSuffix(f, "K") || strings.HasSuffix(f, "k") {
		f = f[:len(f)-1]
	}
	want, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
	if err != nil {
		return ""
	}
	switch d := actual - want; {
	case d >= 0.1:
		return "hot"
	case d <= -0.1:
		return "cool"
	}
	return "inline"
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

	statusOf := func(start, end time.Time) string {
		switch {
		case now.After(end):
			return "past"
		case !now.Before(start) && now.Before(end):
			return "active"
		default:
			return "upcoming"
		}
	}

	// Merge macro events + per-symbol earnings (same gates the engine uses) into
	// one datetime-sorted stream so both show on the calendar.
	type dated struct {
		dt time.Time
		ev calEvent
	}
	var items []dated

	for _, e := range macro.All() {
		start, end := e.Window()
		ev := calEvent{
			Name:    e.Name,
			Cat:     categorizeEvent(e.Name),
			WhenUTC: e.DatetimeUTC.Format("Jan 02 15:04"),
			WhenTPE: e.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			Window:  fmt.Sprintf("−%dm / +%dm", e.BeforeMinutes, e.AfterMinutes),
			Status:  statusOf(start, end),
		}
		if ev.Status == "upcoming" {
			ev.Days = int(e.DatetimeUTC.Sub(now).Hours() / 24)
		}
		items = append(items, dated{e.DatetimeUTC, ev})
	}

	for _, e := range earnings.Default().Events() {
		start, end := e.Window()
		name := e.Symbol + " earnings"
		if e.When != "" {
			name += " (" + e.When + ")"
		}
		ev := calEvent{
			Name:    name,
			Cat:     "earnings",
			WhenUTC: e.DatetimeUTC.Format("Jan 02 15:04"),
			WhenTPE: e.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			Window:  fmt.Sprintf("−%dm / +%dm", e.BeforeMinutes, e.AfterMinutes),
			Status:  statusOf(start, end),
		}
		if ev.Status == "upcoming" {
			ev.Days = int(e.DatetimeUTC.Sub(now).Hours() / 24)
		}
		items = append(items, dated{e.DatetimeUTC, ev})
	}

	// Economic-data releases (ForexFactory feed) — display layer, NOT a blackout
	// gate. Shows impact/forecast/previous like an FX calendar.
	// Read-only here: the cache is refreshed by the monitor
	// (cmd/monitor/blsrefresh.go), because BLS v1 allows 25 queries a DAY and
	// a page load must never spend one. An empty store leaves the column blank.
	blsStore := bls.Load()

	for _, e := range econcal.All() {
		name := e.Title
		if e.Country != "" && e.Country != "USD" {
			name = e.Country + " · " + name
		}
		cat := "data"
		if e.Speaker {
			cat = "speech"
		}
		// A speech has no release instant to be "30 minutes past" — remarks and
		// the Q&A that follows run long, and the tape keeps moving with them.
		active := 30 * time.Minute
		if e.Speaker {
			active = 90 * time.Minute
		}
		ev := calEvent{
			Name:     name,
			Cat:      cat,
			WhenUTC:  e.DatetimeUTC.Format("Jan 02 15:04"),
			WhenTPE:  e.DatetimeUTC.In(tpe).Format("01/02 15:04"),
			Window:   e.Impact,
			Status:   statusOf(e.DatetimeUTC, e.DatetimeUTC.Add(active)),
			Impact:   e.Impact,
			Forecast: e.Forecast,
			Previous: e.Previous,
			Speaker:  e.Speaker,
		}
		// The released figure, and whether it beat the forecast. This is the
		// question a calendar of forecasts alone cannot answer: on 2026-09-10
		// a PPI print moved BTC 1,290 points in an hour and the only way to
		// ask "was it actually hot?" was to read it off the price — which gave
		// the wrong answer, since headline PPI came in at +0.40% against a
		// +0.4% forecast, exactly in line.
		if a, unit, ok := blsStore.Actual(e.Title, e.DatetimeUTC); ok {
			if unit == "k" {
				ev.Actual = fmt.Sprintf("%+.0fK", a)
			} else {
				ev.Actual = fmt.Sprintf("%+.2f%%", a)
			}
			ev.Surprise = surpriseOf(a, e.Forecast)
		}
		if ev.Status == "upcoming" {
			ev.Days = int(e.DatetimeUTC.Sub(now).Hours() / 24)
		}
		items = append(items, dated{e.DatetimeUTC, ev})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].dt.Before(items[j].dt) })

	var months []calMonth
	curKey := ""
	for _, it := range items {
		key := it.dt.Format("2006-01")
		if key != curKey {
			months = append(months, calMonth{Label: it.dt.Format("January 2006")})
			curKey = key
		}
		months[len(months)-1].Events = append(months[len(months)-1].Events, it.ev)
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
