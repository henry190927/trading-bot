package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/analyzer"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/journal"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
	"myFirstGo/trading-bot/validator"
)

type server struct {
	client *bingx.Client
}

// symbolView is the per-symbol bundle the dashboard template iterates over.
type symbolView struct {
	Symbol    market.Symbol
	Short     string // BTC / ETH / XAU / XAG for the header
	Signal    signal.Signal
	Context   signal.Context
	HVNValues []string // formatted top-5 HVNs for display
	Err       string   // non-empty if scan failed for this symbol

	// Candles is the raw candle series this scan ran on. Retained so the
	// open-trades card can re-run validator.Validate against the open
	// trade's specific side (not just the engine's preferred direction),
	// without a second Klines fetch. Not used by the symbol-card template.
	Candles []market.Candle
	// MarkPrice is the live mark fetched alongside FundingRate. Carried
	// separately from Signal.Price (which is overridden to the same value
	// for display) so the open-trade re-validation can pass it through.
	MarkPrice float64
	// ChartJSON is the pre-encoded payload the mini-chart JS reads on the
	// dashboard. Closes + timestamps + optional plan markers, ready to be
	// inlined inside a <script type="application/json"> block.
	ChartJSON template.JS

	// ClosedClose is the close of the most recently CLOSED bar — the
	// reference the engine evaluates against. Displayed alongside the
	// live mark price (Signal.Price) so the trader can see both at a
	// glance: "live = where you'd fill now; close = what the engine sees."
	ClosedClose float64

	// Diagnose is the validator's full scoring of "long now at market" or
	// "short now at market", whichever scores higher. Surfaces the
	// falling-knife / chase / HVN-wall flags directly on the dashboard so
	// the user can tell a swing-low from a falling knife at a glance
	// without typing into /validate manually.
	Diagnose *validator.Result
}

func shortSymbol(s market.Symbol) string {
	switch s {
	case market.XAUUSDT:
		return "XAU"
	case market.XAGUSDT:
		return "XAG"
	case market.BTCUSDT:
		return "BTC"
	case market.ETHUSDT:
		return "ETH"
	}
	return string(s)
}

// handleDashboard renders the snapshot view of all 4 symbols at the given TF.
// Default TF is 1h. Fetches data in parallel and times out after 20s.
func (s *server) handleDashboard(c *gin.Context) {
	tf := c.DefaultQuery("tf", "1h")
	timeframe := market.Timeframe(tf)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	views := make([]symbolView, len(market.All()))
	var wg sync.WaitGroup
	for i, sym := range market.All() {
		wg.Add(1)
		go func(idx int, sy market.Symbol) {
			defer wg.Done()
			views[idx] = s.scanOne(ctx, sy, timeframe)
		}(i, sym)
	}
	wg.Wait()

	// Stable ordering: BTC, ETH, XAU, XAG (matches market.All()).
	sort.SliceStable(views, func(i, j int) bool {
		order := map[market.Symbol]int{market.BTCUSDT: 0, market.ETHUSDT: 1, market.XAUUSDT: 2, market.XAGUSDT: 3}
		return order[views[i].Symbol] < order[views[j].Symbol]
	})

	// Build open-trade cards (live status of journal entries that haven't
	// been closed yet). Each open trade's diagnose is computed on its OWN
	// TF (not the dashboard's selected TF) — so a 1h trade keeps showing
	// its 1h-context diagnose even when the user switches the dashboard
	// to 15m or 4h.
	openTrades := s.buildOpenTradeCards(ctx, views)

	// Disable client-side caching so the meta-refresh reload actually fetches
	// fresh data (iOS Safari otherwise serves the page from cache on the
	// next 60s tick if no Cache-Control is set).
	c.Header("Cache-Control", "no-store, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.HTML(http.StatusOK, "dashboard.html", gin.H{
		"TF":           tf,
		"Symbols":      views,
		"OpenTrades":   openTrades,
		"Now":          time.Now().Format("2006-01-02 15:04:05"),
		"TFOptions":    []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"},
		"MinTradeable": 3, // for verdict coloring
	})
}

// openTradeCard is the live status of an open journal entry, computed against
// the dashboard's current mark prices. Pure decision-support — the system
// can't move stops on BingX itself, but the trader can see at a glance how
// much of each trade's risk envelope has been consumed.
type openTradeCard struct {
	Trade       journal.Trade
	MarkPrice   float64       // live mark for the symbol (closed-bar close fallback)
	CurrentR    float64       // signed R: positive = in profit, negative = drawdown
	PctToStop   float64       // 0-100% of the way from entry to stop
	PctToTP1    float64       // 0-100% of the way from entry to TP1
	PctToTP2    float64       // 0-100% of the way from entry to TP2
	TimeElapsed time.Duration // since the trade became "live" (FilledAt if set, else OpenedAt)
	Diagnose    *validator.Result

	// Pending-state fields (populated when Trade.IsPending()).
	DistToEntryPct float64 // |mark - entry| / entry * 100, signed for direction
	PendingSince   time.Duration
}

