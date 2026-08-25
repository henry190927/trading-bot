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
	var refCandles []market.Candle
	var refPrice float64
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
		if tfStr == "1h" {
			refCandles, refPrice = view.Candles, view.Signal.Price
		}
	}

	resp := gin.H{"symbol": short, "tfs": tfs, "alignment": computeAlignment(tfs)}
	if h := computeHVNTargets(refCandles, refPrice); h != nil {
		resp["hvn"] = h
	}
	biasCacheMu.Lock()
	biasCache[short] = biasCacheEntry{at: time.Now(), resp: resp}
	biasCacheMu.Unlock()
	c.JSON(http.StatusOK, resp)
}

// computeAlignment summarizes multi-TF agreement so the UI can flag a "TF
// conflict" — higher TFs (2h/4h) leaning one way while lower TFs (5m/15m/1h)
// lean the other. That's the low-conviction trap: e.g. buying a 4h pullback
// zone while 1h/15m have already turned down (an M-top). Aligned = higher
// conviction; conflict = wait / size down.
func computeAlignment(tfs []gin.H) gin.H {
	htf := map[string]bool{"2h": true, "4h": true}
	htfSum, ltfSum := 0, 0
	for _, t := range tfs {
		sc, _ := t["score"].(int)
		tf, _ := t["tf"].(string)
		if htf[tf] {
			htfSum += sc
		} else {
			ltfSum += sc
		}
	}
	sign := func(n int) int {
		switch {
		case n > 0:
			return 1
		case n < 0:
			return -1
		default:
			return 0
		}
	}
	word := func(s int) string {
		switch {
		case s > 0:
			return "多"
		case s < 0:
			return "空"
		default:
			return "中性"
		}
	}
	h, l := sign(htfSum), sign(ltfSum)
	switch {
	case h != 0 && l != 0 && h != l:
		return gin.H{"state": "conflict", "label": "⚠ TF 衝突 HTF" + word(h) + "/LTF" + word(l)}
	case h >= 0 && l >= 0 && (h > 0 || l > 0):
		return gin.H{"state": "aligned-long", "label": "✓ TF 一致偏多"}
	case h <= 0 && l <= 0 && (h < 0 || l < 0):
		return gin.H{"state": "aligned-short", "label": "✓ TF 一致偏空"}
	default:
		return gin.H{"state": "mixed", "label": "TF 混合/中性"}
	}
}

// computeHVNTargets returns the nearest volume HVN (chip-concentration) above
// and below the current price — the "target = 短期籌碼密集區" the SMC read uses.
// Built from a RECENT 1h window (~100 bars) so the profile reflects current
// positioning, not the stale multi-week accumulation range. Returns nil if
// there isn't enough data.
func computeHVNTargets(candles []market.Candle, price float64) gin.H {
	if len(candles) < 40 || price <= 0 {
		return nil
	}
	start := len(candles) - 100
	if start < 0 {
		start = 0
	}
	vp := indicator.BuildVolumeProfile(candles[start:], 80, 6)
	var above, below float64
	for _, h := range vp.HVN {
		if h > price {
			if above == 0 || h < above {
				above = h
			}
		} else if h < price {
			if below == 0 || h > below {
				below = h
			}
		}
	}
	if above == 0 && below == 0 {
		return nil
	}
	out := gin.H{"poc": vp.POC}
	if above > 0 {
		out["above"] = above // nearest HVN overhead — long target / short cap
	}
	if below > 0 {
		out["below"] = below // nearest HVN underneath — short target / long floor
	}
	return out
}

// ── Top ticker bar ──────────────────────────────────────────────────
// Live price + 24h change % for every tradeable symbol, so the trader
// can watch all four without switching the <select>. 24h change = last
// close vs the close ~24h ago on the 1h series, matching the daily % the
// chart header shows next to the price.

// tickerSymbols = the full UI universe (core + stock + alt forward-log), so the
// chart's top ticker shows everything. Aliased to uiSymbols to stay in sync
// whenever symbols are added. The strip scrolls horizontally when it overflows.
var tickerSymbols = uiSymbols

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

	// Fan out across the (now larger) symbol set so the strip stays snappy —
	// each symbol is 2 API calls (mark + klines); sequential over 11 symbols
	// would crawl. Output order is preserved by index.
	out := make([]gin.H, len(tickerSymbols))
	var wg sync.WaitGroup
	for i, short := range tickerSymbols {
		wg.Add(1)
		go func(idx int, short string) {
			defer wg.Done()
			row := gin.H{"symbol": short}
			sym, err := resolveWebSymbol(short)
			if err == nil && s.client != nil {
				// Price = live mark, SAME source as the chart header, so the
				// ticker and the header never disagree. Klines are only for the
				// 24h reference close (change %).
				price := 0.0
				if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil && fr.MarkPrice > 0 {
					price = fr.MarkPrice
				}
				// 26 hourly bars ≈ 25h: enough to look back a full 24h.
				if ks, kerr := s.client.KlinesWithForming(ctx, sym, market.Timeframe("1h"), 26); kerr == nil && len(ks) > 0 {
					if price == 0 {
						price = ks[len(ks)-1].Close // mark fetch failed — fall back to last close
					}
					ref := ks[0].Close
					if j := len(ks) - 1 - 24; j >= 0 {
						ref = ks[j].Close
					}
					if ref > 0 && price > 0 {
						row["changePct"] = (price - ref) / ref * 100
					}
				}
				if price > 0 {
					row["price"] = price
				}
			}
			out[idx] = row
		}(i, short)
	}
	wg.Wait()

	resp := gin.H{"tickers": out}
	tickerCacheMu.Lock()
	tickerCache = tickerCacheT{at: time.Now(), resp: resp}
	tickerCacheMu.Unlock()
	c.JSON(http.StatusOK, resp)
}
