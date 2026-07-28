package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
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
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
	"myFirstGo/trading-bot/ai"
	"myFirstGo/trading-bot/onchain"
	"myFirstGo/trading-bot/validator"
)

type server struct {
	client *bingx.Client
	ai     ai.Provider
	// aiProviderName is a friendly label ("gemini", "anthropic", or
	// "gemini(fallback-from-xxx)") for logs + cache-key namespacing.
	aiProviderName string

	// aiModelPath is the on-disk file the /api/ai/model UI writes so
	// the admin's model choice survives restarts. Read on boot (see
	// main.go LoadPersistedModel), written on each POST.
	aiModelPath string

	// onchain orchestrator — powers the /onchain tab's per-coin holder
	// + dump-signal lookup. Constructed once in main.go so the
	// CoinGecko cache + CEX registry persist across requests.
	onchain *onchain.Service

	// aiCache memoizes /ai/analyze responses by trade ID so repeat
	// clicks (page refresh, accordion re-open) don't re-bill against
	// the user's token budget. Invalidated when the trade row is
	// edited or closed.
	aiCacheMu sync.Mutex
	aiCache   map[int]aiCacheEntry

	// aiSymbolCache memoizes /ai/analyze/symbol responses keyed by
	// "SHORT|TF|signalhash" — same signal hash skips re-billing on
	// dashboard refreshes that didn't change the underlying setup.
	aiSymbolCacheMu sync.Mutex
	aiSymbolCache   map[string]aiCacheEntry
}