func (s *server) buildOpenTradeCards(ctx context.Context, dashViews []symbolView) []openTradeCard {
	trades, err := journal.ReadAll("")
	if err != nil {
		return nil
	}

	// Cache (Symbol, TF) -> symbolView so we don't double-scan when the
	// open trade happens to be on the same TF the dashboard is rendering.
	// Pre-populate with the dashboard's already-computed views.
	cache := map[string]symbolView{}
	for _, v := range dashViews {
		cache[string(v.Symbol)+"@"+string(v.Signal.Timeframe)] = v
	}

	// Collect distinct (Symbol, TF) pairs that the dashboard scan didn't
	// cover but open trades need. Fetch them in parallel.
	type pair struct {
		sym market.Symbol
		tf  market.Timeframe
	}
	needed := map[string]pair{}
	for _, t := range trades {
		if !t.IsOpen() || t.TF == "" {
			continue
		}
		sym, err := resolveWebSymbol(t.Symbol)
		if err != nil {
			continue
		}
		// t.TF can be a comma-separated list (e.g. "15m,1h"); take the first
		// concrete TF for the diagnose lookup. Open-trade scanning a freeform
		// composite TF would be ambiguous.
		tfStr := t.TF
		if i := strings.Index(tfStr, ","); i >= 0 {
			tfStr = strings.TrimSpace(tfStr[:i])
		}
		key := string(sym) + "@" + tfStr
		if _, ok := cache[key]; ok {
			continue
		}
		needed[key] = pair{sym: sym, tf: market.Timeframe(tfStr)}
	}
	if len(needed) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for k, p := range needed {
			wg.Add(1)
			go func(k string, p pair) {
				defer wg.Done()
				v := s.scanOne(ctx, p.sym, p.tf)
				mu.Lock()
				cache[k] = v
				mu.Unlock()
			}(k, p)
		}
		wg.Wait()
	}

	// Pass 1: auto-detect entry fills. For each pending trade, scan the
	// candles already fetched for its symbol/TF; if any bar since the
	// trade was recorded touched the entry level, mark it filled. Persist
	// any changes to the journal in one write at the end.
	tradesChanged := false
	for i := range trades {
		t := &trades[i]
		if !t.IsPending() {
			continue
		}
		tfStr := t.TF
		if j := strings.Index(tfStr, ","); j >= 0 {
			tfStr = strings.TrimSpace(tfStr[:j])
		}
		sym, errSym := resolveWebSymbol(t.Symbol)
		if errSym != nil || tfStr == "" {
			continue
		}
		v, ok := cache[string(sym)+"@"+tfStr]
		if !ok || v.Err != "" || len(v.Candles) == 0 {
			continue
		}
		// Floor = opened_at strictly. Only bars closing AFTER the user
		// clicked +record count as potential fills — mirrors how a
		// real limit order behaves on an exchange (the order doesn't
		// exist until placed). Using analyzed_at as a wider floor
		// caused false positives because the bar containing the click
		// often has Low/High that touched entry before the click.
		// If the user records too late and misses a real fill, they
		// can manually set filled_at via the edit form.
		floor := t.OpenedAt
		hit := false
		for _, c := range v.Candles {
			if !c.CloseTime.After(floor) {
				continue
			}
			var barHit bool
			switch t.Side {
			case "long":
				barHit = c.Low <= t.Entry
			case "short":
				barHit = c.High >= t.Entry
			}
			if barHit {
				t.FilledAt = c.CloseTime
				tradesChanged = true
				hit = true
				break
			}
		}
		// Live-mark fallback: closed-bar detection only sees bars after
		// they close. On 2h/4h TFs that lag is hours. If the current
		// mark price is already past the entry in the side's direction,
		// the fill has happened — record it now at wall-clock time
		// (~30s precision per refresh cycle).
		if !hit {
			mark := v.MarkPrice
			if mark == 0 {
				mark = v.Signal.Price
			}
			if mark > 0 {
				var liveHit bool
				switch t.Side {
				case "long":
					liveHit = mark <= t.Entry
				case "short":
					liveHit = mark >= t.Entry
				}
				if liveHit {
					t.FilledAt = time.Now().UTC()
					tradesChanged = true
				}
			}
		}
	}
	if tradesChanged {
		if err := journal.WriteAll("", trades); err != nil {
			// Soft-fail: log only, dashboard still renders w/ in-memory state.
			log.Printf("auto-fill journal write failed: %v", err)
		}
	}

	var cards []openTradeCard
	for _, t := range trades {
		if !t.IsOpen() {
			continue
		}
		// "Live" reference time: FilledAt for active trades, OpenedAt for pending.
		liveSince := t.OpenedAt
		if !t.FilledAt.IsZero() {
			liveSince = t.FilledAt
		}
		card := openTradeCard{Trade: t, TimeElapsed: time.Since(liveSince)}
		if t.IsPending() {
			card.PendingSince = time.Since(t.OpenedAt)
		}

		// Resolve the trade's own scan from the cache (its TF, not the
		// dashboard's). Mark price + diagnose both come from there.
		tfStr := t.TF
		if i := strings.Index(tfStr, ","); i >= 0 {
			tfStr = strings.TrimSpace(tfStr[:i])
		}
		if sym, err := resolveWebSymbol(t.Symbol); err == nil && tfStr != "" {
			if v, ok := cache[string(sym)+"@"+tfStr]; ok && v.Err == "" {
				card.MarkPrice = v.MarkPrice
				if card.MarkPrice == 0 {
					card.MarkPrice = v.Signal.Price
				}
				// Re-run validator at the TRADE's own side (not the engine's
				// preferred side) with the trade's planned entry. This way
				// the diagnose row answers "is MY trade still good?", not
				// "what's the best trade on this symbol right now?".
				if len(v.Candles) >= 60 && (t.Side == "long" || t.Side == "short") {
					var side signal.Side
					if t.Side == "long" {
						side = signal.Long
					} else {
						side = signal.Short
					}
					entry := t.Entry
					if entry == 0 {
						entry = card.MarkPrice
					}
					r := validator.Validate(sym, market.Timeframe(tfStr), side, entry, 6.0, v.Candles, card.MarkPrice)
					card.Diagnose = &r
				}
			}
		}

		// R-progress only makes sense for ACTIVE (filled) trades. Pending
		// trades show distance-to-entry instead.
		if t.IsPending() {
			if card.MarkPrice > 0 && t.Entry > 0 {
				// Signed % from mark TO entry, in the direction needed to fill.
				// Long needs mark to fall to entry (mark > entry currently),
				// short needs mark to rise to entry (mark < entry currently).
				if t.Side == "long" {
					card.DistToEntryPct = (card.MarkPrice - t.Entry) / t.Entry * 100
				} else {
					card.DistToEntryPct = (t.Entry - card.MarkPrice) / t.Entry * 100
				}
			}
		} else if card.MarkPrice > 0 && t.Stop != t.Entry {
			risk := t.Stop - t.Entry
			if risk < 0 {
				risk = -risk
			}
			var moved float64
			if t.Side == "long" {
				moved = card.MarkPrice - t.Entry
			} else {
				moved = t.Entry - card.MarkPrice
			}
			card.CurrentR = moved / risk
			// Each fill (.ot-bar-fill.negative/.positive) is anchored at the
			// bar's center (50%), so its CSS width is the % of *half* the bar.
			// Negative side: full at -1R → 50% width covers entry→stop.
			// Positive side: full at +2R (TP2) → 50% width covers entry→TP2.
			card.PctToStop = clampPct(-moved / risk * 50)
			if t.TP1 != 0 {
				card.PctToTP1 = clampPct(moved / risk * 50)
			}
			if t.TP2 != 0 {
				card.PctToTP2 = clampPct(moved / risk / 2 * 50)
			}
		}
		cards = append(cards, card)
	}
	return cards
}

