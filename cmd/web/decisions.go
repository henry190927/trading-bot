// Take/skip decisions on machine-found candidates (package decision).
//
// The candidates are the autotrade paper fires: the home page lists every
// fire still inside its decision window with 做 / 不做 buttons, the bell
// carries one alert per pending candidate (no ntfy — warnings are not
// pushed, see alertPushes), and /decisions scores the taken, skipped and
// unanswered groups on the same bars and fee.
package main

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/alert"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/decision"
	"github.com/henry190927/trading-bot/market"
)

// decisionFeeBps is the round trip charged in the stats, the backtests'
// verdict fee.
const decisionFeeBps = 10

// decisionReadDepth is how many recent paper fires can still be pending: a
// 4h deadline at a few fires per hour is far inside it.
const decisionReadDepth = 200

var tpe8 = time.FixedZone("UTC+8", 8*3600)

// liveMarkShort is the live mark by short symbol (0 when unknown).
func (s *server) liveMarkShort(ctx context.Context, sym string) float64 {
	if s.client == nil {
		return 0
	}
	ms, err := resolveWebSymbol(sym)
	if err != nil {
		return 0
	}
	if fr, err := s.client.FundingRate(ctx, ms); err == nil && fr.MarkPrice > 0 {
		return fr.MarkPrice
	}
	return 0
}

// pendingView is one card on the home page.
type pendingView struct {
	ID       string  `json:"id"`
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	TF       string  `json:"tf"`
	Strategy string  `json:"strategy"`
	Entry    float64 `json:"entry"`
	Stop     float64 `json:"stop"`
	TP       float64 `json:"tp"`
	Trail    bool    `json:"trail"`
	Market   bool    `json:"market"`
	Why      string  `json:"why"`
	At       string  `json:"at"`
	Deadline string  `json:"deadline"`
	MinsLeft int     `json:"mins_left"`
	RNow     float64 `json:"r_now"`
	HasMark  bool    `json:"has_mark"`
}

// handleAPIDecisions — GET /api/decisions: the candidates awaiting a decision.
func (s *server) handleAPIDecisions(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	now := time.Now()
	decided, _, _ := decision.Latest(decision.Path())
	pend := decision.Pending(autotrade.ReadFires(decisionReadDepth), decided, now)
	marks := map[string]float64{}
	out := make([]pendingView, 0, len(pend))
	for _, f := range pend {
		m, ok := marks[f.Symbol]
		if !ok {
			m = s.liveMarkShort(ctx, f.Symbol)
			marks[f.Symbol] = m
		}
		dl := decision.Deadline(f)
		out = append(out, pendingView{
			ID: decision.ID(f), Symbol: f.Symbol, Side: f.Side, TF: f.TF, Strategy: f.Strategy,
			Entry: f.Entry, Stop: f.Stop, TP: f.TP, Trail: f.IsTrail(), Market: f.Market, Why: f.Why,
			At: f.Time.In(tpe8).Format("01/02 15:04"), Deadline: dl.In(tpe8).Format("15:04"),
			MinsLeft: int(math.Ceil(dl.Sub(now).Minutes())),
			RNow:     decision.RMoved(f, m), HasMark: m > 0,
		})
	}
	tr := s.translator(langOf(c))
	label := func(kind string, keys []string) []gin.H {
		var xs []gin.H
		for _, k := range keys {
			xs = append(xs, gin.H{"key": k, "label": tr("dec.r." + kind + ":" + k)})
		}
		return xs
	}
	c.JSON(http.StatusOK, gin.H{
		"pending":      out,
		"take_reasons": label(decision.Take, decision.TakeReasons),
		"skip_reasons": label(decision.Skip, decision.SkipReasons),
	})
}