type aiCacheEntry struct {
	Text         string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	GeneratedAt  time.Time
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

	// Summary is the pure-function rule-based one-line description of
	// this card's current state, built via signal.BuildSummary from
	// Signal + Diagnose. Always populated (degrades gracefully when
	// Diagnose is nil). The per-symbol AI Analyze button calls a
	// separate endpoint for richer LLM-backed analysis.
	Summary string
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
	// Macro blackout state — show banner when active, or when upcoming
	// within 6h so the user gets a heads-up before placing a trade that
	// would straddle the event.
	nowUTC := time.Now().UTC()
	activeBO := macro.ActiveAt(nowUTC)
	var upcomingBO *macro.Event
	var upcomingIn time.Duration
	if activeBO == nil {
		upcomingBO = macro.NextUpcoming(nowUTC, 6*time.Hour)
		if upcomingBO != nil {
			start, _ := upcomingBO.Window()
			upcomingIn = start.Sub(nowUTC)
		}
	}

	const minTradeable = 3
	// Tradeable signals summary for the top-of-page banner. Filters the
	// 4 symbol views on the CURRENT TF down to just the ones that would
	// fire ntfy (side != Flat AND score >= MIN_SCORE). Saves the user
	// from scanning all four cards to find which one has an actionable
	// setup — parallels the "ntfy pushed but I'm already on the page"
	// use case.
	type tradeableSummary struct {
		Short    string
		TF       string
		Side     string
		Score    int
		MRScore  int
		MOMScore int
		Anchor   string
		Ratio    float64
		Verdict  string
	}
	var tradeable []tradeableSummary
	for _, v := range views {
		if v.Signal.Side == signal.Flat || v.Signal.Score < minTradeable {
			continue
		}
		t := tradeableSummary{
			Short:    v.Short,
			TF:       tf,
			Side:     v.Signal.Side.String(),
			Score:    v.Signal.Score,
			MRScore:  v.Signal.MRScore,
			MOMScore: v.Signal.MomentumScore,
			Anchor:   v.Signal.Plan.Anchor,
		}
		if v.Diagnose != nil {
			t.Ratio = v.Diagnose.Total
			t.Verdict = verdictShortHelper(v.Diagnose.Verdict)
		}
		tradeable = append(tradeable, t)
	}

	c.HTML(http.StatusOK, "dashboard.html", gin.H{
		"TF":              tf,
		"Symbols":         views,
		"Tradeable":       tradeable,
		"OpenTrades":      openTrades,
		"Now":             time.Now().Format("2006-01-02 15:04:05"),
		"TFOptions":       []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"},
		"MinTradeable":    minTradeable,
		"MacroActive":     activeBO,
		"MacroUpcoming":   upcomingBO,
		"MacroUpcomingIn": upcomingIn,
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

	// Diff-since-entry (populated only when the trade has a non-empty
	// SignalCtx snapshot and we have a current Diagnose). Shows the
	// trader how the validator picture has shifted since they committed,
	// so they can distinguish "price drifted" from "thesis broke".
	HasDiff     bool
	ScoreDelta  float64 // current /10 minus entry /10
	DiffCause   string  // one-line headline of the most actionable change
	DiffClass   string  // "up" / "down" / "" — drives chip colour
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
		// Floor = opened_at strictly. The candle gate is on c.OpenTime
		// (NOT CloseTime) — a bar's Low/High covers the WHOLE bar, so
		// if the bar started before opened_at, its extremes may reflect
		// price action BEFORE the limit order existed. Using CloseTime
		// here was a bug that mis-filled trades opened mid-bar (e.g. a
		// 1h trade opened at 15:55 would inherit the 15:00-16:00 bar's
		// 55min of pre-open Low). The live-mark fallback below handles
		// the "trade just opened, no full post-open bar yet" gap.
		floor := t.OpenedAt
		hit := false
		for _, c := range v.Candles {
			if !c.OpenTime.After(floor) {
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
	// Pass 2: auto-place reduce-only orders for active trades. Each of
	// TP1 / Stop / TP2 has its own opt-in flag (TP1Auto / StopAuto /
	// TP2Auto) and its own orderId column; sweep tries each independently
	// and persists successes. The reduce-only flag is hard-coded in all
	// placement helpers, so a misfire's worst outcome is BingX rejecting
	// the order — never an unintended position open.
	for i := range trades {
		t := &trades[i]
		if !t.IsActive() {
			continue
		}
		if t.TP1Auto && t.TP1OrderID == "" {
			status, msg := s.placeTP1OnBingX(ctx, t, "")
			switch status {
			case "ok":
				log.Printf("auto-tp1: trade #%d %s %s → %s", t.ID, t.Symbol, t.Side, msg)
				tradesChanged = true
			case "error":
				log.Printf("auto-tp1: trade #%d %s %s ERROR: %s", t.ID, t.Symbol, t.Side, msg)
			}
		}
		if t.StopAuto && t.StopOrderID == "" {
			status, msg := s.placeStopOnBingX(ctx, t)
			switch status {
			case "ok":
				log.Printf("auto-stop: trade #%d %s %s → %s", t.ID, t.Symbol, t.Side, msg)
				tradesChanged = true
			case "error":
				log.Printf("auto-stop: trade #%d %s %s ERROR: %s", t.ID, t.Symbol, t.Side, msg)
			}
		}
		if t.TP2Auto && t.TP2OrderID == "" {
			status, msg := s.placeTP2OnBingX(ctx, t, 50)
			switch status {
			case "ok":
				log.Printf("auto-tp2: trade #%d %s %s → %s", t.ID, t.Symbol, t.Side, msg)
				tradesChanged = true
			case "error":
				log.Printf("auto-tp2: trade #%d %s %s ERROR: %s", t.ID, t.Symbol, t.Side, msg)
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
					// Diff-since-entry: parse the journal's snapshot from
					// +record time, compare to the live re-validation,
					// surface the most actionable structural change.
					if t.SignalCtx != "" {
						entryCtx := parseSignalCtx(t.SignalCtx)
						delta, cause, has := diagnoseDelta(entryCtx, card.Diagnose)
						if has {
							card.HasDiff = true
							card.ScoreDelta = delta
							card.DiffCause = cause
							switch {
							case delta < -0.5:
								card.DiffClass = "down"
							case delta > 0.5:
								card.DiffClass = "up"
							}
						}
					}
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
	v.Summary = signal.BuildSummary(v.Signal, diagnoseView(v.Diagnose))
	return v
}

// diagnoseView projects a validator.Result into signal.DiagnoseView so the
// signal package can compose a card summary without importing validator
// (which would be a cycle: validator already imports signal).
func diagnoseView(d *validator.Result) signal.DiagnoseView {
	if d == nil {
		return signal.DiagnoseView{}
	}
	return signal.DiagnoseView{
		Has:          true,
		Side:         d.Side,
		Total:        d.Total,
		TotalMR:      d.TotalMR,
		TotalMOM:     d.TotalMOM,
		VerdictShort: verdictShortHelper(d.Verdict),
	}
}

// verdictShortHelper trims a Verdict string like "STRONG TAKE — full size"
// down to just "STRONG TAKE". Mirrors the verdictShort template helper.
func verdictShortHelper(v string) string {
	if i := strings.Index(v, " — "); i > 0 {
		return v[:i]
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
		"Symbol":        c.Query("symbol"),
		"Side":          c.Query("side"),
		"Entry":         c.Query("entry"),
		"Stop":          c.Query("stop"),
		"TP1":           c.Query("tp1"),
		"TP2":           c.Query("tp2"),
		"Anchor":        c.Query("anchor"),
		"TF":            c.Query("tf"),
		"Score":         c.Query("score"),
		"Notes":         "",
		"AnalyzedAt":    analyzedAt,
		"SignalCtx":     c.Query("ctx"), // verbatim from recordHref / validateRecordHref
		"Symbols":       []string{"BTC", "ETH", "XAU", "XAG"},
		"Anchors":       recommendedAnchors,
		"Error":         "",
		"MarginUSDT":     "",
		"TP1PartialPct":  "50",
		"PlaceTP1":       true,
		"MarginOverride": false,
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
			"Leverage":       leverageStr,
			"MarginUSDT":     strings.TrimSpace(c.PostForm("margin_usdt")),
			"TP1PartialPct":  strings.TrimSpace(c.PostForm("tp1_partial_pct")),
			"PlaceTP1":       c.PostForm("place_tp1") == "on",
			"MarginOverride": c.PostForm("margin_override") == "on",
			"Symbols":        []string{"BTC", "ETH", "XAU", "XAG"},
			"Anchors":        recommendedAnchors,
			"Error":          errMsg,
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

	autoTP1 := c.PostForm("place_tp1") == "on"
	autoStop := c.PostForm("place_stop") == "on"
	autoTP2 := c.PostForm("place_tp2") == "on"
	openPos := c.PostForm("open_position") == "on"
	var marginUSDT float64
	if mStr := strings.TrimSpace(c.PostForm("margin_usdt")); mStr != "" {
		marginUSDT, _ = strconv.ParseFloat(mStr, 64)
	}
	// Per-trade margin cap (BINGX_MAX_MARGIN_USDT, default 500) guards
	// against accidental misclick that would route a too-large order.
	if openPos {
		marginCap := 500.0
		if v := os.Getenv("BINGX_MAX_MARGIN_USDT"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				marginCap = f
			}
		}
		marginOverride := c.PostForm("margin_override") == "on"
		if marginUSDT <= 0 {
			rerender("margin_usdt required when 'Open position on BingX' is checked")
			return
		}
		if marginUSDT > marginCap && !marginOverride {
			rerender(fmt.Sprintf("margin_usdt %.2f exceeds BINGX_MAX_MARGIN_USDT=%.2f — tick 'Allow margin > cap' to override", marginUSDT, marginCap))
			return
		}
		if marginUSDT > marginCap && marginOverride {
			log.Printf("auto-open: margin override accepted (%.2f > cap %.2f) for %s %s", marginUSDT, marginCap, symbol, side)
		}
		if leverage < 1 {
			rerender("leverage required when 'Open position on BingX' is checked")
			return
		}
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
		MarginUSDT: marginUSDT,
		TP1Auto:    autoTP1,
		StopAuto:   autoStop,
		TP2Auto:    autoTP2,
	}
	trades = append(trades, t)
	idx := len(trades) - 1
	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}

	// Synchronous order placements (entry + immediate TP1 attempt). The
	// journal row above ALREADY has the auto flags persisted, so any
	// failure here just leaves the sweep to retry later — never blocks
	// the user from recording the plan.
	statuses := []string{}
	if openPos {
		st, msg := s.placeEntryOnBingX(c.Request.Context(), &trades[idx])
		statuses = append(statuses, "entry:"+st+":"+msg)
	}
	if autoTP1 {
		st, msg := s.placeTP1OnBingX(c.Request.Context(), &trades[idx], strings.TrimSpace(c.PostForm("tp1_partial_pct")))
		statuses = append(statuses, "tp1:"+st+":"+msg)
	}
	// Persist any orderIds that got written by the helpers.
	if trades[idx].EntryOrderID != "" || trades[idx].TP1OrderID != "" {
		if err := journal.WriteAll("", trades); err != nil {
			log.Printf("auto-open: post-place journal write failed: %v", err)
		}
	}

	redirectURL := "/journal"
	if len(statuses) > 0 {
		// Pack multi-status into the existing flash params; the template
		// renders the joined messages line-by-line.
		msg := strings.Join(statuses, "\n")
		// Worst status wins for color coding.
		status := "ok"
		for _, s := range statuses {
			if strings.HasPrefix(s, "entry:error") || strings.HasPrefix(s, "tp1:error") {
				status = "error"
				break
			}
			if strings.HasPrefix(s, "entry:skip") || strings.HasPrefix(s, "tp1:skip") {
				status = "skip"
			}
		}
		redirectURL = "/journal?tp1_status=" + url.QueryEscape(status) + "&tp1_msg=" + url.QueryEscape(msg)
	}
	c.Redirect(http.StatusSeeOther, redirectURL)
}

// placeTP1OnBingX looks up the open position for t.Symbol on BingX, then
// submits a reduce-only LIMIT order at t.TP1 for the chosen partial %.
// On success, writes the returned orderId into t.TP1OrderID — the caller
// is responsible for persisting the mutated trade back to the journal.
// Returns (status, msg) where status is one of: "ok" / "skip" / "error".
// Never panics; all failures are surfaced via msg.
func (s *server) placeTP1OnBingX(ctx context.Context, t *journal.Trade, partialStr string) (string, string) {
	if t.TP1OrderID != "" {
		return "skip", fmt.Sprintf("TP1 already placed (orderId=%s) — cancel on BingX first to re-place", t.TP1OrderID)
	}
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		return "skip", "BingX API key/secret not configured in .env"
	}
	pct := 50.0
	if partialStr != "" {
		v, err := strconv.ParseFloat(partialStr, 64)
		if err != nil || v <= 0 || v > 100 {
			return "error", "tp1_partial_pct must be 1-100"
		}
		pct = v
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return "error", err.Error()
	}
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		return "error", "read positions: " + err.Error()
	}
	if pos == nil {
		return "skip", fmt.Sprintf("no open %s position on BingX for %s — TP1 will be retried on next fill-detection sweep", t.Side, t.Symbol)
	}
	// Sanity: TP1 must be on the profitable side of entry for the position
	// we're closing. The journal-form validation already enforced this for
	// the *trade plan*, but the LIVE position might be different (e.g. user
	// opened a different size/side). Re-check defensively.
	if t.Side == "long" && t.TP1 <= pos.EntryPrice {
		return "error", fmt.Sprintf("TP1 %.4f is not above live entry %.4f (long); refusing", t.TP1, pos.EntryPrice)
	}
	if t.Side == "short" && t.TP1 >= pos.EntryPrice {
		return "error", fmt.Sprintf("TP1 %.4f is not below live entry %.4f (short); refusing", t.TP1, pos.EntryPrice)
	}

	qty := pos.Quantity * pct / 100
	if qty <= 0 {
		return "error", "computed qty <= 0"
	}
	hedgeMode := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"
	res, err := s.client.PlaceReduceOnlyLimit(ctx, sym, t.Side, qty, t.TP1, hedgeMode)
	if err != nil {
		return "error", "place TP1: " + err.Error()
	}
	t.TP1OrderID = res.OrderID
	return "ok", fmt.Sprintf("TP1 placed on BingX — orderId=%s qty=%g @ %.4f (%.0f%% of %g)", res.OrderID, qty, t.TP1, pct, pos.Quantity)
}

// qtyPrecision is the per-symbol qty step we floor placement size to.
// Fetched once from BingX /openApi/swap/v2/quote/contracts:
//
//	BTC-USDT          quantityPrecision=4
//	ETH-USDT          quantityPrecision=2
//	NCCOGOLD2USD-USDT quantityPrecision=4
//	NCCOXAG2USD-USDT  quantityPrecision=4
//
// The JS PnL preview mirrors this map; keep them in sync if BingX changes.
var qtyPrecision = map[string]int{
	"BTC": 4, "ETH": 2, "XAU": 4, "XAG": 4,
}

func floorTo(v float64, decimals int) float64 {
	f := math.Pow(10, float64(decimals))
	return math.Floor(v*f) / f
}

// hedgeModeEnabled reflects BINGX_HEDGE_MODE=true on the VPS. One-way
// mode (default) sends no positionSide; hedge mode sends LONG/SHORT.
// Read once per request; cheap enough not to cache.
func hedgeModeEnabled() bool {
	return os.Getenv("BINGX_HEDGE_MODE") == "true"
}

// placeEntryOnBingX sets the symbol's leverage then submits a LIMIT
// order at t.Entry for the qty derived from t.MarginUSDT × t.Leverage / t.Entry
// (floored to lot precision). On success, writes the orderId into
// t.EntryOrderID. Returns (status, msg) for the /journal flash banner.
func (s *server) placeEntryOnBingX(ctx context.Context, t *journal.Trade) (string, string) {
	if t.EntryOrderID != "" {
		return "skip", fmt.Sprintf("entry already placed (orderId=%s) — cancel on BingX first to re-place", t.EntryOrderID)
	}
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		return "skip", "BingX API key/secret not configured in .env"
	}
	if t.MarginUSDT <= 0 {
		return "error", "margin_usdt must be > 0 to auto-open"
	}
	if t.Leverage < 1 {
		return "error", "leverage must be >= 1 to auto-open"
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return "error", err.Error()
	}
	rawQty := t.MarginUSDT * float64(t.Leverage) / t.Entry
	prec, ok := qtyPrecision[t.Symbol]
	if !ok {
		prec = 4
	}
	qty := floorTo(rawQty, prec)
	if qty <= 0 {
		return "error", fmt.Sprintf("computed qty=%g <= 0 (margin %.2f × lev %d / entry %.4f, floored to %d decimals)", qty, t.MarginUSDT, t.Leverage, t.Entry, prec)
	}

	hedge := hedgeModeEnabled()
	levSide := "BOTH"
	if hedge {
		if t.Side == "long" {
			levSide = "LONG"
		} else {
			levSide = "SHORT"
		}
	}
	if err := s.client.SetLeverage(ctx, sym, levSide, t.Leverage); err != nil {
		return "error", "set leverage: " + err.Error()
	}
	// Bundle SL and TP2 with the entry where applicable. BingX activates
	// them the moment the entry fills, so neither the stop nor the TP2
	// runner has a sweep-based delay. Sentinels in StopOrderID / TP2OrderID
	// prevent the sweep from placing duplicates after fill. TP1 (partial)
	// stays on the sweep path — bundled SL/TP close 100% of the position,
	// which collides with partial scaling, so TP1 needs the explicit
	// reduce-only LIMIT path.
	var bundledStop, bundledTP float64
	if t.StopAuto && t.Stop > 0 {
		bundledStop = t.Stop
	}
	if t.TP2Auto && t.TP2 > 0 {
		bundledTP = t.TP2
	}
	res, err := s.client.PlaceLimit(ctx, sym, t.Side, qty, t.Entry, bundledStop, bundledTP, hedge)
	if err != nil {
		return "error", "place entry: " + err.Error()
	}
	t.EntryOrderID = res.OrderID
	bundledMsg := ""
	if bundledStop > 0 {
		t.StopOrderID = "ATTACHED:" + res.OrderID
		bundledMsg += fmt.Sprintf(" + SL bundled @ %.4f", t.Stop)
	}
	if bundledTP > 0 {
		t.TP2OrderID = "ATTACHED:" + res.OrderID
		bundledMsg += fmt.Sprintf(" + TP2 bundled @ %.4f", t.TP2)
	}
	return "ok", fmt.Sprintf("entry placed — orderId=%s qty=%g @ %.4f (margin %.2f × %dx)%s", res.OrderID, qty, t.Entry, t.MarginUSDT, t.Leverage, bundledMsg)
}

// placeStopOnBingX submits a reduce-only STOP_MARKET sized to the full
// live position (after TP1's partial is also placed, the stop still
// covers everything reduce-only can touch — BingX won't over-close).
func (s *server) placeStopOnBingX(ctx context.Context, t *journal.Trade) (string, string) {
	if t.StopOrderID != "" {
		return "skip", fmt.Sprintf("stop already placed (orderId=%s) — cancel on BingX first to re-place", t.StopOrderID)
	}
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		return "skip", "BingX API key/secret not configured in .env"
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return "error", err.Error()
	}
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		return "error", "read positions: " + err.Error()
	}
	if pos == nil {
		return "skip", fmt.Sprintf("no open %s position on BingX for %s — stop will be retried on next sweep", t.Side, t.Symbol)
	}
	if t.Side == "long" && t.Stop >= pos.EntryPrice {
		return "error", fmt.Sprintf("stop %.4f is not below live entry %.4f (long); refusing", t.Stop, pos.EntryPrice)
	}
	if t.Side == "short" && t.Stop <= pos.EntryPrice {
		return "error", fmt.Sprintf("stop %.4f is not above live entry %.4f (short); refusing", t.Stop, pos.EntryPrice)
	}
	hedge := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"
	res, err := s.client.PlaceStopMarket(ctx, sym, t.Side, pos.Quantity, t.Stop, hedge)
	if err != nil {
		return "error", "place stop: " + err.Error()
	}
	t.StopOrderID = res.OrderID
	return "ok", fmt.Sprintf("stop placed — orderId=%s qty=%g @ %.4f", res.OrderID, pos.Quantity, t.Stop)
}

// placeTP2OnBingX submits a reduce-only LIMIT at t.TP2 for the
// remaining (post-TP1-partial) position size. Sized as the position's
// full qty minus what we'd close at TP1 (so TP1 + TP2 together fully
// close the position).
func (s *server) placeTP2OnBingX(ctx context.Context, t *journal.Trade, tp1PartialPct float64) (string, string) {
	if t.TP2OrderID != "" {
		return "skip", fmt.Sprintf("TP2 already placed (orderId=%s) — cancel on BingX first to re-place", t.TP2OrderID)
	}
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		return "skip", "BingX API key/secret not configured in .env"
	}
	if tp1PartialPct <= 0 || tp1PartialPct > 100 {
		tp1PartialPct = 50
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return "error", err.Error()
	}
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		return "error", "read positions: " + err.Error()
	}
	if pos == nil {
		return "skip", fmt.Sprintf("no open %s position on BingX for %s — TP2 will be retried on next sweep", t.Side, t.Symbol)
	}
	if t.Side == "long" && t.TP2 <= pos.EntryPrice {
		return "error", fmt.Sprintf("TP2 %.4f is not above live entry %.4f (long); refusing", t.TP2, pos.EntryPrice)
	}
	if t.Side == "short" && t.TP2 >= pos.EntryPrice {
		return "error", fmt.Sprintf("TP2 %.4f is not below live entry %.4f (short); refusing", t.TP2, pos.EntryPrice)
	}
	prec, ok := qtyPrecision[t.Symbol]
	if !ok {
		prec = 4
	}
	tp1Qty := floorTo(pos.Quantity*tp1PartialPct/100, prec)
	tp2Qty := floorTo(pos.Quantity-tp1Qty, prec)
	if tp2Qty <= 0 {
		return "skip", fmt.Sprintf("TP1 partial=%.0f%% leaves no remainder for TP2", tp1PartialPct)
	}
	hedge := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"
	res, err := s.client.PlaceReduceOnlyLimit(ctx, sym, t.Side, tp2Qty, t.TP2, hedge)
	if err != nil {
		return "error", "place TP2: " + err.Error()
	}
	t.TP2OrderID = res.OrderID
	return "ok", fmt.Sprintf("TP2 placed — orderId=%s qty=%g @ %.4f (remaining after %.0f%% TP1)", res.OrderID, tp2Qty, t.TP2, tp1PartialPct)
}