// buildChartJSON packs the last ~60 closed candles + plan markers into a
// compact JSON object the mini-chart JS unmarshals into uPlot data. We
// inline this in the dashboard HTML (no separate /api endpoint) — 4 symbols
// × 60 points × ~12 bytes/point ≈ 3KB per refresh, well under the meta-
// refresh's existing 100KB budget.
func buildChartJSON(v symbolView) template.JS {
	const N = 120
	if len(v.Candles) == 0 {
		return template.JS(`null`)
	}
	start := len(v.Candles) - N
	if start < 0 {
		start = 0
	}
	cs := v.Candles[start:]
	// Two parallel arrays: t (unix seconds), c (close prices). uPlot's
	// expected x-y data shape — saves repeated key lookups in JS.
	type payload struct {
		T      []int64   `json:"t"`     // unix seconds, x-axis
		O      []float64 `json:"o"`     // opens (for candle mode)
		H      []float64 `json:"h"`     // highs
		L      []float64 `json:"l"`     // lows
		C      []float64 `json:"c"`     // closes (also used for line mode)
		Side   string    `json:"side"`  // "long" / "short" / ""
		Entry  float64   `json:"entry"` // plan entry, 0 if no plan
		Stop   float64   `json:"stop"`
		TP1    float64   `json:"tp1"`
		TP2    float64   `json:"tp2"`
		Mark   float64   `json:"mark"` // live mark price
		POC    float64   `json:"poc"`  // primary POC (200-bar)
		POC50  float64   `json:"poc50"`
		POC100 float64   `json:"poc100"`
		VAH    float64   `json:"vah"`
		VAL    float64   `json:"val"`
	}
	p := payload{
		T:      make([]int64, len(cs)),
		O:      make([]float64, len(cs)),
		H:      make([]float64, len(cs)),
		L:      make([]float64, len(cs)),
		C:      make([]float64, len(cs)),
		Mark:   v.MarkPrice,
		POC:    v.Signal.VP.POC,
		POC50:  v.Signal.POCMig.POCShort,
		POC100: v.Signal.POCMig.POCMed,
		VAH:    v.Signal.VP.VAH,
		VAL:    v.Signal.VP.VAL,
	}
	for i, c := range cs {
		p.T[i] = c.CloseTime.Unix()
		p.O[i] = c.Open
		p.H[i] = c.High
		p.L[i] = c.Low
		p.C[i] = c.Close
	}
	if v.Signal.Plan.Entry > 0 {
		p.Entry = v.Signal.Plan.Entry
		p.Stop = v.Signal.Plan.StopLoss
		if len(v.Signal.Plan.TakeProfit) >= 1 {
			p.TP1 = v.Signal.Plan.TakeProfit[0]
		}
		if len(v.Signal.Plan.TakeProfit) >= 2 {
			p.TP2 = v.Signal.Plan.TakeProfit[1]
		}
		switch v.Signal.Side {
		case signal.Long:
			p.Side = "long"
		case signal.Short:
			p.Side = "short"
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return template.JS(`null`)
	}
	return template.JS(b)
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func (s *server) scanOne(ctx context.Context, sym market.Symbol, tf market.Timeframe) symbolView {
	v := symbolView{Symbol: sym, Short: shortSymbol(sym)}

	candles, err := s.client.Klines(ctx, sym, tf, 300)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	sigCtx := signal.Context{}
	// Mark price is the live perp price BingX continuously updates — what the
	// BingX app shows. After Patch 1 trims the forming bar, the closed-candle
	// close is up to one TF-bar stale, so we use mark price for the displayed
	// "current price" while signal evaluation continues to use closed bars
	// only (no lookahead). Falls back to candle close if funding fetch fails.
	var markPrice float64
	if fr, err := s.client.FundingRate(ctx, sym); err == nil {
		sigCtx.FundingRate = fr.Rate
		markPrice = fr.MarkPrice
	}
	if oi, err := s.client.OpenInterest(ctx, sym); err == nil {
		sigCtx.OpenInterest = oi
	}
	v.Signal = signal.Evaluate(signal.Inputs{
		Symbol: sym, Timeframe: tf, Candles: candles, Ctx: sigCtx,
		LiveMarkPrice: markPrice, // engine uses for plan-validity suppression
	})
	v.Context = sigCtx
	v.Candles = candles                           // retain for open-trade re-validation
	v.MarkPrice = markPrice                       // separate from Signal.Price for downstream
	v.ClosedClose = candles[len(candles)-1].Close // closed-bar reference (engine sees this)
	if markPrice > 0 {
		v.Signal.Price = markPrice // override for display only; engine math unchanged
	}
	v.ChartJSON = buildChartJSON(v)
	for _, h := range v.Signal.VP.HVN {
		v.HVNValues = append(v.HVNValues, fmt.Sprintf("%.4f", h))
	}

	// Diagnose now: score the engine's actual plan against the live market.
	//
	// When the engine has a plan (Side != Flat), the diagnostic evaluates
	// THAT plan's entry — so the score on the dashboard answers "is the
	// plan as displayed still good?" and chase math correctly fires when
	// market has moved past the plan entry. When Flat (no plan), we score
	// "if you tried to trade at the mark right now" for both sides and
	// surface the higher one.
	const dashboardFeeBps = 6.0
	mark := v.Signal.Price // live mark, or closed-bar close fallback
	switch v.Signal.Side {
	case signal.Long:
		entry := mark
		if v.Signal.Plan.Entry > 0 {
			entry = v.Signal.Plan.Entry
		}
		r := validator.Validate(sym, tf, signal.Long, entry, dashboardFeeBps, candles, mark)
		v.Diagnose = &r
	case signal.Short:
		entry := mark
		if v.Signal.Plan.Entry > 0 {
			entry = v.Signal.Plan.Entry
		}
		r := validator.Validate(sym, tf, signal.Short, entry, dashboardFeeBps, candles, mark)
		v.Diagnose = &r
	default: // Flat — score both sides at the mark, pick higher
		long := validator.Validate(sym, tf, signal.Long, mark, dashboardFeeBps, candles, mark)
		short := validator.Validate(sym, tf, signal.Short, mark, dashboardFeeBps, candles, mark)
		if short.Total > long.Total {
			v.Diagnose = &short
		} else {
			v.Diagnose = &long
		}
	}
	return v
}

// recommendedAnchors mirrors the canonical labels printed by `journal anchors`.
// Exposed via datalist on the new-trade form so callers get auto-complete
// without losing the freedom to type anything else.
var recommendedAnchors = []string{
	"sweep-low", "sweep-high",
	"fib-uptrend", "fib-downtrend",
	"boll-lower", "boll-upper",
	"hvn-support", "hvn-resistance",
	"daily-open", "weekly-open",
	"discretionary", "manual",
}

// handleJournalNew renders the new-trade form, pre-filled from query params
// when invoked from a dashboard "Record this trade" button.
func (s *server) handleJournalNew(c *gin.Context) {
	// analyzed_at — prefer the URL query (set by the dashboard's recordHref
	// to the last closed bar's CloseTime, so fill-detection has a precise
	// floor). Fall back to "now" for /validate-style flows or direct visits.
	analyzedAt := c.Query("analyzed_at")
	if analyzedAt == "" {
		analyzedAt = time.Now().Local().Format("2006-01-02T15:04")
	}
	c.HTML(http.StatusOK, "journal_new.html", gin.H{
		"Symbol":     c.Query("symbol"),
		"Side":       c.Query("side"),
		"Entry":      c.Query("entry"),
		"Stop":       c.Query("stop"),
		"TP1":        c.Query("tp1"),
		"TP2":        c.Query("tp2"),
		"Anchor":     c.Query("anchor"),
		"TF":         c.Query("tf"),
		"Score":      c.Query("score"),
		"Notes":      "",
		"AnalyzedAt": analyzedAt,
		"SignalCtx":  c.Query("ctx"), // verbatim from recordHref / validateRecordHref
		"Symbols":    []string{"BTC", "ETH", "XAU", "XAG"},
		"Anchors":    recommendedAnchors,
		"Error":      "",
	})
}

// handleJournalOpen processes the new-trade POST. On validation error, the
// form is re-rendered with the user's values preserved and an error banner.
// On success, redirect to /journal so the new entry shows at the top.
func (s *server) handleJournalOpen(c *gin.Context) {
	// Pull all fields up-front so we can re-render on error.
	symbol := strings.ToUpper(strings.TrimSpace(c.PostForm("symbol")))
	side := strings.ToLower(strings.TrimSpace(c.PostForm("side")))
	entryStr := strings.TrimSpace(c.PostForm("entry"))
	stopStr := strings.TrimSpace(c.PostForm("stop"))
	tp1Str := strings.TrimSpace(c.PostForm("tp1"))
	tp2Str := strings.TrimSpace(c.PostForm("tp2"))
	anchor := strings.TrimSpace(c.PostForm("anchor"))
	tf := strings.TrimSpace(c.PostForm("tf"))
	score := strings.TrimSpace(c.PostForm("score"))
	notes := strings.TrimSpace(c.PostForm("notes"))
	analyzedAtStr := strings.TrimSpace(c.PostForm("analyzed_at"))
	leverageStr := strings.TrimSpace(c.PostForm("leverage"))

	rerender := func(errMsg string) {
		c.HTML(http.StatusOK, "journal_new.html", gin.H{
			"Symbol": symbol, "Side": side, "Entry": entryStr, "Stop": stopStr,
			"TP1": tp1Str, "TP2": tp2Str, "Anchor": anchor, "TF": tf,
			"Score": score, "Notes": notes, "AnalyzedAt": analyzedAtStr,
			"Leverage": leverageStr,
			"Symbols":  []string{"BTC", "ETH", "XAU", "XAG"},
			"Anchors":  recommendedAnchors,
			"Error":    errMsg,
		})
	}

	if symbol == "" || (side != "long" && side != "short") {
		rerender("symbol and side are required")
		return
	}
	entry, err := parseFloatPositive(entryStr, "entry")
	if err != nil {
		rerender(err.Error())
		return
	}
	stop, err := parseFloatPositive(stopStr, "stop")
	if err != nil {
		rerender(err.Error())
		return
	}
	tp1, err := parseFloatPositive(tp1Str, "tp1")
	if err != nil {
		rerender(err.Error())
		return
	}
	tp2, err := parseFloatPositive(tp2Str, "tp2")
	if err != nil {
		rerender(err.Error())
		return
	}

	// Sanity: directional consistency between entry/stop/TPs.
	if side == "long" && stop >= entry {
		rerender("for a long, stop must be below entry")
		return
	}
	if side == "short" && stop <= entry {
		rerender("for a short, stop must be above entry")
		return
	}

	now := time.Now().UTC()
	analyzedAt, err := journal.ParseTimeSpec(analyzedAtStr, now)
	if err != nil {
		rerender("analyzed_at: " + err.Error())
		return
	}

	trades, err := journal.ReadAll("")
	if err != nil {
		rerender("read journal: " + err.Error())
		return
	}
	var leverage int
	if leverageStr != "" {
		n, err := strconv.Atoi(leverageStr)
		if err != nil || n < 0 || n > 500 {
			rerender("leverage must be an integer 0-500")
			return
		}
		leverage = n
	}

	t := journal.Trade{
		ID:         journal.NextID(trades),
		OpenedAt:   now,
		AnalyzedAt: analyzedAt,
		Symbol:     symbol,
		Side:       side,
		TF:         tf,
		Score:      score,
		Entry:      entry,
		Stop:       stop,
		TP1:        tp1,
		TP2:        tp2,
		Anchor:     anchor,
		OpenNotes:  notes,
		Leverage:   leverage,
		SignalCtx:  strings.TrimSpace(c.PostForm("signal_ctx")),
	}
	trades = append(trades, t)
	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}
	c.Redirect(http.StatusSeeOther, "/journal")
}

// handleJournalCloseForm renders the close form for a given trade.
func (s *server) handleJournalCloseForm(c *gin.Context) {
	t, idx, trades, err := s.loadTradeByID(c)
	if err != nil {
		c.HTML(http.StatusNotFound, "journal_close.html", gin.H{"Error": err.Error()})
		return
	}
	_ = trades
	_ = idx
	if !t.IsOpen() {
		c.HTML(http.StatusBadRequest, "journal_close.html", gin.H{
			"Error": fmt.Sprintf("trade #%d is already closed", t.ID),
			"Trade": t,
		})
		return
	}
	c.HTML(http.StatusOK, "journal_close.html", gin.H{
		"Trade":     t,
		"Outcome":   "",
		"ExitPrice": "",
		// Carry over any close_notes the user may have written earlier via
		// /edit (e.g. "legs: 50% @ 3100" recorded mid-trade) so the partials
		// widget can restore the prior legs.
		"Notes": t.CloseNotes,
		"Now":   time.Now().Local().Format("2006-01-02T15:04"),
		"Error": "",
	})
}

// handleJournalClosePost processes the close form, computes realized R, writes.
func (s *server) handleJournalClosePost(c *gin.Context) {
	t, idx, trades, err := s.loadTradeByID(c)
	if err != nil {
		c.HTML(http.StatusNotFound, "journal_close.html", gin.H{"Error": err.Error()})
		return
	}
	if !t.IsOpen() {
		c.HTML(http.StatusBadRequest, "journal_close.html", gin.H{
			"Error": fmt.Sprintf("trade #%d is already closed", t.ID),
			"Trade": t,
		})
		return
	}

	outcome := strings.ToLower(strings.TrimSpace(c.PostForm("outcome")))
	exitStr := strings.TrimSpace(c.PostForm("exit_price"))
	notes := strings.TrimSpace(c.PostForm("notes"))
	closedAtStr := strings.TrimSpace(c.PostForm("closed_at"))

	rerender := func(errMsg string) {
		c.HTML(http.StatusOK, "journal_close.html", gin.H{
			"Trade":     t,
			"Outcome":   outcome,
			"ExitPrice": exitStr,
			"Notes":     notes,
			"ClosedAt":  closedAtStr,
			"Now":       time.Now().Local().Format("2006-01-02T15:04"),
			"Error":     errMsg,
		})
	}

	switch outcome {
	case "tp1", "tp2", "stop", "manual", "timeout", "no-fill":
	default:
		rerender("outcome must be tp1, tp2, stop, manual, timeout, or no-fill")
		return
	}
	// no-fill: plan never triggered, so exit_price is irrelevant and R is
	// always 0. Just record the close time + notes for the discipline log.
	var exit float64
	if outcome == "no-fill" {
		exit = 0
	} else {
		exit, err = parseFloatPositive(exitStr, "exit price")
		if err != nil {
			rerender(err.Error())
			return
		}
	}
	closedAt := time.Now().UTC()
	if closedAtStr != "" {
		if tt, perr := journal.ParseTimeSpec(closedAtStr, closedAt); perr == nil {
			closedAt = tt
		} else {
			rerender("closed_at: " + perr.Error())
			return
		}
	}

	trades[idx].ClosedAt = closedAt
	trades[idx].ExitPrice = exit
	trades[idx].Outcome = outcome
	trades[idx].CloseNotes = notes
	if outcome == "no-fill" {
		trades[idx].RRealized = 0
	} else {
		trades[idx].RRealized = journal.RealizedR(trades[idx], exit)
	}

	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}
	c.Redirect(http.StatusSeeOther, "/journal")
}

