package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"

	"github.com/gin-gonic/gin"
)

// Multi-TF bias strip: for each timeframe, a composite directional vote of
// three lenses — {N-struct, POC-drift regime, engine signal} — the same
// three reads used by hand when judging TF confluence. Rendered as a
// coloured chip row on /chart so alignment across TFs is visible at a
// glance (5m 🟢 · 15m ⚪ · 1h 🔴 · 2h 🟢 · 4h 🟢).

var biasStripTFs = []string{"5m", "15m", "1h", "2h", "4h"}

type biasCacheEntry struct {
	at   time.Time
	resp gin.H
}

var (
	biasCacheMu sync.Mutex
	biasCache   = map[string]biasCacheEntry{}
)

const biasCacheTTL = 30 * time.Second

// computeTFBias combines the three lenses into a single [-3,+3] score plus a
// direction label. Each lens contributes at most ±1, so no single lens can
// dominate — a TF only reads "strong" when at least two lenses agree.
func computeTFBias(st signal.StructureState, sig signal.Signal) gin.H {
	// Lens 1 — N-struct: trend classification, nudged by any structural
	// event (CHoCH/BOS), clamped to ±1.
	sv := 0
	switch st.Trend {
	case signal.StructUptrend:
		sv = 1
	case signal.StructDowntrend:
		sv = -1
	}
	switch st.Event {
	case signal.EvCHoCHUp, signal.EvBOSUp:
		sv++
	case signal.EvCHoCHDown, signal.EvBOSDown:
		sv--
	}
	if sv > 1 {
		sv = 1
	} else if sv < -1 {
		sv = -1
	}

	// Lens 2 — POC-drift regime: only counts when the ladder is stacked
	// (high conviction). A non-stacked drift is treated as flat.
	pv := 0
	if sig.POCMig.Stacked {
		switch sig.POCMig.Trend {
		case indicator.POCRising:
			pv = 1
		case indicator.POCFalling:
			pv = -1
		}
	}

	// Lens 3 — engine signal.
	ev := 0
	switch sig.Side {
	case signal.Long:
		ev = 1
	case signal.Short:
		ev = -1
	}

	score := sv + pv + ev
	dir := "neutral"
	switch {
	case score >= 2:
		dir = "long"
	case score == 1:
		dir = "long-lean"
	case score == -1:
		dir = "short-lean"
	case score <= -2:
		dir = "short"
	}

	// Component labels for the hover tooltip.
	structLabel := st.Trend.String()
	if st.Event != signal.EvNone {
		structLabel += " · " + st.Event.String()
	}
	pocArrow := "→"
	switch sig.POCMig.Trend {
	case indicator.POCRising:
		pocArrow = "↗"
	case indicator.POCFalling:
		pocArrow = "↘"
	}
	pocLabel := fmt.Sprintf("POC %s %+.1f%%", pocArrow, sig.POCMig.DriftPct*100)
	if sig.POCMig.Stacked {
		pocLabel += " stacked"
	} else {
		pocLabel += " 未疊"
	}

	return gin.H{
		"dir":        dir,
		"score":      score,
		"struct":     structLabel,
		"structVote": sv,
		"poc":        pocLabel,
		"pocVote":    pv,
		"engine":     sig.Side.String(),
		"engineVote": ev,
	}
}

// handleChartBias — GET /api/chart/bias?symbol=X
// Returns the multi-TF bias strip for the symbol. Cached 30s per symbol
// because each TF is a full scanOne (~0.5s) and the strip is refreshed on
// the chart's 20s poll.
func (s *server) handleChartBias(c *gin.Context) {
	short := strings.ToUpper(strings.TrimSpace(c.DefaultQuery("symbol", "BTC")))
	sym, err := resolveWebSymbol(short)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	biasCacheMu.Lock()
	if e, ok := biasCache[short]; ok && time.Since(e.at) < biasCacheTTL {
		resp := e.resp
		biasCacheMu.Unlock()
		c.JSON(http.StatusOK, resp)
		return
	}
	biasCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	tfs := make([]gin.H, 0, len(biasStripTFs))
	for _, tfStr := range biasStripTFs {
		tf := market.Timeframe(tfStr)
		view := s.scanOne(ctx, sym, tf)
		if view.Err != "" {
			tfs = append(tfs, gin.H{"tf": tfStr, "dir": "na", "score": 0})
			continue
		}
		st := signal.AnalyzeStructure(view.Candles, 2)
		b := computeTFBias(st, view.Signal)
		b["tf"] = tfStr
		tfs = append(tfs, b)
	}

	resp := gin.H{"symbol": short, "tfs": tfs}
	biasCacheMu.Lock()
	biasCache[short] = biasCacheEntry{at: time.Now(), resp: resp}
	biasCacheMu.Unlock()
	c.JSON(http.StatusOK, resp)
}

// ── Top ticker bar ──────────────────────────────────────────────────
// Live price + 24h change % for every tradeable symbol, so the trader
// can watch all four without switching the <select>. 24h change = last
// close vs the close ~24h ago on the 1h series, matching the daily % the
// chart header shows next to the price.

var tickerSymbols = []string{"BTC", "ETH", "XAU", "XAG"}

type tickerCacheT struct {
	at   time.Time
	resp gin.H
}

var (
	tickerCacheMu sync.Mutex
	tickerCache   tickerCacheT
)

const tickerCacheTTL = 12 * time.Second

// handleTickers — GET /api/tickers
func (s *server) handleTickers(c *gin.Context) {
	tickerCacheMu.Lock()
	if tickerCache.resp != nil && time.Since(tickerCache.at) < tickerCacheTTL {
		resp := tickerCache.resp
		tickerCacheMu.Unlock()
		c.JSON(http.StatusOK, resp)
		return
	}
	tickerCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	out := make([]gin.H, 0, len(tickerSymbols))
	for _, short := range tickerSymbols {
		row := gin.H{"symbol": short}
		sym, err := resolveWebSymbol(short)
		if err == nil && s.client != nil {
			// 26 hourly bars ≈ 25h: enough to look back a full 24h and
			// still keep the current forming bar as the live price.
			if ks, err := s.client.KlinesWithForming(ctx, sym, market.Timeframe("1h"), 26); err == nil && len(ks) > 0 {
				last := ks[len(ks)-1].Close
				row["price"] = last
				ref := ks[0].Close
				if idx := len(ks) - 1 - 24; idx >= 0 {
					ref = ks[idx].Close
				}
				if ref > 0 {
					row["changePct"] = (last - ref) / ref * 100
				}
			}
		}
		out = append(out, row)
	}

	resp := gin.H{"tickers": out}
	tickerCacheMu.Lock()
	tickerCache = tickerCacheT{at: time.Now(), resp: resp}
	tickerCacheMu.Unlock()
	c.JSON(http.StatusOK, resp)
}