// unwindOnBingX is the "abandon this trade" path: cancels every known
// pending order tied to t (entry / stop / TP1 / TP2), then market-closes
// any remaining live position. Returns a list of human-readable status
// lines suitable for the /journal flash banner. Tolerant: cancel-already-
// cancelled or no-live-position are treated as best-effort soft-success.
//
// Mutates t to clear orderIds that were cancelled (so the journal row,
// if the caller chooses to keep it, reflects reality).
// cancelTrackedOrdersOnBingX cancels every non-empty orderId stored on
// t (entry / stop / tp1 / tp2), tolerating "already gone" errors from
// BingX. Mutates t to clear cancelled orderIds. Returns one status line
// per leg. DRY-RUN sentinels and ATTACHED-to-entry sentinels (bundled
// SL/TP that BingX manages itself) are skipped without calling the API.
//
// Used by both the unwind path (cancel + market-close) and the close
// path (cancel after user records the exit, so the journal row's
// remaining orderIds don't orphan on BingX).
func (s *server) cancelTrackedOrdersOnBingX(ctx context.Context, t *journal.Trade) []string {
	out := []string{}
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		return []string{"BingX API not configured — nothing to cancel"}
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return []string{"resolve symbol: " + err.Error()}
	}
	type leg struct{ name, id string }
	for _, lg := range []leg{
		{"entry", t.EntryOrderID},
		{"stop", t.StopOrderID},
		{"tp1", t.TP1OrderID},
		{"tp2", t.TP2OrderID},
	} {
		if lg.id == "" {
			continue
		}
		if strings.HasPrefix(lg.id, "DRY-RUN") {
			out = append(out, fmt.Sprintf("skip %s (DRY-RUN orderId)", lg.name))
			continue
		}
		if strings.HasPrefix(lg.id, "ATTACHED:") {
			out = append(out, fmt.Sprintf("skip %s (bundled with entry; BingX auto-cancels)", lg.name))
			continue
		}
		if err := s.client.CancelOrder(ctx, sym, lg.id); err != nil {
			out = append(out, fmt.Sprintf("cancel %s (%s): %s", lg.name, lg.id, err.Error()))
			continue
		}
		out = append(out, fmt.Sprintf("cancelled %s order %s", lg.name, lg.id))
		switch lg.name {
		case "entry":
			t.EntryOrderID = ""
		case "stop":
			t.StopOrderID = ""
		case "tp1":
			t.TP1OrderID = ""
		case "tp2":
			t.TP2OrderID = ""
		}
	}
	return out
}

func (s *server) unwindOnBingX(ctx context.Context, t *journal.Trade) []string {
	out := s.cancelTrackedOrdersOnBingX(ctx, t)
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return out
	}
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		out = append(out, "read position: "+err.Error())
		return out
	}
	if pos == nil {
		out = append(out, "no live position to close")
		return out
	}
	hedge := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"
	res, err := s.client.MarketCloseReduceOnly(ctx, sym, t.Side, pos.Quantity, hedge)
	if err != nil {
		out = append(out, "market close: "+err.Error())
		return out
	}
	out = append(out, fmt.Sprintf("market closed %g %s @ market (orderId=%s)", pos.Quantity, t.Symbol, res.OrderID))
	return out
}