// handleJournalEditForm renders the full-edit form for a trade.
func (s *server) handleJournalEditForm(c *gin.Context) {
	t, _, _, err := s.loadTradeByID(c)
	if err != nil {
		c.HTML(http.StatusNotFound, "journal_edit.html", editFormData(journal.Trade{}, err.Error()))
		return
	}
	c.HTML(http.StatusOK, "journal_edit.html", editFormData(t, ""))
}

// handleJournalEditPost applies field-by-field changes from the form. Handles
// open↔closed transitions and auto-recomputes R when the trade is closed.
func (s *server) handleJournalEditPost(c *gin.Context) {
	_, idx, trades, err := s.loadTradeByID(c)
	if err != nil {
		c.HTML(http.StatusNotFound, "journal_edit.html", editFormData(journal.Trade{}, err.Error()))
		return
	}

	t := trades[idx]
	rerender := func(errMsg string) {
		// Re-render with the user's just-typed values, not the stored row.
		t.Symbol = strings.ToUpper(strings.TrimSpace(c.PostForm("symbol")))
		t.Side = strings.ToLower(strings.TrimSpace(c.PostForm("side")))
		t.TF = strings.TrimSpace(c.PostForm("tf"))
		t.Score = strings.TrimSpace(c.PostForm("score"))
		t.Anchor = strings.TrimSpace(c.PostForm("anchor"))
		t.OpenNotes = strings.TrimSpace(c.PostForm("open_notes"))
		t.CloseNotes = strings.TrimSpace(c.PostForm("close_notes"))
		t.Outcome = strings.ToLower(strings.TrimSpace(c.PostForm("outcome")))
		c.HTML(http.StatusOK, "journal_edit.html", editFormData(t, errMsg))
	}

	// Required string fields
	sym := strings.ToUpper(strings.TrimSpace(c.PostForm("symbol")))
	side := strings.ToLower(strings.TrimSpace(c.PostForm("side")))
	if sym == "" || (side != "long" && side != "short") {
		rerender("symbol and side are required")
		return
	}

	entry, err := parseFloatPositive(c.PostForm("entry"), "entry")
	if err != nil {
		rerender(err.Error())
		return
	}
	stop, err := parseFloatPositive(c.PostForm("stop"), "stop")
	if err != nil {
		rerender(err.Error())
		return
	}
	tp1, err := parseFloatPositive(c.PostForm("tp1"), "tp1")
	if err != nil {
		rerender(err.Error())
		return
	}
	tp2, err := parseFloatPositive(c.PostForm("tp2"), "tp2")
	if err != nil {
		rerender(err.Error())
		return
	}
	if side == "long" && stop >= entry {
		rerender("for a long, stop must be below entry")
		return
	}
	if side == "short" && stop <= entry {
		rerender("for a short, stop must be above entry")
		return
	}

	openedAt, err := journal.ParseTimeSpec(c.PostForm("opened_at"), time.Now().UTC())
	if err != nil {
		rerender("opened_at: " + err.Error())
		return
	}
	analyzedAt, err := journal.ParseTimeSpec(c.PostForm("analyzed_at"), openedAt)
	if err != nil {
		rerender("analyzed_at: " + err.Error())
		return
	}

	// filled_at: blank → still pending; non-blank → entry was triggered.
	filledAtStr := strings.TrimSpace(c.PostForm("filled_at"))
	var filledAt time.Time
	if filledAtStr != "" {
		filledAt, err = journal.ParseTimeSpec(filledAtStr, time.Now().UTC())
		if err != nil {
			rerender("filled_at: " + err.Error())
			return
		}
	}

	// closed_at: blank → trade is open; non-blank → trade is closed.
	closedAtStr := strings.TrimSpace(c.PostForm("closed_at"))
	var closedAt time.Time
	if closedAtStr != "" {
		closedAt, err = journal.ParseTimeSpec(closedAtStr, time.Now().UTC())
		if err != nil {
			rerender("closed_at: " + err.Error())
			return
		}
	}

	// Outcome + exit only required when trade is closed.
	outcome := strings.ToLower(strings.TrimSpace(c.PostForm("outcome")))
	var exitPrice float64
	exitStr := strings.TrimSpace(c.PostForm("exit_price"))
	if !closedAt.IsZero() {
		switch outcome {
		case "tp1", "tp2", "stop", "manual", "timeout", "no-fill":
		default:
			rerender("outcome required when closed_at is set")
			return
		}
		if outcome == "no-fill" {
			exitPrice = 0
		} else {
			exitPrice, err = parseFloatPositive(exitStr, "exit price")
			if err != nil {
				rerender(err.Error())
				return
			}
		}
	} else {
		// Re-opening: clear close-only fields.
		outcome = ""
		exitPrice = 0
	}

	// Leverage (optional, 0 = "not recorded").
	var leverage int
	if levStr := strings.TrimSpace(c.PostForm("leverage")); levStr != "" {
		n, err := strconv.Atoi(levStr)
		if err != nil || n < 0 || n > 500 {
			rerender("leverage must be an integer 0-500")
			return
		}
		leverage = n
	}

	// Apply mutations
	trades[idx].Symbol = sym
	trades[idx].Side = side
	trades[idx].TF = strings.TrimSpace(c.PostForm("tf"))
	trades[idx].Score = strings.TrimSpace(c.PostForm("score"))
	trades[idx].Anchor = strings.TrimSpace(c.PostForm("anchor"))
	trades[idx].OpenNotes = strings.TrimSpace(c.PostForm("open_notes"))
	trades[idx].CloseNotes = strings.TrimSpace(c.PostForm("close_notes"))
	trades[idx].Leverage = leverage
	trades[idx].Entry = entry
	trades[idx].Stop = stop
	trades[idx].TP1 = tp1
	trades[idx].TP2 = tp2
	trades[idx].OpenedAt = openedAt
	trades[idx].AnalyzedAt = analyzedAt
	trades[idx].FilledAt = filledAt
	trades[idx].ClosedAt = closedAt
	trades[idx].Outcome = outcome
	trades[idx].ExitPrice = exitPrice
	trades[idx].SignalCtx = strings.TrimSpace(c.PostForm("signal_ctx"))
	switch {
	case closedAt.IsZero():
		trades[idx].RRealized = 0
	case outcome == "no-fill":
		trades[idx].RRealized = 0
	default:
		trades[idx].RRealized = journal.RealizedR(trades[idx], exitPrice)
	}

	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}
	c.Redirect(http.StatusSeeOther, "/journal")
}