// handleAPIDecide — POST /api/decisions (id, decision=take|skip, reasons=a,b).
//
// Refused once the deadline has passed or the fire has already settled: a
// decision made after the outcome is known is not a forecast. Re-deciding
// inside the window is allowed and the last record wins (a mis-tap).
func (s *server) handleAPIDecide(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	id := strings.TrimSpace(c.PostForm("id"))
	d := c.PostForm("decision")
	if d != decision.Take && d != decision.Skip {
		c.JSON(http.StatusBadRequest, gin.H{"error": "decision must be take or skip"})
		return
	}
	allowed := map[string]bool{}
	for _, r := range append(append([]string{}, decision.TakeReasons...), decision.SkipReasons...) {
		allowed[r] = true
	}
	var reasons []string
	for _, r := range strings.Split(c.PostForm("reasons"), ",") {
		if r = strings.TrimSpace(r); allowed[r] {
			reasons = append(reasons, r)
		}
	}

	var f autotrade.PaperFire
	found := false
	for _, x := range autotrade.ReadFires(decisionReadDepth) {
		if decision.ID(x) == id {
			f, found = x, true
			break
		}
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "candidate not found"})
		return
	}
	now := time.Now()
	if f.Time.Before(decision.Since) || !now.Before(decision.Deadline(f)) {
		c.JSON(http.StatusConflict, gin.H{"error": "decision window closed"})
		return
	}
	if s.client != nil {
		cs, err := klinesWithRetry(ctx, s.client, f.Symbol, f.TF, 500)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "klines: " + err.Error()})
			return
		}
		if oc := autotrade.Resolve(f, cs, f.Expiry(6)); oc.Status == autotrade.OutTP || oc.Status == autotrade.OutStop {
			c.JSON(http.StatusConflict, gin.H{"error": "already settled (" + string(oc.Status) + ")"})
			return
		}
	}
	mark := s.liveMarkShort(ctx, f.Symbol)
	rec := decision.Record{ID: id, Decision: d, Reasons: reasons, DecidedAt: now.UTC(),
		Mark: mark, RAtDecision: decision.RMoved(f, mark), Fire: f}
	if err := decision.Append(decision.Path(), rec); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if s.alerts != nil {
		s.alerts.AckKey(decisionAlertKey(id))
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func decisionAlertKey(id string) string { return "decision:" + id }

// decisionConditions is the bell's view: one warning per pending candidate.
// Warnings are not pushed to ntfy (alertPushes), which is what was asked.
func decisionConditions(now time.Time) []alert.Condition {
	decided, _, _ := decision.Latest(decision.Path())
	var out []alert.Condition
	for _, f := range decision.Pending(autotrade.ReadFires(decisionReadDepth), decided, now) {
		side := "多"
		if f.Side == "short" {
			side = "空"
		}
		out = append(out, alert.Condition{
			Key: decisionAlertKey(decision.ID(f)), Severity: alert.Warning,
			Title:  "待決策:" + f.Symbol + " " + side + " · " + f.Strategy + " " + f.TF,
			Detail: "到首頁按 做 / 不做 · 截止 " + decision.Deadline(f).In(tpe8).Format("15:04"),
		})
	}
	return out
}

// decisionAcker clears the bell for candidates that stopped being pending
// without a decision through the API (deadline passed, or decided from
// another session): an expired candidate is no longer actionable.
type decisionAcker struct{ last map[string]bool }

func (a *decisionAcker) observe(eng *alert.Engine, firing []alert.Condition) {
	now := map[string]bool{}
	for _, c := range firing {
		now[c.Key] = true
	}
	for k := range a.last {
		if !now[k] {
			eng.AckKey(k)
		}
	}
	a.last = now
}

// ---- /decisions: the scoreboard ----

type decisionRow struct {
	At, Symbol, Side, TF, Strategy, Group string
	Reasons                               []string
	RAtDecision                           float64
	HasDecision                           bool
	Status                                string
	NetR                                  float64
	Settled                               bool
}

type decisionGroup struct {
	Key                string
	N, Settled, Open   int
	Wins               int
	SumR, RPT, SE, WR  float64
	AvgRAtDecision     float64
	payoffs, rAtDecide []float64
}

func (g *decisionGroup) finish() {
	g.Settled = len(g.payoffs)
	if g.Settled == 0 {
		return
	}
	g.RPT = g.SumR / float64(g.Settled)
	g.WR = 100 * float64(g.Wins) / float64(g.Settled)
	if g.Settled > 1 {
		ss := 0.0
		for _, v := range g.payoffs {
			ss += (v - g.RPT) * (v - g.RPT)
		}
		g.SE = math.Sqrt(ss/float64(g.Settled-1)) / math.Sqrt(float64(g.Settled))
	}
	if len(g.rAtDecide) > 0 {
		t := 0.0
		for _, v := range g.rAtDecide {
			t += v
		}
		g.AvgRAtDecision = t / float64(len(g.rAtDecide))
	}
}

// handleDecisionsPage — GET /decisions.
func (s *server) handleDecisionsPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	now := time.Now()
	decided, bad, _ := decision.Latest(decision.Path())

	// Every fire that belongs to a group: the decided ones from their own
	// snapshot (independent of the paper log's depth), the unanswered ones
	// from the log.
	type item struct {
		f     autotrade.PaperFire
		group string
		rec   *decision.Record
	}
	var items []item
	for id := range decided {
		r := decided[id]
		items = append(items, item{r.Fire, r.Decision, &r})
	}
	for _, f := range autotrade.ReadFires(autotradeLogDepth) {
		if g, ok := decision.Group(f, decided, now); ok && g == "none" {
			items = append(items, item{f, g, nil})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].f.Time.After(items[j].f.Time) })

	cache := map[string][]market.Candle{}
	var fetchFails []string
	groups := map[string]*decisionGroup{}
	for _, k := range []string{decision.Take, decision.Skip, "none"} {
		groups[k] = &decisionGroup{Key: k}
	}
	reasonG := map[string]*decisionGroup{}
	var rows []decisionRow
	for _, it := range items {
		f := it.f
		key := f.Symbol + "|" + f.TF
		cs, ok := cache[key]
		if !ok && s.client != nil {
			from := f.Time
			for _, x := range items {
				if x.f.Symbol == f.Symbol && x.f.TF == f.TF && x.f.Time.Before(from) {
					from = x.f.Time
				}
			}
			if f.IsTrail() {
				from = from.Add(-time.Duration(f.TrailM+2) * market.BarDuration(market.Timeframe(f.TF)))
			}
			var err error
			cs, err = s.klinesSince(ctx, f.Symbol, f.TF, from)
			if err != nil {
				fetchFails = append(fetchFails, key+": "+err.Error())
			}
			cache[key] = cs
		}
		oc := autotrade.WithFee(f, autotrade.Resolve(f, cs, f.Expiry(6)), decisionFeeBps)
		settled := oc.Status == autotrade.OutTP || oc.Status == autotrade.OutStop
		row := decisionRow{
			At: f.Time.In(tpe8).Format("01/02 15:04"), Symbol: f.Symbol, Side: f.Side, TF: f.TF,
			Strategy: f.Strategy, Group: it.group, Status: string(oc.Status), NetR: oc.NetR, Settled: settled,
		}
		g := groups[it.group]
		g.N++
		var tags []string
		if it.rec != nil {
			row.Reasons, row.RAtDecision, row.HasDecision = it.rec.Reasons, it.rec.RAtDecision, true
			g.rAtDecide = append(g.rAtDecide, it.rec.RAtDecision)
			for _, r := range it.rec.Reasons {
				tags = append(tags, it.group+":"+r)
			}
		}
		if settled {
			g.SumR += oc.NetR
			g.payoffs = append(g.payoffs, oc.NetR)
			if oc.NetR > 0 {
				g.Wins++
			}
			for _, t := range tags {
				rg := reasonG[t]
				if rg == nil {
					rg = &decisionGroup{Key: t}
					reasonG[t] = rg
				}
				rg.N++
				rg.SumR += oc.NetR
				rg.payoffs = append(rg.payoffs, oc.NetR)
				if oc.NetR > 0 {
					rg.Wins++
				}
			}
		} else if oc.Status == autotrade.OutOpen || oc.Status == autotrade.OutPending {
			g.Open++
		}
		rows = append(rows, row)
	}
	var gs []*decisionGroup
	for _, k := range []string{decision.Take, decision.Skip, "none"} {
		groups[k].finish()
		gs = append(gs, groups[k])
	}
	var rs []*decisionGroup
	for _, g := range reasonG {
		g.finish()
		rs = append(rs, g)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Key < rs[j].Key })

	// The one comparison the log exists for, with its noise: take − skip
	// R/trade and the combined SE. Under ~30 settled per side it is noise.
	t, sk := groups[decision.Take], groups[decision.Skip]
	diff := t.RPT - sk.RPT
	diffSE := math.Sqrt(t.SE*t.SE + sk.SE*sk.SE)
	s.page(c, http.StatusOK, "decisions.html", gin.H{
		"Groups": gs, "Reasons": rs, "Rows": rows, "FetchFails": fetchFails, "Bad": bad,
		"Since": decision.Since.Format("2006-01-02"), "FeeBps": decisionFeeBps,
		"Diff": diff, "DiffSE": diffSE, "Enough": t.Settled >= 30 && sk.Settled >= 30,
	})
}