// handleJournalUnwind cancels all BingX orders + market-closes any live
// position tied to this trade, then deletes the journal row. Distinct
// from handleJournalDelete (which only removes the row).
func (s *server) handleJournalUnwind(c *gin.Context) {
	_, idx, trades, err := s.loadTradeByID(c)
	if err != nil {
		c.String(http.StatusNotFound, "%s", err.Error())
		return
	}
	t := &trades[idx]
	msgs := s.unwindOnBingX(c.Request.Context(), t)
	id := t.ID

	// Drop the row regardless of unwind outcome — the user pressed
	// "Unwind + Delete" and we logged whatever happened on BingX side.
	out := make([]journal.Trade, 0, len(trades)-1)
	for i, x := range trades {
		if i == idx {
			continue
		}
		out = append(out, x)
	}
	if err := journal.WriteAll("", out); err != nil {
		c.String(http.StatusInternalServerError, "write journal: %s", err.Error())
		return
	}

	flash := fmt.Sprintf("UNWIND #%d:\n%s", id, strings.Join(msgs, "\n"))
	c.Redirect(http.StatusSeeOther, "/journal?tp1_status=ok&tp1_msg="+url.QueryEscape(flash))
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

	// Cancel any still-pending BingX orderIds tied to this trade. Closing
	// a journal row implies "this trade is done"; anything BingX still
	// thinks is open (sweep-placed TP1 reduce-only, residual stop, etc.)
	// is by definition an orphan. Default ON; the user can untick the
	// checkbox on the close form to skip (rare).
	cancelOrders := c.PostForm("cancel_orders") == "on"
	cancelMsgs := []string{}
	if cancelOrders {
		cancelMsgs = s.cancelTrackedOrdersOnBingX(c.Request.Context(), &trades[idx])
	}

	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}

	redirect := "/journal"
	if len(cancelMsgs) > 0 {
		flash := fmt.Sprintf("CLOSE #%d cancel results:\n%s", trades[idx].ID, strings.Join(cancelMsgs, "\n"))
		redirect = "/journal?tp1_status=ok&tp1_msg=" + url.QueryEscape(flash)
	}
	c.Redirect(http.StatusSeeOther, redirect)
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

	// Persist the auto-placement intent flags from the edit form. Editing
	// always reflects the current checkbox state — unchecked = clear future
	// auto-retry for that order type.
	autoTP1 := c.PostForm("place_tp1") == "on"
	autoStop := c.PostForm("place_stop") == "on"
	autoTP2 := c.PostForm("place_tp2") == "on"
	trades[idx].TP1Auto = autoTP1
	trades[idx].StopAuto = autoStop
	trades[idx].TP2Auto = autoTP2

	if err := journal.WriteAll("", trades); err != nil {
		rerender("write journal: " + err.Error())
		return
	}

	// Same BingX TP1 placement as /journal/open. From edit, this is also the
	// path used to (a) retroactively place TP1 on an existing journal row,
	// or (b) re-fire the BingX API while iterating without spamming new
	// journal entries.
	redirectURL := "/journal"
	if autoTP1 {
		status, msg := s.placeTP1OnBingX(c.Request.Context(), &trades[idx], strings.TrimSpace(c.PostForm("tp1_partial_pct")))
		if trades[idx].TP1OrderID != "" {
			if err := journal.WriteAll("", trades); err != nil {
				log.Printf("auto-tp1: post-place journal write failed (orderId=%s): %v", trades[idx].TP1OrderID, err)
			}
		}
		redirectURL = "/journal?tp1_status=" + url.QueryEscape(status) + "&tp1_msg=" + url.QueryEscape(msg)
	}
	c.Redirect(http.StatusSeeOther, redirectURL)
}

// handleAIAnalyzeTrade is the Phase 1 AI advisor endpoint. POST
// /ai/analyze/:id — packages the trade's plan + journal context + live
// BingX position + recent same-symbol history + macro events into a
// structured user message, sends to Anthropic with the Quant persona
// system prompt, and returns the response as JSON.
//
// Cached per trade ID in memory so repeat clicks (page refresh, modal
// re-open) don't re-bill. Cache invalidates implicitly when the user
// edits or closes the trade (next call computes fresh context anyway).
func (s *server) handleAIAnalyzeTrade(c *gin.Context) {
	t, _, _, err := s.loadTradeByID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if s.ai == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai client not initialized"})
		return
	}
	apiKey := ai.APIKeyForProvider(s.ai)
	if apiKey == "" && !s.ai.IsDryRun() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI provider API key not configured on server. Set GEMINI_API_KEY / ANTHROPIC_API_KEY (matching AI_PROVIDER) in .env, or enable the matching *_DRY_RUN=true for stub responses."})
		return
	}

	// Cache hit? Return immediately.
	s.aiCacheMu.Lock()
	if cached, ok := s.aiCache[t.ID]; ok {
		s.aiCacheMu.Unlock()
		c.JSON(http.StatusOK, gin.H{
			"text":          cached.Text,
			"input_tokens":  cached.InputTokens,
			"output_tokens": cached.OutputTokens,
			"cost_usd":      cached.CostUSD,
			"generated_at":  cached.GeneratedAt.Format(time.RFC3339),
			"cached":        true,
		})
		return
	}
	s.aiCacheMu.Unlock()

	// Gather context. Each fetcher is best-effort; missing data just
	// means the LLM sees a smaller prompt, not an error.
	inputs := ai.TradeAnalysisInputs{Trade: t}

	// Live BingX position + mark price + funding rate for the trade's symbol.
	if sym, err := resolveWebSymbol(t.Symbol); err == nil && s.client != nil {
		if pos, perr := s.client.FindOpenPosition(c.Request.Context(), sym, t.Side); perr == nil && pos != nil {
			inputs.LivePosition = pos
		}
		if fr, ferr := s.client.FundingRate(c.Request.Context(), sym); ferr == nil {
			inputs.MarkPrice = fr.MarkPrice
			inputs.FundingRate = fr.Rate
		}
	}

	// Recent candles for the trade's TF (skip if TF unparseable).
	// Fetch 200 (not 50) so we can compute structure classifier + HVNs.
	if sym, err := resolveWebSymbol(t.Symbol); err == nil && s.client != nil {
		tfStr := t.TF
		if j := strings.Index(tfStr, ","); j >= 0 {
			tfStr = strings.TrimSpace(tfStr[:j])
		}
		if tfStr != "" {
			tf := market.Timeframe(tfStr)
			if candles, cerr := s.client.Klines(c.Request.Context(), sym, tf, 200); cerr == nil && len(candles) >= 60 {
				// Bar-path summary uses last 50 to stay compact.
				start := len(candles) - 50
				if start < 0 {
					start = 0
				}
				inputs.RecentBars = candles[start:]

				// Enrichment: structure state + HVN list + higher-TF context.
				inputs.StructureNote = structureNoteFor(candles)
				sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: candles})
				inputs.TopHVNs = topHVNList(sig.VP.HVN, sig.VP.POC, inputs.MarkPrice)
				inputs.HigherTFs = s.buildHigherTFSummaries(c.Request.Context(), sym, tf)
			}
		}
	}

	// Recent same-symbol closed trades for journal context (last 5).
	allTrades, _ := journal.ReadAll("")
	for i := len(allTrades) - 1; i >= 0 && len(inputs.RecentSame) < 5; i-- {
		r := allTrades[i]
		if r.ID == t.ID {
			continue
		}
		if r.Symbol != t.Symbol {
			continue
		}
		if r.ClosedAt.IsZero() {
			continue
		}
		inputs.RecentSame = append(inputs.RecentSame, r)
	}

	// Macro events within ±24h of opened_at (or now if still open).
	anchor := t.OpenedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(anchor)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		inputs.MacroNear = append(inputs.MacroNear, e)
	}

	userMsg := ai.BuildTradeAnalysisMessage(inputs)

	// Call Anthropic.
	resp, err := s.ai.Send(c.Request.Context(), ai.SendOptions{
		APIKey:      apiKey,
		System:      ai.SystemPromptQuantAdvisor,
		Messages:    []ai.Message{{Role: "user", Content: userMsg}},
		Temperature: 0.3, // analytical determinism — low temp, not creative writing
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "AI provider call failed: " + err.Error()})
		return
	}

	cost := resp.EstimatedCostUSD()
	now := time.Now().UTC()
	s.aiCacheMu.Lock()
	s.aiCache[t.ID] = aiCacheEntry{
		Text:         resp.Text,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      cost,
		GeneratedAt:  now,
	}
	s.aiCacheMu.Unlock()
	log.Printf("ai.analyze trade #%d: %d/%d tokens, est $%.4f, stop=%s",
		t.ID, resp.InputTokens, resp.OutputTokens, cost, resp.StopReason)

	c.JSON(http.StatusOK, gin.H{
		"text":          resp.Text,
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
		"cost_usd":      cost,
		"model":         resp.Model,
		"stop_reason":   resp.StopReason,
		"generated_at":  now.Format(time.RFC3339),
		"cached":        false,
	})
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
		"Trade":         t,
		"OpenedAt":      asLocalInput(t.OpenedAt),
		"AnalyzedAt":    asLocalInput(t.AnalyzedAt),
		"FilledAt":      asLocalInput(t.FilledAt),
		"ClosedAt":      asLocalInput(t.ClosedAt),
		"Entry":         fmt.Sprintf("%.4f", t.Entry),
		"Stop":          fmt.Sprintf("%.4f", t.Stop),
		"TP1":           fmt.Sprintf("%.4f", t.TP1),
		"TP2":           fmt.Sprintf("%.4f", t.TP2),
		"ExitPrice":     exitStr,
		"Symbols":       []string{"BTC", "ETH", "XAU", "XAG"},
		"Anchors":       recommendedAnchors,
		"Error":         errMsg,
		"TP1PartialPct": "50",
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
		"TP1Status":    c.Query("tp1_status"), // "" / "ok" / "skip" / "error" — flash from /journal/open
		"TP1Msg":       c.Query("tp1_msg"),
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