// handleJournalDelete removes a trade by ID (POST only).
func (s *server) handleJournalDelete(c *gin.Context) {
	_, _, trades, err := s.loadTradeByID(c)
	if err != nil {
		c.String(http.StatusNotFound, "%s", err.Error())
		return
	}
	idStr := c.Param("id")
	id, _ := strconv.Atoi(idStr)
	out := make([]journal.Trade, 0, len(trades))
	for _, t := range trades {
		if t.ID == id {
			continue
		}
		out = append(out, t)
	}
	if err := journal.WriteAll("", out); err != nil {
		c.String(http.StatusInternalServerError, "write journal: %s", err.Error())
		return
	}
	c.Redirect(http.StatusSeeOther, "/journal")
}

// editFormData packs a Trade and an error into the template's gin.H. Times
// are rendered for HTML datetime-local inputs (no timezone, local).
func editFormData(t journal.Trade, errMsg string) gin.H {
	asLocalInput := func(tm time.Time) string {
		if tm.IsZero() {
			return ""
		}
		return tm.Local().Format("2006-01-02T15:04")
	}
	exitStr := ""
	if t.ExitPrice != 0 {
		exitStr = fmt.Sprintf("%.4f", t.ExitPrice)
	}
	return gin.H{
		"Trade":      t,
		"OpenedAt":   asLocalInput(t.OpenedAt),
		"AnalyzedAt": asLocalInput(t.AnalyzedAt),
		"FilledAt":   asLocalInput(t.FilledAt),
		"ClosedAt":   asLocalInput(t.ClosedAt),
		"Entry":      fmt.Sprintf("%.4f", t.Entry),
		"Stop":       fmt.Sprintf("%.4f", t.Stop),
		"TP1":        fmt.Sprintf("%.4f", t.TP1),
		"TP2":        fmt.Sprintf("%.4f", t.TP2),
		"ExitPrice":  exitStr,
		"Symbols":    []string{"BTC", "ETH", "XAU", "XAG"},
		"Anchors":    recommendedAnchors,
		"Error":      errMsg,
	}
}

// loadTradeByID is a shared helper that pulls the trade slice + the one we're
// editing. Returns an error if the :id param is missing/invalid.
func (s *server) loadTradeByID(c *gin.Context) (journal.Trade, int, []journal.Trade, error) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return journal.Trade{}, 0, nil, fmt.Errorf("invalid id %q", idStr)
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		return journal.Trade{}, 0, nil, err
	}
	idx := journal.FindByID(trades, id)
	if idx < 0 {
		return journal.Trade{}, 0, nil, fmt.Errorf("no trade with id %d", id)
	}
	return trades[idx], idx, trades, nil
}

func parseFloatPositive(s, name string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	v, err := parseFloat(s)
	if err != nil {
		return 0, fmt.Errorf("%s: not a number", name)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s must be > 0", name)
	}
	return v, nil
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscan(s, &f)
	return f, err
}

// handleJournalList renders all journal entries newest-first, with summary stats.
func (s *server) handleJournalList(c *gin.Context) {
	trades, err := journal.ReadAll("")
	if err != nil {
		c.HTML(http.StatusInternalServerError, "journal_list.html", gin.H{
			"Error": err.Error(),
		})
		return
	}
	journal.SortByOpenedDesc(trades)

	// Compute summary stats over closed trades. No-fills are tracked
	// separately and excluded from WR/R aggregates — they're plan
	// records, not trades. Open trades are split into pending (entry
	// not yet filled) and active (filled, not yet closed) for the
	// hero pills.
	var totalR, bestR, worstR float64
	wins, closedCount, noFillCount := 0, 0, 0
	pendingCount, activeCount := 0, 0
	for _, t := range trades {
		if t.IsPending() {
			pendingCount++
			continue
		}
		if t.IsActive() {
			activeCount++
			continue
		}
		if t.IsNoFill() {
			noFillCount++
			continue
		}
		// Anything that reaches here is a closed (non-no-fill) trade.
		closedCount++
		totalR += t.RRealized
		if t.RRealized > 0 {
			wins++
		}
		if t.RRealized > bestR {
			bestR = t.RRealized
		}
		if closedCount == 1 || t.RRealized < worstR {
			worstR = t.RRealized
		}
	}
	wr := 0.0
	avgR := 0.0
	if closedCount > 0 {
		wr = float64(wins) / float64(closedCount) * 100
		avgR = totalR / float64(closedCount)
	}

	histogram := buildRHistogram(trades)
	calendar := buildDailyCalendar(trades, 42) // 6 weeks
	periods := buildPeriodStats(trades)
	equity := buildEquityCurve(trades)

	// Pagination — 10 per page, ?page=N (1-indexed). All stats above are
	// computed over the FULL trade set; pagination only chunks the list
	// rendered in the Journal section.
	const perPage = 10
	totalTrades := len(trades)
	totalPages := (totalTrades + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}
	page := 1
	if p, err := strconv.Atoi(c.Query("page")); err == nil && p >= 1 && p <= totalPages {
		page = p
	}
	pageStart := (page - 1) * perPage
	pageEnd := pageStart + perPage
	if pageEnd > totalTrades {
		pageEnd = totalTrades
	}
	pageTrades := trades[pageStart:pageEnd]
	// Build a list of page numbers for the pager (1..totalPages). Small
	// enough to show all; if it ever blows past ~30 we'd add ellipsis logic.
	pageNums := make([]int, 0, totalPages)
	for i := 1; i <= totalPages; i++ {
		pageNums = append(pageNums, i)
	}

	c.HTML(http.StatusOK, "journal_list.html", gin.H{
		"Trades":       pageTrades,
		"TotalTrades":  totalTrades,
		"Page":         page,
		"TotalPages":   totalPages,
		"PageNums":     pageNums,
		"OpenCount":    pendingCount + activeCount, // legacy alias — total still-open
		"PendingCount": pendingCount,
		"ActiveCount":  activeCount,
		"ClosedCount":  closedCount,
		"NoFillCount":  noFillCount,
		"WR":           wr,
		"AvgR":         avgR,
		"TotalR":       totalR,
		"BestR":        bestR,
		"WorstR":       worstR,
		"Histogram":    histogram,
		"Calendar":     calendar,
		"Periods":      periods,
		"Equity":       equity,
	})
}

// equityPoint is one node on the cumulative R curve — one per closed trade,
// plus a (0, 0) origin so the line starts at zero R.
type equityPoint struct {
	Index int       // trade ordinal; 0 = origin
	R     float64   // cumulative R after this trade
	Date  time.Time // close time (zero on origin)
	X, Y  float64   // SVG coordinates (0-100 viewBox)
}

// equityCurve packages the cumulative-R series into the values the template
// needs to render an SVG: a line path, a closed-area path (for the gradient
// fill), key reference numbers (peak / current / drawdown), and trend color.
// All viewBox math is done server-side so the template is dumb rendering.
type equityCurve struct {
	HasData      bool
	Points       []equityPoint
	LinePath     string  // SVG `d` for the line itself
	AreaPath     string  // SVG `d` for the filled area below the line
	ZeroY        float64 // Y coordinate of the 0R reference line in viewBox
	PeakR        float64
	PeakIdx      int
	CurrentR     float64
	Drawdown     float64 // distance from peak to current (positive number)
	MinR, MaxR   float64
	Trend        string // "up" | "down" | "flat" — controls line color
	FirstClose   time.Time
	LastClose    time.Time
	ViewBoxW     int
	ViewBoxH     int
}

