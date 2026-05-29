package main

import (
	"context"
	"fmt"
	"html/template"
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
	// been closed yet). Independent of the requested TF — we always show
	// open trades regardless of which TF the dashboard is rendering.
	openTrades := buildOpenTradeCards(views)

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
	TimeElapsed time.Duration // since OpenedAt
	Diagnose    *validator.Result
}

func buildOpenTradeCards(views []symbolView) []openTradeCard {
	trades, err := journal.ReadAll("")
	if err != nil {
		return nil
	}
	var cards []openTradeCard
	for _, t := range trades {
		if !t.IsOpen() {
			continue
		}
		card := openTradeCard{Trade: t, TimeElapsed: time.Since(t.OpenedAt)}

		// Find matching symbolView for live mark + diagnose.
		for _, v := range views {
			if v.Short != t.Symbol {
				continue
			}
			card.MarkPrice = v.Signal.Price
			// Only attach diagnose when TF matches — diagnose is TF-specific.
			if string(v.Signal.Timeframe) == t.TF && v.Diagnose != nil {
				card.Diagnose = v.Diagnose
			}
			break
		}

		// Compute R progress against the trade's own entry/stop/TPs.
		// Risk magnitude is |stop - entry|; signed direction depends on side.
		if card.MarkPrice > 0 && t.Stop != t.Entry {
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
			card.PctToStop = clampPct(-moved / risk * 100)
			if t.TP1 != 0 {
				card.PctToTP1 = clampPct(moved / risk * 100) // TP1 is at +1R, so mover/risk * 100 = % progress
			}
			if t.TP2 != 0 {
				card.PctToTP2 = clampPct(moved / risk / 2 * 100) // TP2 at +2R, so /2 to normalize
			}
		}
		cards = append(cards, card)
	}
	return cards
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
	v.ClosedClose = candles[len(candles)-1].Close // closed-bar reference (engine sees this)
	if markPrice > 0 {
		v.Signal.Price = markPrice // override for display only; engine math unchanged
	}
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
	// Default analyzed_at to current local time, formatted for <input type="datetime-local">.
	now := time.Now().Local().Format("2006-01-02T15:04")
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
		"AnalyzedAt": now,
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
	case "tp1", "tp2", "stop", "manual", "timeout":
	default:
		rerender("outcome must be tp1, tp2, stop, manual, or timeout")
		return
	}
	exit, err := parseFloatPositive(exitStr, "exit price")
	if err != nil {
		rerender(err.Error())
		return
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
	trades[idx].RRealized = journal.RealizedR(trades[idx], exit)

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
		case "tp1", "tp2", "stop", "manual", "timeout":
		default:
			rerender("outcome required when closed_at is set")
			return
		}
		exitPrice, err = parseFloatPositive(exitStr, "exit price")
		if err != nil {
			rerender(err.Error())
			return
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
	trades[idx].ClosedAt = closedAt
	trades[idx].Outcome = outcome
	trades[idx].ExitPrice = exitPrice
	if !closedAt.IsZero() {
		trades[idx].RRealized = journal.RealizedR(trades[idx], exitPrice)
	} else {
		trades[idx].RRealized = 0
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

	// Compute summary stats over closed trades.
	var totalR, bestR, worstR float64
	wins, closedCount := 0, 0
	for _, t := range trades {
		if t.IsOpen() {
			continue
		}
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

	c.HTML(http.StatusOK, "journal_list.html", gin.H{
		"Trades":      trades,
		"OpenCount":   len(trades) - closedCount,
		"ClosedCount": closedCount,
		"WR":          wr,
		"AvgR":        avgR,
		"TotalR":      totalR,
		"BestR":       bestR,
		"WorstR":      worstR,
	})
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
			if t.IsOpen() {
				return "status-open"
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
			if t.IsOpen() {
				return "open"
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
			q := url.Values{}
			q.Set("symbol", short)
			q.Set("side", strings.ToLower(r.Side.String()))
			q.Set("entry", fmt.Sprintf("%.4f", r.Entry))
			// Prefer engine's own plan if it has a tradeable one and the
			// direction matches; else fall back to the suggested ATR-based
			// levels (still useful structure for a discretionary entry).
			if r.EnginePlan.Entry != 0 && r.EngineSide == r.Side {
				q.Set("stop", fmt.Sprintf("%.4f", r.EnginePlan.StopLoss))
				if len(r.EnginePlan.TakeProfit) >= 1 {
					q.Set("tp1", fmt.Sprintf("%.4f", r.EnginePlan.TakeProfit[0]))
				}
				if len(r.EnginePlan.TakeProfit) >= 2 {
					q.Set("tp2", fmt.Sprintf("%.4f", r.EnginePlan.TakeProfit[1]))
				}
				q.Set("anchor", r.EnginePlan.Anchor)
			} else {
				q.Set("stop", fmt.Sprintf("%.4f", r.SuggStop))
				q.Set("tp1", fmt.Sprintf("%.4f", r.SuggTP1))
				q.Set("tp2", fmt.Sprintf("%.4f", r.SuggTP2))
			}
			q.Set("tf", string(r.Timeframe))
			q.Set("score", fmt.Sprintf("v%.1f", r.Total))
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
		"Symbol":  strings.ToUpper(c.Query("symbol")),
		"Side":    strings.ToLower(c.Query("side")),
		"Entry":   c.Query("entry"),
		"TF":      defaultStr(c.Query("tf"), "1h"),
		"FeeBps":  defaultStr(c.Query("fee_bps"), "6"),
		"Symbols": []string{"BTC", "ETH", "XAU", "XAG"},
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
			"Symbol":  symStr,
			"Side":    sideStr,
			"Entry":   c.PostForm("entry"),
			"TF":      tfStr,
			"FeeBps":  defaultStr(c.PostForm("fee_bps"), "6"),
			"Symbols": []string{"BTC", "ETH", "XAU", "XAG"},
			"Error":   errMsg,
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