// parseSignalCtx parses the journal's signal_ctx column (semicolon-
// delimited key=value pairs we wrote at +record time) back into a map
// for diff comparison against the live validator result.
func parseSignalCtx(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, kv := range strings.Split(s, ";") {
		if i := strings.Index(kv, "="); i > 0 {
			out[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}
	return out
}

// diagnoseDelta computes the score-delta + a one-line "most actionable
// cause" of the change, by comparing the validator state at entry (from
// the journal's signal_ctx snapshot) to the current validator result.
// Returns (delta, cause, hasDiff). cause is empty if there's nothing
// notable to surface even when delta is non-zero.
func diagnoseDelta(entry map[string]string, current *validator.Result) (float64, string, bool) {
	if len(entry) == 0 || current == nil {
		return 0, "", false
	}
	var entryScore float64
	if v, ok := entry["v"]; ok {
		_, _ = fmt.Sscanf(v, "%f", &entryScore)
	}
	delta := current.Total - entryScore

	// Find structural causes in priority order. Most-actionable first.
	// We surface ONE cause to keep the chip readable; the rest still
	// show in the existing diagnose chips on the same row.

	// 1. Flash bar appeared since entry — strongest reversal warning.
	if current.RecentFlashBarBearish {
		if _, had := entry["knife"]; !had {
			return delta, "⚠ falling-knife flag appeared", true
		}
	}
	if current.RecentFlashBarBullish {
		if _, had := entry["squeeze"]; !had {
			return delta, "⚠ blow-off flag appeared", true
		}
	}

	// 2. POC drift direction flipped.
	entryDrift := entry["d"] // "up" | "down" | "" (flat omitted at capture)
	currentDrift := ""
	switch current.POCMig.Trend {
	case indicator.POCRising:
		currentDrift = "up"
	case indicator.POCFalling:
		currentDrift = "down"
	}
	if entryDrift != "" && currentDrift != "" && entryDrift != currentDrift {
		return delta, fmt.Sprintf("regime drift flipped (%s → %s)", entryDrift, currentDrift), true
	}
	if entryDrift != "" && currentDrift == "" {
		return delta, fmt.Sprintf("regime drift collapsed (%s → flat)", entryDrift), true
	}

	// 3. VA position moved.
	entryVA := entry["va"]
	currentVA := ""
	switch {
	case current.AtVAH:
		currentVA = "at_VAH"
	case current.AtVAL:
		currentVA = "at_VAL"
	case current.InsideVA:
		currentVA = "in"
	case current.OutsideVAUp:
		currentVA = "above"
	case current.OutsideVADn:
		currentVA = "below"
	}
	if entryVA != "" && currentVA != "" && entryVA != currentVA {
		// Edge-leaving is more meaningful than edge-arriving for held trades.
		if entryVA == "at_VAH" || entryVA == "at_VAL" {
			return delta, fmt.Sprintf("left VA edge (%s → %s)", entryVA, currentVA), true
		}
		return delta, fmt.Sprintf("VA pos %s → %s", entryVA, currentVA), true
	}

	// 4. No structural change — describe the score delta if meaningful.
	switch {
	case delta <= -1.5:
		return delta, "price drift only (no structural change)", true
	case delta >= 1.5:
		return delta, "score climbed (factors firming up)", true
	}
	// Small delta + no structural change — not worth surfacing.
	return delta, "", false
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
		"fmtSigned1": func(v float64) string {
			return fmt.Sprintf("%+.1f", v)
		},
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
		"TFOptions": []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"},
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
			"TFOptions": []string{"5m", "15m", "30m", "1h", "2h", "4h", "1d"},
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

// handleOnchainPage renders the /onchain tab — a search form + result
// pane for the top-holders / dump-signal lookup. Pure HTML shell; the
// lookup itself is triggered client-side via /api/onchain/lookup.
func (s *server) handleOnchainPage(c *gin.Context) {
	c.HTML(http.StatusOK, "onchain.html", gin.H{
		"Prefill":    strings.TrimSpace(c.Query("symbol")),
		"Chains":     []string{"", "bsc", "eth", "base"},
	})
}

// handleOnchainLookup — GET /api/onchain/lookup?symbol=X&chain=Y
// Runs the onchain.Service pipeline (CoinGecko → Moralis → scan → CEX match)
// and returns JSON with holders + dump signals.
func (s *server) handleOnchainLookup(c *gin.Context) {
	if s.onchain == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "onchain service not initialized"})
		return
	}
	symbol := strings.TrimSpace(c.Query("symbol"))
	if symbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol required (ticker like AKE or coingecko id like akedo)"})
		return
	}
	chain := onchain.Chain(strings.TrimSpace(c.Query("chain")))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()

	res, err := s.onchain.Lookup(ctx, symbol, chain)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// handleAIAnalyzeSymbol is the dashboard-level AI advisor endpoint. POST
// /ai/analyze/symbol/:short/:tf — packages the current signal + diagnose
// + recent candles + macro + same-symbol journal history into a Quant
// message, calls Anthropic (or returns the DRY_RUN stub).
//
// Cached by (short|tf|signal-hash) so repeat clicks on an unchanged
// setup don't re-bill. Cache invalidates implicitly when score / side /
// MR / MOM / verdict change between requests.
func (s *server) handleAIAnalyzeSymbol(c *gin.Context) {
	short := strings.ToUpper(strings.TrimSpace(c.Param("short")))
	tfStr := strings.TrimSpace(c.Param("tf"))
	if short == "" || tfStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "short and tf are required"})
		return
	}
	sym, err := resolveWebSymbol(short)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tf := market.Timeframe(tfStr)

	if s.ai == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai client not initialized"})
		return
	}
	apiKey := ai.APIKeyForProvider(s.ai)
	if apiKey == "" && !s.ai.IsDryRun() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI provider API key not configured on server. Set GEMINI_API_KEY / ANTHROPIC_API_KEY (matching AI_PROVIDER) in .env, or enable the matching *_DRY_RUN=true for stub responses."})
		return
	}

	// Re-scan the symbol so we get the same Signal + Diagnose the
	// dashboard's just rendered. Then build the cache key around the
	// engine-output hash so repeated clicks against an unchanged
	// setup return cached LLM output instantly.
	view := s.scanOne(c.Request.Context(), sym, tf)
	if view.Err != "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "scan failed: " + view.Err})
		return
	}
	cacheKey := fmt.Sprintf("%s|%s|%d|%d|%d|%s", short, tfStr,
		view.Signal.Score, view.Signal.MRScore, view.Signal.MomentumScore,
		signalCacheTag(view))
	s.aiSymbolCacheMu.Lock()
	if cached, ok := s.aiSymbolCache[cacheKey]; ok {
		s.aiSymbolCacheMu.Unlock()
		c.JSON(http.StatusOK, gin.H{
			"text":          cached.Text,
			"input_tokens":  cached.InputTokens,
			"output_tokens": cached.OutputTokens,
			"cost_usd":      cached.CostUSD,
			"generated_at":  cached.GeneratedAt.Format(time.RFC3339),
			"cached":        true,
		})
		return
	}
	s.aiSymbolCacheMu.Unlock()

	// Fetch funding rate for the "Live market context" section. Best-effort.
	var fundingRate float64
	if fr, ferr := s.client.FundingRate(c.Request.Context(), sym); ferr == nil {
		fundingRate = fr.Rate
	}
	in := ai.SymbolAnalysisInputs{
		Symbol:        sym,
		Short:         short,
		Timeframe:     tf,
		Signal:        view.Signal,
		Summary:       view.Summary,
		MarkPrice:     view.MarkPrice,
		FundingRate:   fundingRate,
		RecentBars:    view.Candles,
		StructureNote: structureNoteFor(view.Candles),
		HigherTFs:     s.buildHigherTFSummaries(c.Request.Context(), sym, tf),
	}
	if view.Diagnose != nil {
		d := view.Diagnose
		sd := &ai.SymbolDiagnose{
			Side:         d.Side,
			Entry:        d.Entry,
			Total:        d.Total,
			TotalMR:      d.TotalMR,
			TotalMOM:     d.TotalMOM,
			Verdict:      d.Verdict,
			AtVAH:        d.AtVAH,
			AtVAL:        d.AtVAL,
			InsideVA:     d.InsideVA,
			OutsideVAUp:  d.OutsideVAUp,
			OutsideVADn:  d.OutsideVADn,
			POCTrend:    d.POCMig.Trend,
			POCDriftPct: d.POCMig.DriftPct,
			POCStacked:  d.POCMig.Stacked,
			FallingKnife: d.RecentFlashBarBearish,
			BlowOff:     d.RecentFlashBarBullish,
		}
		for _, f := range d.Factors {
			sd.Factors = append(sd.Factors, ai.SymbolDiagnoseFactor{
				Name: f.Name, Points: f.Points, Detail: f.Detail, Axis: f.Axis,
			})
		}
		in.Diagnose = sd
	}

	// Same-symbol journal context (last 5 closed).
	allTrades, _ := journal.ReadAll("")
	for i := len(allTrades) - 1; i >= 0 && len(in.RecentSame) < 5; i-- {
		r := allTrades[i]
		if r.Symbol != short {
			continue
		}
		if r.ClosedAt.IsZero() {
			continue
		}
		in.RecentSame = append(in.RecentSame, r)
	}

	// Macro events within ±24h of NOW.
	now := time.Now()
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(now)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		in.MacroNear = append(in.MacroNear, e)
	}

	userMsg := ai.BuildSymbolAnalysisMessage(in)
	resp, err := s.ai.Send(c.Request.Context(), ai.SendOptions{
		APIKey:      apiKey,
		System:      ai.SystemPromptQuantAdvisor,
		Messages:    []ai.Message{{Role: "user", Content: userMsg}},
		Temperature: 0.3,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "AI provider call failed: " + err.Error()})
		return
	}

	cost := resp.EstimatedCostUSD()
	nowT := time.Now().UTC()
	s.aiSymbolCacheMu.Lock()
	s.aiSymbolCache[cacheKey] = aiCacheEntry{
		Text:         resp.Text,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      cost,
		GeneratedAt:  nowT,
	}
	s.aiSymbolCacheMu.Unlock()
	log.Printf("ai.analyze symbol %s/%s: %d/%d tokens, est $%.4f, stop=%s, cache_key=%s",
		short, tfStr, resp.InputTokens, resp.OutputTokens, cost, resp.StopReason, cacheKey)

	c.JSON(http.StatusOK, gin.H{
		"text":          resp.Text,
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
		"cost_usd":      cost,
		"generated_at":  nowT.Format(time.RFC3339),
		"cached":        false,
	})
}