// buildEquityCurve builds the cumulative-R series sorted by close time,
// then maps it into a 100×40 viewBox for SVG rendering. Origin (0, 0R) is
// always the leftmost point so the line visually starts at the baseline.
func buildEquityCurve(trades []journal.Trade) equityCurve {
	// Closed trades sorted by ClosedAt ascending. No-fills don't move the
	// equity curve so they're excluded — including them would emit flat
	// points that misrepresent the R progression.
	var closed []journal.Trade
	for _, t := range trades {
		if !t.IsOpen() && !t.ClosedAt.IsZero() && !t.IsNoFill() {
			closed = append(closed, t)
		}
	}
	if len(closed) == 0 {
		return equityCurve{}
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].ClosedAt.Before(closed[j].ClosedAt) })

	// Build points: origin + one per trade.
	pts := []equityPoint{{Index: 0, R: 0}}
	cum := 0.0
	peak := 0.0
	peakIdx := 0
	for i, t := range closed {
		cum += t.RRealized
		if cum > peak {
			peak = cum
			peakIdx = i + 1
		}
		pts = append(pts, equityPoint{
			Index: i + 1,
			R:     cum,
			Date:  t.ClosedAt.Local(),
		})
	}

	// Find Y bounds with a small headroom so the line doesn't touch the edge.
	minR, maxR := 0.0, 0.0
	for _, p := range pts {
		if p.R < minR {
			minR = p.R
		}
		if p.R > maxR {
			maxR = p.R
		}
	}
	// Pad bounds by 10% of range (or 0.5R floor) so the chart breathes.
	pad := (maxR - minR) * 0.10
	if pad < 0.5 {
		pad = 0.5
	}
	yLo, yHi := minR-pad, maxR+pad

	const vbW, vbH = 100.0, 40.0
	xStep := vbW / float64(len(pts)-1)
	if len(pts) == 1 {
		xStep = vbW
	}
	yScale := vbH / (yHi - yLo)

	// Compute screen coords for each point (Y is inverted in SVG — origin top).
	for i := range pts {
		pts[i].X = float64(i) * xStep
		pts[i].Y = vbH - (pts[i].R-yLo)*yScale
	}
	zeroY := vbH - (0-yLo)*yScale

	// Build line path: "M x,y L x,y L x,y ..."
	var line, area strings.Builder
	for i, p := range pts {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s %.2f %.2f ", cmd, p.X, p.Y)
	}
	// Build area path: line + close to bottom-right + bottom-left, back to start.
	area.WriteString(line.String())
	fmt.Fprintf(&area, "L %.2f %.2f L %.2f %.2f Z", vbW, vbH, 0.0, vbH)

	currentR := pts[len(pts)-1].R
	trend := "flat"
	switch {
	case currentR > 0.01:
		trend = "up"
	case currentR < -0.01:
		trend = "down"
	}

	drawdown := peak - currentR
	if drawdown < 0 {
		drawdown = 0
	}

	return equityCurve{
		HasData:    true,
		Points:     pts,
		LinePath:   strings.TrimSpace(line.String()),
		AreaPath:   strings.TrimSpace(area.String()),
		ZeroY:      zeroY,
		PeakR:      peak,
		PeakIdx:    peakIdx,
		CurrentR:   currentR,
		Drawdown:   drawdown,
		MinR:       minR,
		MaxR:       maxR,
		Trend:      trend,
		FirstClose: closed[0].ClosedAt.Local(),
		LastClose:  closed[len(closed)-1].ClosedAt.Local(),
		ViewBoxW:   int(vbW),
		ViewBoxH:   int(vbH),
	}
}

// periodStats holds R + trade counts across rolling time windows for the
// portfolio hero. Mirrors the "today / week / month / all-time" rows that
// every major exchange portfolio screen surfaces at the top.
type periodStats struct {
	TodayR     float64
	TodayN     int
	WeekR      float64 // since Monday 00:00 local
	WeekN      int
	MonthR     float64 // since 1st of this month 00:00 local
	MonthN     int
	AllTimeR   float64
	AllTimeN   int
}

func buildPeriodStats(trades []journal.Trade) periodStats {
	now := time.Now().Local()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// Week starts Monday in this project's convention (matches calendar).
	weekStart := todayStart
	for weekStart.Weekday() != time.Monday {
		weekStart = weekStart.AddDate(0, 0, -1)
	}
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var p periodStats
	for _, t := range trades {
		if t.IsOpen() || t.ClosedAt.IsZero() || t.IsNoFill() {
			continue // no-fill plans don't count in R / WR aggregates
		}
		closedLocal := t.ClosedAt.Local()
		p.AllTimeR += t.RRealized
		p.AllTimeN++
		if !closedLocal.Before(monthStart) {
			p.MonthR += t.RRealized
			p.MonthN++
		}
		if !closedLocal.Before(weekStart) {
			p.WeekR += t.RRealized
			p.WeekN++
		}
		if !closedLocal.Before(todayStart) {
			p.TodayR += t.RRealized
			p.TodayN++
		}
	}
	return p
}

// rBucket is one column in the R-distribution histogram.
type rBucket struct {
	Label     string  // e.g. "-1R", "+1R", "+2R"
	Lo, Hi    float64 // bucket bounds (inclusive low, exclusive high)
	Count     int
	PctHeight float64 // 0-100, scaled to tallest bucket
	IsPos     bool    // green vs red bar color
}

// buildRHistogram bucks closed trades' realized R into fixed-width bins
// from -3R to +3R in 0.5R steps. Anything beyond the edges is clamped
// into the outermost bucket (rare; signal stop is -1R by design).
func buildRHistogram(trades []journal.Trade) []rBucket {
	// Bucket boundaries: -3, -2.5, -2, ... +2.5, +3 → 12 buckets.
	bounds := []float64{-3, -2.5, -2, -1.5, -1, -0.5, 0, 0.5, 1, 1.5, 2, 2.5, 3}
	labels := []string{"≤-2.5", "-2", "-1.5", "-1", "-0.5", "-0+", "0+", "+0.5", "+1", "+1.5", "+2", "+2.5+"}
	buckets := make([]rBucket, len(labels))
	for i := range buckets {
		buckets[i] = rBucket{
			Label: labels[i],
			Lo:    bounds[i],
			Hi:    bounds[i+1],
			IsPos: bounds[i] >= 0,
		}
	}
	maxCount := 0
	for _, t := range trades {
		if t.IsOpen() || t.IsNoFill() {
			continue
		}
		r := t.RRealized
		// Find the bucket — clamp to ends if out of range.
		idx := len(buckets) - 1
		if r < bounds[0] {
			idx = 0
		} else if r < bounds[len(bounds)-1] {
			for i := 0; i < len(buckets); i++ {
				if r >= bounds[i] && r < bounds[i+1] {
					idx = i
					break
				}
			}
		}
		buckets[idx].Count++
		if buckets[idx].Count > maxCount {
			maxCount = buckets[idx].Count
		}
	}
	if maxCount > 0 {
		for i := range buckets {
			buckets[i].PctHeight = float64(buckets[i].Count) / float64(maxCount) * 100
		}
	}
	return buckets
}

// dailyCell is one square in the daily-R calendar (one day).
type dailyCell struct {
	Date    time.Time // local date midnight
	Day     int       // day-of-month for display
	IsToday bool
	Count   int     // trades closed that day
	TotalR  float64 // sum of R for that day
	// Intensity is the absolute R normalized to 0-100 for color saturation,
	// capped to a sensible max so a +5R day doesn't make every other day
	// invisible.
	Intensity float64
	IsPos     bool
	IsZero    bool // no trades closed that day
}