// handleAIAnalyzeValidate is the validate-page sibling of
// handleAIAnalyzeSymbol. POST /ai/analyze/validate with form params
// (short, tf, side, entry) — analyzes the user's PROPOSED trade
// hypothesis rather than the engine's own recommended setup. Same
// Anthropic call, same Quant persona; only the Diagnose section in
// the user message reflects the user's proposed side/entry instead
// of the engine's auto-pick.
//
// Cached by (short|tf|side|entry@4dp|signal-hash) so re-clicking on
// the same proposed trade doesn't re-bill.
func (s *server) handleAIAnalyzeValidate(c *gin.Context) {
	short := strings.ToUpper(strings.TrimSpace(c.PostForm("short")))
	tfStr := strings.TrimSpace(c.PostForm("tf"))
	sideStr := strings.ToLower(strings.TrimSpace(c.PostForm("side")))
	entryStr := strings.TrimSpace(c.PostForm("entry"))
	if short == "" || tfStr == "" || sideStr == "" || entryStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "short, tf, side, entry all required"})
		return
	}
	sym, err := resolveWebSymbol(short)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tf := market.Timeframe(tfStr)
	var side signal.Side
	switch sideStr {
	case "long":
		side = signal.Long
	case "short":
		side = signal.Short
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "side must be long or short"})
		return
	}
	var entry float64
	if _, err := fmt.Sscanf(entryStr, "%f", &entry); err != nil || entry <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "entry must be a positive number"})
		return
	}

	if s.ai == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai client not initialized"})
		return
	}
	apiKey := ai.APIKeyForProvider(s.ai)
	if apiKey == "" && !s.ai.IsDryRun() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "AI provider API key not configured on server (check AI_PROVIDER + matching GEMINI_API_KEY / ANTHROPIC_API_KEY in .env)."})
		return
	}

	ctx := c.Request.Context()
	candles, err := s.client.Klines(ctx, sym, tf, 200)
	if err != nil || len(candles) < 60 {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("klines fetch failed or insufficient bars: %v", err)})
		return
	}
	var markPrice float64
	if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil {
		markPrice = fr.MarkPrice
	}
	const dashboardFeeBps = 6.0
	d := validator.Validate(sym, tf, side, entry, dashboardFeeBps, candles, markPrice)

	// Cache key includes user-proposed entry rounded to 4dp + engine
	// signal hash so an unchanged proposal doesn't re-bill.
	view := symbolView{Signal: signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: candles, LiveMarkPrice: markPrice}), Diagnose: &d}
	cacheKey := fmt.Sprintf("validate|%s|%s|%s|%.4f|%d|%s",
		short, tfStr, sideStr, entry, view.Signal.Score, signalCacheTag(view))
	s.aiSymbolCacheMu.Lock()
	if cached, ok := s.aiSymbolCache[cacheKey]; ok {
		s.aiSymbolCacheMu.Unlock()
		c.JSON(http.StatusOK, gin.H{
			"text":          cached.Text,
			"input_tokens":  cached.InputTokens,
			"output_tokens": cached.OutputTokens,
			"cost_usd":      cached.CostUSD,
			"generated_at":  cached.GeneratedAt.Format(time.RFC3339),
			"cached":        true,
		})
		return
	}
	s.aiSymbolCacheMu.Unlock()

	// Build the LLM context. Reuse SymbolAnalysisInputs but flag the
	// rule-based summary to emphasize this is a USER hypothesis, not
	// the engine's pick.
	ruleSummary := signal.BuildSummary(view.Signal, diagnoseView(&d))
	var fundingRate float64
	if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil {
		fundingRate = fr.Rate
	}
	in := ai.SymbolAnalysisInputs{
		Symbol:        sym,
		Short:         short,
		Timeframe:     tf,
		Signal:        view.Signal,
		Summary:       fmt.Sprintf("USER PROPOSAL — %s %s @ %.4f. (Engine's own read: %s)", sideStr, short, entry, ruleSummary),
		MarkPrice:     markPrice,
		FundingRate:   fundingRate,
		RecentBars:    candles,
		StructureNote: structureNoteFor(candles),
		HigherTFs:     s.buildHigherTFSummaries(ctx, sym, tf),
	}
	sd := &ai.SymbolDiagnose{
		Side: d.Side, Entry: d.Entry, Total: d.Total, TotalMR: d.TotalMR, TotalMOM: d.TotalMOM,
		Verdict: d.Verdict,
		AtVAH:   d.AtVAH, AtVAL: d.AtVAL, InsideVA: d.InsideVA,
		OutsideVAUp: d.OutsideVAUp, OutsideVADn: d.OutsideVADn,
		POCTrend:    d.POCMig.Trend, POCDriftPct: d.POCMig.DriftPct, POCStacked: d.POCMig.Stacked,
		FallingKnife: d.RecentFlashBarBearish, BlowOff: d.RecentFlashBarBullish,
	}
	for _, f := range d.Factors {
		sd.Factors = append(sd.Factors, ai.SymbolDiagnoseFactor{
			Name: f.Name, Points: f.Points, Detail: f.Detail, Axis: f.Axis,
		})
	}
	in.Diagnose = sd

	allTrades, _ := journal.ReadAll("")
	for i := len(allTrades) - 1; i >= 0 && len(in.RecentSame) < 5; i-- {
		r := allTrades[i]
		if r.Symbol != short || r.ClosedAt.IsZero() {
			continue
		}
		in.RecentSame = append(in.RecentSame, r)
	}
	now := time.Now()
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(now)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		in.MacroNear = append(in.MacroNear, e)
	}

	userMsg := ai.BuildSymbolAnalysisMessage(in)
	resp, err := s.ai.Send(ctx, ai.SendOptions{
		APIKey:      apiKey,
		System:      ai.SystemPromptQuantAdvisor,
		Messages:    []ai.Message{{Role: "user", Content: userMsg}},
		Temperature: 0.3,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "AI provider call failed: " + err.Error()})
		return
	}
	cost := resp.EstimatedCostUSD()
	nowT := time.Now().UTC()
	s.aiSymbolCacheMu.Lock()
	s.aiSymbolCache[cacheKey] = aiCacheEntry{
		Text:         resp.Text,
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		CostUSD:      cost,
		GeneratedAt:  nowT,
	}
	s.aiSymbolCacheMu.Unlock()
	log.Printf("ai.analyze validate %s/%s %s @ %.4f: %d/%d tokens, est $%.4f",
		short, tfStr, sideStr, entry, resp.InputTokens, resp.OutputTokens, cost)
	c.JSON(http.StatusOK, gin.H{
		"text":          resp.Text,
		"input_tokens":  resp.InputTokens,
		"output_tokens": resp.OutputTokens,
		"cost_usd":      cost,
		"generated_at":  nowT.Format(time.RFC3339),
		"cached":        false,
	})
}

// signalCacheTag derives a stable per-setup hash component so cache
// keys turn over only on meaningful changes (verdict-band / anchor /
// regime), not on every dashboard refresh's tiny price drift.
func signalCacheTag(v symbolView) string {
	verdict := ""
	side := ""
	if v.Diagnose != nil {
		verdict = verdictShortHelper(v.Diagnose.Verdict)
		side = v.Diagnose.Side.String()
	}
	anchor := ""
	if v.Signal.Plan.Anchor != "" {
		anchor = v.Signal.Plan.Anchor
	}
	return fmt.Sprintf("%s|%s|%s|%s", v.Signal.Side, side, verdict, anchor)
}

// handleAIModelGet returns the current AI model + the list of models
// available on the configured provider. UI populates a dropdown from
// this. Only meaningful when provider is Gemini — Anthropic doesn't
// expose a ListModels equivalent for this purpose.
func (s *server) handleAIModelGet(c *gin.Context) {
	gc, ok := s.ai.(*ai.GeminiClient)
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"provider":  s.aiProviderName,
			"current":   "(provider does not support runtime model switch)",
			"available": []string{},
		})
		return
	}
	apiKey := ai.APIKeyForProvider(s.ai)
	models, err := gc.ListModels(c.Request.Context(), apiKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "list models failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"provider":  s.aiProviderName,
		"current":   gc.CurrentModel(),
		"available": models,
	})
}

// handleAIModelPost sets the active AI model. Validates against
// ListModels' filtered list, updates in-memory + persists to
// s.aiModelPath. Body: form field "model=<name>".
func (s *server) handleAIModelPost(c *gin.Context) {
	gc, ok := s.ai.(*ai.GeminiClient)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider does not support runtime model switch"})
		return
	}
	name := strings.TrimSpace(c.PostForm("model"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model field required"})
		return
	}
	apiKey := ai.APIKeyForProvider(s.ai)
	models, err := gc.ListModels(c.Request.Context(), apiKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "list models failed: " + err.Error()})
		return
	}
	// Validate: must be in the filtered available list.
	valid := false
	for _, m := range models {
		if m == name {
			valid = true
			break
		}
	}
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model not in available list; refresh /api/ai/model"})
		return
	}
	gc.SetModel(name)
	if err := gc.PersistModel(s.aiModelPath); err != nil {
		log.Printf("[ai] persist model failed (in-memory change still applied): %v", err)
	}
	log.Printf("[ai] runtime model changed to %s (persisted to %s)", name, s.aiModelPath)
	c.JSON(http.StatusOK, gin.H{"current": name, "persisted": true})
}

// tfDurationSeconds returns the bar duration for a supported timeframe.
// Used by handleChartData to estimate the start-time window for
// KlinesRange when the user requests deep historical data. Mirror of
// bingx.tfDuration (package-private there).
func tfDurationSeconds(tf market.Timeframe) time.Duration {
	switch tf {
	case "1m":
		return time.Minute
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "2h":
		return 2 * time.Hour
	case "4h":
		return 4 * time.Hour
	case "6h":
		return 6 * time.Hour
	case "12h":
		return 12 * time.Hour
	case "1d":
		return 24 * time.Hour
	}
	return time.Hour
}

// handleChartPage renders /chart — the full-page interactive candlestick
// view backed by TradingView Lightweight Charts. Server-side just emits
// the shell + hydrates via /api/chart/data. Symbol / TF chosen client-
// side so switching doesn't require a full page reload.
func (s *server) handleChartPage(c *gin.Context) {
	// Force browsers to re-fetch the page HTML every visit — the
	// embedded JS is where the live-price polling lives, so a
	// user with a cached shell will silently run last-week's code.
	c.Header("Cache-Control", "no-store, must-revalidate")
	c.HTML(http.StatusOK, "chart.html", gin.H{
		"Symbol":  strings.ToUpper(defaultStr(c.Query("symbol"), "BTC")),
		"TF":      defaultStr(c.Query("tf"), "1h"),
		"Symbols": []string{"BTC", "ETH", "XAU", "XAG"},
		"TFs":     []string{"15m", "30m", "1h", "2h", "4h", "1d"},
	})
}

// handleChartData — GET /api/chart/data?symbol=X&tf=Y&limit=N
// Returns OHLCV candles + Bollinger arrays + current Signal / Diagnose /
// rule-based summary in one payload. Called by the /chart page JS on
// load AND on every symbol/TF change. Cached briefly via HTTP headers.
func (s *server) handleChartData(c *gin.Context) {
	short := strings.ToUpper(strings.TrimSpace(c.DefaultQuery("symbol", "BTC")))
	tfStr := strings.TrimSpace(c.DefaultQuery("tf", "1h"))
	sym, err := resolveWebSymbol(short)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tf := market.Timeframe(tfStr)

	limit := 1000
	if v := c.Query("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
		if limit < 60 {
			limit = 60
		}
		if limit > 5000 {
			limit = 5000
		}
	}
	// `before` (unix seconds) — lazy-load pagination. When the chart JS
	// detects the user has panned near the left edge of loaded data, it
	// calls back with before=<earliest_loaded_time>. We return the batch
	// ending just before that timestamp so the client can prepend.
	var beforeSec int64
	if v := c.Query("before"); v != "" {
		fmt.Sscanf(v, "%d", &beforeSec)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Two paths:
	//  - "before" set (pagination): fetch older-only via KlinesRange from
	//    (before - limit * TF_duration) to before-1. Signal/Diagnose are
	//    not recomputed for historical requests — they'd be stale by
	//    definition. Return candles + bollinger only.
	//  - No "before" (initial load): full scanOne pipeline for
	//    Signal/Diagnose + Klines(limit) for the OHLCV window.
	var candles []market.Candle
	var view symbolView
	historicalOnly := beforeSec > 0

	if historicalOnly {
		endT := time.Unix(beforeSec-1, 0)
		startT := endT.Add(-time.Duration(limit) * tfDurationSeconds(tf))
		got, err := s.client.KlinesRange(ctx, sym, tf, startT, endT)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "klines range: " + err.Error()})
			return
		}
		candles = got
	} else {
		view = s.scanOne(ctx, sym, tf)
		if view.Err != "" {
			c.JSON(http.StatusBadGateway, gin.H{"error": view.Err})
			return
		}
		// Chart display uses KlinesWithForming to include the current
		// forming bar so the chart matches what BingX's own web UI
		// shows in real-time. Engine paths (view.Signal/Diagnose) still
		// use closed-only candles from scanOne — those stay canonical.
		if s.client != nil {
			fetch := limit
			if fetch > 1440 {
				fetch = 1440
			}
			if fresh, err := s.client.KlinesWithForming(ctx, sym, tf, fetch); err == nil && len(fresh) > 0 {
				candles = fresh
			} else {
				candles = view.Candles
			}
			// If user requested > 1440, extend with older bars via KlinesRange.
			if limit > 1440 && len(candles) > 0 {
				endT := candles[0].OpenTime.Add(-time.Second)
				startT := endT.Add(-time.Duration(limit-1440) * tfDurationSeconds(tf))
				if older, err := s.client.KlinesRange(ctx, sym, tf, startT, endT); err == nil && len(older) > 0 {
					candles = append(older, candles...)
				}
			}
		} else {
			candles = view.Candles
		}
	}

	// Format candles for LWC: { time: unix, open, high, low, close, volume }
	// LWC v4 accepts either businessDay or unix (seconds). We emit unix seconds.
	type ohlc struct {
		Time   int64   `json:"time"`
		Open   float64 `json:"open"`
		High   float64 `json:"high"`
		Low    float64 `json:"low"`
		Close  float64 `json:"close"`
		Volume float64 `json:"volume"`
	}
	type band struct {
		Time  int64   `json:"time"`
		Value float64 `json:"value"`
	}
	ohlcArr := make([]ohlc, 0, len(candles))
	closes := make([]float64, 0, len(candles))
	for _, k := range candles {
		ohlcArr = append(ohlcArr, ohlc{
			Time: k.OpenTime.Unix(), Open: k.Open, High: k.High, Low: k.Low, Close: k.Close, Volume: k.Volume,
		})
		closes = append(closes, k.Close)
	}

	// Volume-by-price profile for the right-side vertical histogram
	// (matches BingX-style "chip distribution" overlay). Splits each
	// candle's volume into buy (green candles: close >= open) vs sell
	// (red candles: close < open) at every price level the candle
	// touched. Backend gives raw buckets; frontend positions bars via
	// LWC priceScale.priceToCoordinate.
	const vpBins = 60
	vpBuckets := volumeByPrice(candles, vpBins)

	// Chart markers: sweep events + double-top/bottom + divergences +
	// range-expansion flash bars. Each entry maps to an LWC
	// series.setMarkers([]) item. Client picks color/shape via `kind`.
	chartMarkers := computeChartMarkers(candles)

	// Bollinger (20, 2) matches engine's canonical params. Emit three
	// aligned arrays offset by 19 bars (BB needs 20-bar warm-up).
	bb := indicator.Bollinger(closes, 20, 2)
	upper := make([]band, 0, len(bb))
	mid := make([]band, 0, len(bb))
	lower := make([]band, 0, len(bb))
	for i, b := range bb {
		if b.Mid == 0 {
			continue
		}
		t := candles[i].OpenTime.Unix()
		upper = append(upper, band{Time: t, Value: b.Upper})
		mid = append(mid, band{Time: t, Value: b.Mid})
		lower = append(lower, band{Time: t, Value: b.Lower})
	}

	// Historical-only paginated requests: skip signal/diagnose/plan
	// derivation — the caller only wants OHLCV + Bollinger for the older
	// window to render on the left side of the chart.
	if historicalOnly {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"symbol":        short,
			"tf":            tfStr,
			"candles":       ohlcArr,
			"bollinger":     gin.H{"upper": upper, "mid": mid, "lower": lower},
			"volumeProfile": vpBuckets,
			"markers":       chartMarkers,
			"paginated":     true,
		})
		return
	}

	// Plan levels — only emit when engine has a plan.
	var plan map[string]any
	if view.Signal.Plan.Entry > 0 {
		p := view.Signal.Plan
		plan = map[string]any{
			"entry":  p.Entry,
			"stop":   p.StopLoss,
			"anchor": p.Anchor,
			"side":   view.Signal.Side.String(),
		}
		if len(p.TakeProfit) >= 1 {
			plan["tp1"] = p.TakeProfit[0]
		}
		if len(p.TakeProfit) >= 2 {
			plan["tp2"] = p.TakeProfit[1]
		}
	}

	// Compact Signal + Diagnose projection.
	sig := map[string]any{
		"side":       view.Signal.Side.String(),
		"score":      view.Signal.Score,
		"mrScore":    view.Signal.MRScore,
		"momScore":   view.Signal.MomentumScore,
		"reasons":    view.Signal.Reasons,
		"notes":      view.Signal.Notes,
		"warnings":   view.Signal.Warnings,
	}
	var diag map[string]any
	if view.Diagnose != nil {
		d := view.Diagnose
		diag = map[string]any{
			"side":     d.Side.String(),
			"total":    d.Total,
			"totalMR":  d.TotalMR,
			"totalMOM": d.TotalMOM,
			"verdict":  verdictShortHelper(d.Verdict),
			"atVAH":    d.AtVAH,
			"atVAL":    d.AtVAL,
			"insideVA": d.InsideVA,
		}
	}

	c.Header("Cache-Control", "no-store")
	// Live market context — funding rate + OI snapshot for the banner
	// chips. Historical OI series is not available on BingX free/public
	// endpoints (per ROADMAP OI-delta note), so we only show the CURRENT
	// values; no historical OI markers can be computed.
	ctxSnap := map[string]any{
		"fundingRate":  view.Context.FundingRate,
		"openInterest": view.Context.OpenInterest,
	}

	c.JSON(http.StatusOK, gin.H{
		"symbol":        short,
		"tf":            tfStr,
		"contract":      string(sym),
		"candles":       ohlcArr,
		"bollinger":     gin.H{"upper": upper, "mid": mid, "lower": lower},
		"volumeProfile": vpBuckets,
		"markers":       chartMarkers,
		"plan":          plan,
		"signal":        sig,
		"diagnose":      diag,
		"summary":       view.Summary,
		"markPrice":     view.MarkPrice,
		"marketCtx":     ctxSnap,
	})
}