// buildDailyCalendar builds a weeks-tall grid of the last `days` days
// (default 42 = 6 weeks). Each cell shows the day's net R from closed
// trades. Renders Mon-Sun rows, with the most recent week at the bottom
// (GitHub-style).
func buildDailyCalendar(trades []journal.Trade, days int) [][]dailyCell {
	// Aggregate closed R by local date.
	type agg struct {
		count int
		r     float64
	}
	byDate := map[string]*agg{}
	for _, t := range trades {
		if t.IsOpen() || t.ClosedAt.IsZero() || t.IsNoFill() {
			continue
		}
		key := t.ClosedAt.Local().Format("2006-01-02")
		a, ok := byDate[key]
		if !ok {
			a = &agg{}
			byDate[key] = a
		}
		a.count++
		a.r += t.RRealized
	}

	// Find max absolute daily R for color scaling.
	var maxAbs float64 = 1.0 // floor so a single +0.5R day isn't max intensity
	for _, a := range byDate {
		if abs := a.r; abs < 0 {
			abs = -abs
		}
		if absR := a.r; absR < 0 {
			absR = -absR
			if absR > maxAbs {
				maxAbs = absR
			}
		} else if absR > maxAbs {
			maxAbs = absR
		}
	}

	// Walk from `days-1` ago up to today; align grid so Monday starts each row.
	now := time.Now().Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today.AddDate(0, 0, -(days - 1))
	// Back up `start` so its weekday is Monday — leading cells will be empty.
	for start.Weekday() != time.Monday {
		start = start.AddDate(0, 0, -1)
	}

	var grid [][]dailyCell
	var row []dailyCell
	d := start
	end := today.AddDate(0, 0, 1) // exclusive
	for d.Before(end) {
		cell := dailyCell{Date: d, Day: d.Day(), IsToday: d.Equal(today)}
		key := d.Format("2006-01-02")
		if a, ok := byDate[key]; ok {
			cell.Count = a.count
			cell.TotalR = a.r
			cell.IsPos = a.r >= 0
			abs := a.r
			if abs < 0 {
				abs = -abs
			}
			// Linear scaling alone makes small-R days nearly invisible when
			// any single big-R day pushes maxAbs high. Floor at 40 so a
			// quiet day still reads as colored, not black; ceiling at 100.
			scaled := abs / maxAbs * 100
			if scaled < 40 {
				scaled = 40
			}
			if scaled > 100 {
				scaled = 100
			}
			cell.Intensity = scaled
		} else if d.Before(today.AddDate(0, 0, -(days - 1))) {
			cell.IsZero = true // padding before the requested window
		} else {
			cell.IsZero = true // window day with no trades
		}
		row = append(row, cell)
		if d.Weekday() == time.Sunday {
			grid = append(grid, row)
			row = nil
		}
		d = d.AddDate(0, 0, 1)
	}
	if len(row) > 0 {
		// Pad last row to 7 cells.
		for len(row) < 7 {
			row = append(row, dailyCell{IsZero: true})
		}
		grid = append(grid, row)
	}
	return grid
}

// buildSignalCtx packs the validator + engine state at signal moment
// into a compact key=value string for the journal's signal_ctx column.
// Shape: "v=5.5;ver=TAKE;d=up;st=1;va=at_VAL;f=-0.0004"
// All fields are optional — emit only what's meaningful.
func buildSignalCtx(r *validator.Result, fundingRate float64) string {
	if r == nil {
		return ""
	}
	parts := make([]string, 0, 8)
	if r.Total > 0 {
		parts = append(parts, fmt.Sprintf("v=%.1f", r.Total))
	}
	if v := shortVerdictTag(r.Verdict); v != "" {
		parts = append(parts, "ver="+v)
	}
	switch r.POCMig.Trend {
	case indicator.POCRising:
		parts = append(parts, "d=up")
	case indicator.POCFalling:
		parts = append(parts, "d=down")
	case indicator.POCFlat:
		// skip — flat drift adds noise without info
	}
	if r.POCMig.Stacked {
		parts = append(parts, "st=1")
	}
	switch {
	case r.AtVAH:
		parts = append(parts, "va=at_VAH")
	case r.AtVAL:
		parts = append(parts, "va=at_VAL")
	case r.InsideVA:
		parts = append(parts, "va=in")
	case r.OutsideVAUp:
		parts = append(parts, "va=above")
	case r.OutsideVADn:
		parts = append(parts, "va=below")
	}
	if fundingRate != 0 {
		parts = append(parts, fmt.Sprintf("f=%.5f", fundingRate))
	}
	if r.RecentFlashBarBearish {
		parts = append(parts, "knife=1")
	}
	if r.RecentFlashBarBullish {
		parts = append(parts, "squeeze=1")
	}
	return strings.Join(parts, ";")
}

// shortVerdictTag returns a compact tag for the journal snapshot.
// The full Verdict string is e.g. "STRONG TAKE — full size"; we keep
// just the headline word for parseability.
func shortVerdictTag(v string) string {
	switch {
	case strings.HasPrefix(v, "STRONG"):
		return "STRONG"
	case strings.HasPrefix(v, "TAKE"):
		return "TAKE"
	case strings.HasPrefix(v, "NEUTRAL"):
		return "NEUTRAL"
	case strings.HasPrefix(v, "WEAK"):
		return "WEAK"
	case strings.HasPrefix(v, "AVOID"):
		return "AVOID"
	}
	return ""
}