// computeChartMarkers derives event markers for the LWC candlestick
// series overlay: sweep events (from analyzer.DetectSweeps), double
// tops/bottoms, RSI/CVD divergence pivots, range-expansion flash bars.
//
// Each returned map matches LWC's setMarkers([]) format:
//   time     — unix seconds (matches candle time)
//   position — "aboveBar" | "belowBar" | "inBar"
//   color    — CSS color string
//   shape    — "circle" | "arrowUp" | "arrowDown" | "square"
//   text     — short label (e.g. "sweep", "2×top", "div")
//   kind     — internal category so frontend can filter/style further
//
// All input candles must be in ascending time order (as returned by
// bingx.Klines / KlinesWithForming). Marker times MUST also be sorted
// ascending for LWC — we sort at the end.
func computeChartMarkers(candles []market.Candle) []map[string]any {
	if len(candles) < 20 {
		return nil
	}
	var markers []map[string]any

	// Sweeps — analyzer.DetectSweeps is directional & already close-
	// confirmed. Only include confirmed ones (ReclaimOK) — the wick-
	// pierce-only ones are noise.
	sweeps := analyzer.DetectSweeps(candles, 0.0008, 2)
	for _, sw := range sweeps {
		if !sw.ReclaimOK {
			continue
		}
		if sw.SweepIdx < 0 || sw.SweepIdx >= len(candles) {
			continue
		}
		bar := candles[sw.SweepIdx]
		if sw.Side == analyzer.SweepHigh {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "aboveBar",
				"color":    "#e03131",
				"shape":    "arrowDown",
				"text":     "sweep",
				"kind":     "sweep_high",
			})
		} else {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "belowBar",
				"color":    "#2f9e44",
				"shape":    "arrowUp",
				"text":     "sweep",
				"kind":     "sweep_low",
			})
		}
	}

	// Double top / bottom patterns.
	dps := analyzer.DetectDoublePatterns(candles, 100, 2, 5, 5, 0.003, 0.01)
	for _, dp := range dps {
		if dp.PivotBIdx < 0 || dp.PivotBIdx >= len(candles) {
			continue
		}
		bar := candles[dp.PivotBIdx]
		if dp.Side == analyzer.DoubleTop {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "aboveBar",
				"color":    "#ae3ec9",
				"shape":    "circle",
				"text":     "2×top",
				"kind":     "double_top",
			})
		} else {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "belowBar",
				"color":    "#ae3ec9",
				"shape":    "circle",
				"text":     "2×bot",
				"kind":     "double_bottom",
			})
		}
	}

	// RSI divergence — pivot at the LATER of the two divergence points.
	closes := market.Closes(candles)
	rsi := indicator.RSI(closes, 14)
	div := analyzer.Detect(closes, rsi, 60, 2)
	if div.Kind != analyzer.NoDivergence && div.PivotB >= 0 && div.PivotB < len(candles) {
		bar := candles[div.PivotB]
		bearish := div.Kind == analyzer.BearishRegular || div.Kind == analyzer.BearishHidden
		if bearish {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "aboveBar",
				"color":    "#adb5bd",
				"shape":    "square",
				"text":     "div-",
				"kind":     "rsi_bear_div",
			})
		} else {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "belowBar",
				"color":    "#adb5bd",
				"shape":    "square",
				"text":     "div+",
				"kind":     "rsi_bull_div",
			})
		}
	}

	// Volume anomaly bars — signal-bar volume > 3× 20-bar avg AND
	// range > 1.5× ATR. Mirrors engine's BTC MOM vote criteria but we
	// mark all such bars on chart regardless of symbol (visual only).
	atr := indicator.ATR(candles, 14)
	volAnomalyByIdx := map[int]bool{}
	for i := 20; i < len(candles); i++ {
		bar := candles[i]
		barRange := bar.High - bar.Low
		if barRange <= 1.5*atr[i] {
			continue
		}
		var avgVol float64
		for j := i - 20; j < i; j++ {
			avgVol += candles[j].Volume
		}
		avgVol /= 20.0
		if avgVol <= 0 || bar.Volume < 3.0*avgVol {
			continue
		}
		volAnomalyByIdx[i] = true
		markers = append(markers, map[string]any{
			"time":     bar.OpenTime.Unix(),
			"position": "aboveBar",
			"color":    "#fab005",
			"shape":    "circle",
			"text":     "vol×",
			"kind":     "vol_anomaly",
		})
	}

	// Bollinger band touch / break events. Bar High piercing upper band
	// = potential mean-rev SHORT setup; Bar Low piercing lower band =
	// potential mean-rev LONG. "Break" (close outside band) is rarer +
	// more significant than "touch" (wick outside, close inside).
	// Track per-bar hits so the turning-point flag pass below can
	// cross-reference confluence.
	bb := indicator.Bollinger(closes, 20, 2)
	bbUpperByIdx := map[int]bool{}
	bbLowerByIdx := map[int]bool{}
	for i, b := range bb {
		if b.Mid == 0 {
			continue
		}
		bar := candles[i]
		// Upper band: high > upper (touch) or close > upper (break).
		if bar.High >= b.Upper {
			bbUpperByIdx[i] = true
			kind := "bb_touch_upper"
			text := "BB↑"
			color := "#e8590c"
			if bar.Close > b.Upper {
				kind = "bb_break_upper"
				text = "BB!↑"
				color = "#d9480f"
			}
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "aboveBar",
				"color":    color,
				"shape":    "arrowDown",
				"text":     text,
				"kind":     kind,
			})
		}
		if bar.Low <= b.Lower {
			bbLowerByIdx[i] = true
			kind := "bb_touch_lower"
			text := "BB↓"
			color := "#1971c2"
			if bar.Close < b.Lower {
				kind = "bb_break_lower"
				text = "BB!↓"
				color = "#1864ab"
			}
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "belowBar",
				"color":    color,
				"shape":    "arrowUp",
				"text":     text,
				"kind":     kind,
			})
		}
	}

	// RSI extremes — track for confluence check (not emitted as own
	// markers, since the RSI divergence marker already covers the
	// interesting cases).
	rsiOversoldByIdx := map[int]bool{}
	rsiOverboughtByIdx := map[int]bool{}
	for i := 14; i < len(candles); i++ {
		if i >= len(rsi) {
			break
		}
		if rsi[i] < 30 {
			rsiOversoldByIdx[i] = true
		} else if rsi[i] > 70 {
			rsiOverboughtByIdx[i] = true
		}
	}

	// Sweep-by-index maps for confluence check.
	sweepHighByIdx := map[int]bool{}
	sweepLowByIdx := map[int]bool{}
	for _, sw := range sweeps {
		if !sw.ReclaimOK || sw.SweepIdx < 0 || sw.SweepIdx >= len(candles) {
			continue
		}
		if sw.Side == analyzer.SweepHigh {
			sweepHighByIdx[sw.SweepIdx] = true
		} else {
			sweepLowByIdx[sw.SweepIdx] = true
		}
	}

	// Turning-point flags — bars with ≥2 concurrent bullish OR ≥2
	// concurrent bearish confluences from {BB touch, sweep, RSI
	// extreme, vol anomaly}. These are the setups the engine considers
	// tradeable, rendered as bigger flag markers so they pop out of the
	// smaller event markers.
	for i := 20; i < len(candles); i++ {
		bull := 0
		bear := 0
		if bbLowerByIdx[i] {
			bull++
		}
		if bbUpperByIdx[i] {
			bear++
		}
		if sweepLowByIdx[i] {
			bull++
		}
		if sweepHighByIdx[i] {
			bear++
		}
		if rsiOversoldByIdx[i] {
			bull++
		}
		if rsiOverboughtByIdx[i] {
			bear++
		}
		if volAnomalyByIdx[i] {
			bar := candles[i]
			if bar.Close >= bar.Open {
				bull++
			} else {
				bear++
			}
		}
		bar := candles[i]
		if bull >= 2 && bear < 2 {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "belowBar",
				"color":    "#2f9e44",
				"shape":    "arrowUp",
				"text":     "⚑ LONG",
				"kind":     "flag_long",
			})
		} else if bear >= 2 && bull < 2 {
			markers = append(markers, map[string]any{
				"time":     bar.OpenTime.Unix(),
				"position": "aboveBar",
				"color":    "#e03131",
				"shape":    "arrowDown",
				"text":     "⚑ SHORT",
				"kind":     "flag_short",
			})
		}
	}

	// Sort ascending by time (LWC requires it) + dedup by (time, kind).
	sort.Slice(markers, func(i, j int) bool {
		ti, _ := markers[i]["time"].(int64)
		tj, _ := markers[j]["time"].(int64)
		return ti < tj
	})
	// Simple dedup — same time+kind combo (e.g. two sweep_high computations
	// hitting the same bar) collapses to first.
	seen := map[string]bool{}
	out := markers[:0]
	for _, m := range markers {
		t, _ := m["time"].(int64)
		k, _ := m["kind"].(string)
		key := fmt.Sprintf("%d|%s", t, k)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// volumeByPrice builds a per-price-bucket buy/sell volume histogram
// suitable for a right-side "chip distribution" overlay. Each candle's
// volume is uniformly attributed across its [low, high] range and
// classified buy (close >= open) or sell (close < open). Mirrors the
// existing indicator.BuildVolumeProfile shape but splits by direction.
func volumeByPrice(candles []market.Candle, bins int) []map[string]any {
	if len(candles) < 2 || bins < 2 {
		return nil
	}
	minP, maxP := candles[0].Low, candles[0].High
	for _, c := range candles {
		if c.Low < minP {
			minP = c.Low
		}
		if c.High > maxP {
			maxP = c.High
		}
	}
	if maxP <= minP {
		return nil
	}
	binWidth := (maxP - minP) / float64(bins)
	buy := make([]float64, bins)
	sell := make([]float64, bins)
	for _, c := range candles {
		if c.Volume <= 0 || c.High <= c.Low {
			continue
		}
		sb := int((c.Low - minP) / binWidth)
		eb := int((c.High - minP) / binWidth)
		if sb < 0 {
			sb = 0
		}
		if eb >= bins {
			eb = bins - 1
		}
		if eb < sb {
			eb = sb
		}
		n := eb - sb + 1
		per := c.Volume / float64(n)
		bull := c.Close >= c.Open
		for i := sb; i <= eb; i++ {
			if bull {
				buy[i] += per
			} else {
				sell[i] += per
			}
		}
	}
	out := make([]map[string]any, bins)
	// Find POC (bin with highest total volume) for a UI callout.
	pocIdx := 0
	pocMax := 0.0
	for i := 0; i < bins; i++ {
		total := buy[i] + sell[i]
		if total > pocMax {
			pocMax = total
			pocIdx = i
		}
	}
	for i := 0; i < bins; i++ {
		out[i] = map[string]any{
			"priceLow":  minP + float64(i)*binWidth,
			"priceHigh": minP + float64(i+1)*binWidth,
			"buy":       buy[i],
			"sell":      sell[i],
			"isPOC":     i == pocIdx,
		}
	}
	return out
}