// templateFuncs exposes formatting helpers to the templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"fmtPrice": func(v float64) string {
			if v == 0 {
				return "-"
			}
			return fmt.Sprintf("%.4f", v)
		},
		"fmtPct": func(v float64) string {
			return fmt.Sprintf("%+.4f%%", v*100)
		},
		"mulPct": func(v float64) float64 { return v * 100 },
		"driftArrow": func(t indicator.POCTrend) string {
			switch t {
			case indicator.POCRising:
				return "↗"
			case indicator.POCFalling:
				return "↘"
			default:
				return "→"
			}
		},
		"isPOCRising":  func(t indicator.POCTrend) bool { return t == indicator.POCRising },
		"isPOCFalling": func(t indicator.POCTrend) bool { return t == indicator.POCFalling },
		"fmtOI": func(v float64) string {
			if v >= 1e9 {
				return fmt.Sprintf("%.2fB", v/1e9)
			}
			if v >= 1e6 {
				return fmt.Sprintf("%.1fM", v/1e6)
			}
			return fmt.Sprintf("%.0f", v)
		},
		"sideClass": func(s signal.Side) string {
			switch s {
			case signal.Long:
				return "side-long"
			case signal.Short:
				return "side-short"
			default:
				return "side-flat"
			}
		},
		"sideText": func(s signal.Side) string {
			return s.String()
		},
		"scoreClass": func(score, min int) string {
			switch {
			case score >= min:
				return "score-good"
			case score == min-1:
				return "score-borderline"
			default:
				return "score-weak"
			}
		},
		"isSweepAnchor": func(anchor string) bool {
			return strings.HasPrefix(anchor, "sweep")
		},
		"hasPlan": func(s signal.Signal) bool {
			return s.Plan.Entry != 0
		},
		"join": func(items []string, sep string) string {
			return strings.Join(items, sep)
		},
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"fmtR": func(r float64) string {
			return fmt.Sprintf("%+.2f", r)
		},
		"rClass": func(r float64) string {
			switch {
			case r > 0:
				return "r-pos"
			case r < 0:
				return "r-neg"
			}
			return "r-zero"
		},
		"outcomeClass": func(t journal.Trade) string {
			// Classify by realized R, not by outcome label. A "manual" exit
			// can be a winning trade (e.g. partial close above breakeven),
			// and labeling it red just because it isn't a clean TP would
			// mislead at a glance.
			if t.IsPending() {
				return "status-pending"
			}
			if t.IsOpen() {
				return "status-open" // = active (filled, not closed)
			}
			if t.IsNoFill() {
				return "status-skip" // distinct from "flat" (0R but no trade actually taken)
			}
			switch {
			case t.RRealized > 0:
				return "status-win"
			case t.RRealized < 0:
				return "status-loss"
			}
			return "status-flat"
		},
		"outcomeText": func(t journal.Trade) string {
			if t.IsPending() {
				return "pending"
			}
			if t.IsOpen() {
				return "active"
			}
			return t.Outcome
		},
		"sideStr": func(s string) string {
			return strings.ToUpper(s)
		},
		"sideClassStr": func(s string) string {
			switch strings.ToLower(s) {
			case "long":
				return "side-long"
			case "short":
				return "side-short"
			}
			return "side-flat"
		},
		"or": func(a, b string) string {
			if a == "" {
				return b
			}
			return a
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"fmtElapsed": func(d time.Duration) string {
			if d < time.Minute {
				return "just now"
			}
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			switch {
			case h >= 24:
				return fmt.Sprintf("%dd %dh", h/24, h%24)
			case h > 0:
				return fmt.Sprintf("%dh %dm", h, m)
			}
			return fmt.Sprintf("%dm", m)
		},
		"sweepStr": func(side analyzer.SweepSide) string {
			if side == analyzer.SweepLow {
				return "low"
			}
			return "high"
		},
		"verdictShort": func(v string) string {
			// "STRONG TAKE — full size" → "STRONG TAKE"
			if i := strings.Index(v, " — "); i > 0 {
				return v[:i]
			}
			return v
		},
		"diagnoseHref": func(v symbolView, tf string) template.URL {
			if v.Diagnose == nil {
				return template.URL("/validate")
			}
			q := url.Values{}
			q.Set("symbol", v.Short)
			q.Set("side", strings.ToLower(v.Diagnose.Side.String()))
			q.Set("entry", fmt.Sprintf("%.4f", v.Diagnose.Entry))
			q.Set("tf", tf)
			return template.URL("/validate?" + q.Encode())
		},
		"verdictClass": func(score float64) string {
			switch {
			case score >= 8:
				return "verdict-strong"
			case score >= 6:
				return "verdict-take"
			case score >= 4:
				return "verdict-neutral"
			case score >= 2:
				return "verdict-weak"
			}
			return "verdict-avoid"
		},
		"factorClass": func(points float64) string {
			switch {
			case points > 0:
				return "factor-pos"
			case points < 0:
				return "factor-neg"
			}
			return "factor-zero"
		},
		"factorSign": func(points float64) string {
			if points >= 0 {
				return "+"
			}
			return ""
		},
		"pctDiff": func(target, ref float64) string {
			if ref == 0 {
				return ""
			}
			return fmt.Sprintf("%+.2f%%", (target-ref)/ref*100)
		},
		"validateRecordHref": func(r validator.Result, short string) template.URL {
			// /validate page records the USER's entry + the Suggested
			// levels derived from it (SuggStop/TP1/TP2 = entry ± 1.5×ATR
			// risk units). Never substitutes the engine's own plan —
			// that defeats the purpose of validating a custom entry.
			q := url.Values{}
			q.Set("symbol", short)
			q.Set("side", strings.ToLower(r.Side.String()))
			q.Set("entry", fmt.Sprintf("%.4f", r.Entry))
			q.Set("stop", fmt.Sprintf("%.4f", r.SuggStop))
			q.Set("tp1", fmt.Sprintf("%.4f", r.SuggTP1))
			q.Set("tp2", fmt.Sprintf("%.4f", r.SuggTP2))
			q.Set("anchor", "manual")
			q.Set("tf", string(r.Timeframe))
			q.Set("score", fmt.Sprintf("v%.1f", r.Total))
			// Funding rate isn't on validator.Result; /validate doesn't
			// fetch it. Pass 0 — snapshot will omit the `f` field.
			if ctx := buildSignalCtx(&r, 0); ctx != "" {
				q.Set("ctx", ctx)
			}
			return template.URL("/journal/new?" + q.Encode())
		},
		"recordHref": func(v symbolView, tf string) template.URL {
			q := url.Values{}
			q.Set("symbol", v.Short)
			q.Set("side", strings.ToLower(v.Signal.Side.String()))
			q.Set("entry", fmt.Sprintf("%.4f", v.Signal.Plan.Entry))
			q.Set("stop", fmt.Sprintf("%.4f", v.Signal.Plan.StopLoss))
			if len(v.Signal.Plan.TakeProfit) >= 1 {
				q.Set("tp1", fmt.Sprintf("%.4f", v.Signal.Plan.TakeProfit[0]))
			}
			if len(v.Signal.Plan.TakeProfit) >= 2 {
				q.Set("tp2", fmt.Sprintf("%.4f", v.Signal.Plan.TakeProfit[1]))
			}
			q.Set("anchor", v.Signal.Plan.Anchor)
			q.Set("tf", tf)
			q.Set("score", fmt.Sprintf("%d", v.Signal.Score))
			// analyzed_at = close time of the last closed bar the engine
			// used to compute this signal. Improves fill-detection
			// accuracy on the resulting trade.
			if len(v.Candles) > 0 {
				q.Set("analyzed_at", v.Candles[len(v.Candles)-1].CloseTime.Local().Format("2006-01-02T15:04"))
			}
			// Signal-context snapshot for journal analytics. Captures
			// validator score + regime chips + funding at click time
			// so we can bucket realized trades by validator band later.
			if ctx := buildSignalCtx(v.Diagnose, v.Context.FundingRate); ctx != "" {
				q.Set("ctx", ctx)
			}
			return template.URL("/journal/new?" + q.Encode())
		},
	}
}

// --- /validate (scored entry validator) ---

// handleValidateForm renders the empty validator form. Optional ?symbol=,
// ?side=, ?entry=, ?tf= query params pre-fill the inputs (so users can
// re-validate a slightly tweaked entry by adjusting URL bookmarks).
func (s *server) handleValidateForm(c *gin.Context) {
	c.HTML(http.StatusOK, "validate_form.html", gin.H{
		"Symbol":    strings.ToUpper(c.Query("symbol")),
		"Side":      strings.ToLower(c.Query("side")),
		"Entry":     c.Query("entry"),
		"TF":        defaultStr(c.Query("tf"), "1h"),
		"FeeBps":    defaultStr(c.Query("fee_bps"), "6"),
		"Symbols":   []string{"BTC", "ETH", "XAU", "XAG"},
		"TFOptions": []string{"15m", "30m", "1h", "2h", "4h", "1d"},
	})
}

// handleValidatePost runs the validator against the proposed entry and
// renders the scored result. Falls back to rerendering the form with an
// error banner on bad inputs / fetch failures.
func (s *server) handleValidatePost(c *gin.Context) {
	symStr := strings.ToUpper(strings.TrimSpace(c.PostForm("symbol")))
	sideStr := strings.ToLower(strings.TrimSpace(c.PostForm("side")))
	tfStr := strings.TrimSpace(c.PostForm("tf"))
	if tfStr == "" {
		tfStr = "1h"
	}

	rerender := func(errMsg string) {
		c.HTML(http.StatusOK, "validate_form.html", gin.H{
			"Symbol":    symStr,
			"Side":      sideStr,
			"Entry":     c.PostForm("entry"),
			"TF":        tfStr,
			"FeeBps":    defaultStr(c.PostForm("fee_bps"), "6"),
			"Symbols":   []string{"BTC", "ETH", "XAU", "XAG"},
			"TFOptions": []string{"15m", "30m", "1h", "2h", "4h", "1d"},
			"Error":     errMsg,
		})
	}

	sym, err := resolveWebSymbol(symStr)
	if err != nil {
		rerender(err.Error())
		return
	}
	var side signal.Side
	switch sideStr {
	case "long":
		side = signal.Long
	case "short":
		side = signal.Short
	default:
		rerender("side must be long or short")
		return
	}
	entry, err := parseFloatPositive(c.PostForm("entry"), "entry")
	if err != nil {
		rerender(err.Error())
		return
	}
	feeBps, err := parseFloatPositive(defaultStr(c.PostForm("fee_bps"), "6"), "fee_bps")
	if err != nil {
		rerender(err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	tf := market.Timeframe(tfStr)
	candles, err := s.client.Klines(ctx, sym, tf, 300)
	if err != nil {
		rerender("fetch klines: " + err.Error())
		return
	}
	if len(candles) < 60 {
		rerender(fmt.Sprintf("only %d candles available, need 60+", len(candles)))
		return
	}

	// Fetch live mark price so the validator's "current market" reference
	// matches what the dashboard shows. Best-effort: on failure, validator
	// falls back to closed-bar close internally.
	var markPrice float64
	if fr, err := s.client.FundingRate(ctx, sym); err == nil {
		markPrice = fr.MarkPrice
	}

	res := validator.Validate(sym, tf, side, entry, feeBps, candles, markPrice)
	c.HTML(http.StatusOK, "validate_result.html", gin.H{
		"R":     res,
		"TF":    tfStr,
		"Short": shortSymbol(sym),
	})
}

// resolveWebSymbol maps the short symbol the form posts (BTC/ETH/XAU/XAG)
// to the full BingX market symbol the engine needs.
func resolveWebSymbol(s string) (market.Symbol, error) {
	switch s {
	case "BTC":
		return market.BTCUSDT, nil
	case "ETH":
		return market.ETHUSDT, nil
	case "XAU":
		return market.XAUUSDT, nil
	case "XAG":
		return market.XAGUSDT, nil
	}
	return "", fmt.Errorf("unknown symbol %q (use BTC / ETH / XAU / XAG)", s)
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
