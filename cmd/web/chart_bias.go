package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/binfut"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/oi"
	"github.com/henry190927/trading-bot/signal"

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

// biasCall is one in-flight recompute. Requests that arrive for a symbol
// already being recomputed wait on done and read resp, rather than each
// starting their own full multi-TF scan.
type biasCall struct {
	done chan struct{}
	once sync.Once
	resp gin.H
}

var (
	biasCacheMu  sync.Mutex
	biasCache    = map[string]biasCacheEntry{}
	biasInflight = map[string]*biasCall{}
)

const biasCacheTTL = 30 * time.Second

// computeTFBias combines the three lenses into a single [-3,+3] score plus a
// direction label. Each lens contributes at most ±1, so no single lens can
// dominate — a TF only reads "strong" when at least two lenses agree.
func computeTFBias(tf market.Timeframe, st signal.StructureState, sig signal.Signal) gin.H {
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
	// Name the WINDOW, not just the number. DriftPct is
	// (POC50 - POC200) / POC200, so the span is 200 bars OF THIS TIMEFRAME —
	// on 4h that is 800 hours, about 33 days. A chip read as "the 4h bias"
	// was reporting a month-old drift: ETH printed "POC ↗ +32.5% stacked" on
	// 2026-09-11 because it ran 1,900 -> 2,475 since mid-August, and that one
	// vote was what tipped the alignment label to "一致偏多" while the 1h
	// lens read short. The figure was never wrong; the label let it be read
	// as recent momentum.
	span := ""
	if d := market.BarDuration(tf); d > 0 {
		if days := (200 * d).Hours() / 24; days >= 1 {
			span = fmt.Sprintf("/200根≈%.0f天", days)
		} else {
			span = fmt.Sprintf("/200根≈%.0f小時", (200 * d).Hours())
		}
	}
	pocLabel := fmt.Sprintf("POC%s %s %+.1f%%", span, pocArrow, sig.POCMig.DriftPct*100)
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

// computeChartOI is the open-interest read for the chart, from the same store
// and the same rule as the /ops card (oiQuadrant).
//
// FIXED at a 1-hour window regardless of the chart's timeframe, and that is a
// data limit rather than a choice. BingX publishes no OI history endpoint —
// both openInterestHist spellings answer "this api is not exist" — so the only
// series is the one cmd/monitor samples, and the venue itself republishes the
// figure only about every ten minutes: 112 of 254 consecutive 5-minute samples
// came back byte-identical. A delta measured over less than an hour would
// therefore be reporting the publication cadence. An hour spans roughly six
// published values, which is enough.
//
// Which is also why this reports a NUMBER and not a sparkline. The same source
// that supports "OI is up 4.7% over the hour" cannot support a slope drawn at
// chart resolution, and drawing one would render a publication artifact as
// market structure.
//
// BingX on purpose, not Binance. Binance publishes a real 5m series and a
// whale/retail split, but it is a different book — this answers "is my
// position on the crowded side of the exchange my order actually sits in",
// and for that the venue you trade on is the right one.
func computeChartOI(sym market.Symbol) gin.H {
	snaps := oi.Load()
	if len(snaps) == 0 {
		return nil
	}
	now := time.Now().UTC()
	cur, ok := oi.Latest(snaps, string(sym))
	if !ok || cur.OI <= 0 {
		return nil
	}
	out := gin.H{
		"oi":      cur.OI,
		"funding": cur.Funding,
		"ageMin":  int(now.Sub(cur.Time).Minutes()),
		"window":  "1h",
	}
	// The synthetics publish a frozen figure — one distinct value across 30
	// samples — so their delta is structurally zero forever. Say so instead of
	// rendering 0.00%, which reads as "quiet" rather than "impossible".
	if oiIsStatic(snaps, string(sym)) {
		out["static"] = true
		// Do NOT return early any more. The frozen synthetics are exactly the
		// symbols Binance can answer for and BingX cannot, so skipping the
		// cross-reference here would blank the only readable OI they have.
	}
	// Binance cross-reference, kept in separate keys. Different book: an
	// entry fills against BingX's, so these never merge into the figures
	// above. The 15m reading exists only on this side — Binance publishes
	// genuine 5-minute buckets, while BingX would be re-reading one value.
	if short := market.Short(sym); short != "" {
		bf := binfut.Load()
		if d, ok := bf.OIChange(short, time.Hour); ok {
			out["bnDelta1h"] = d
		}
		if d, ok := bf.OIChange(short, 15*time.Minute); ok {
			out["bnDelta15m"] = d
		}
		if w, ok := bf.Whales(short); ok {
			out["whaleTop"] = w.Top
			out["whaleAll"] = w.All
			if c := w.Crowd(); c != "" {
				out["crowd"] = c
			}
		}
	}

	if out["static"] == true {
		return out
	}
	bar := market.BarDuration(market.TF1h)
	prev := oi.PrevFor(snaps, string(sym), bar, now)
	if prev <= 0 {
		return out
	}
	d := (cur.OI - prev) / prev
	out["delta"] = d
	if pd, ok := oi.PriceChangeOver(snaps, string(sym), bar, now); ok {
		out["priceDelta"] = pd
		if key, label := oiQuadrant(d, pd); key != "" {
			out["quadrant"] = key
			out["reading"] = label
		}
	} else if d <= -oiDeltaThreshold || d >= oiDeltaThreshold {
		out["quadrant"] = "no-price"
		out["reading"] = "OI 已過門檻,但缺同窗價格,無法判邊"
	}
	return out
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

	// Three states, not two: fresh cache, someone else already recomputing,
	// or this request does the work.
	biasCacheMu.Lock()
	if e, ok := biasCache[short]; ok && time.Since(e.at) < biasCacheTTL {
		resp := e.resp
		biasCacheMu.Unlock()
		c.JSON(http.StatusOK, resp)
		return
	}
	// A recompute costs a multi-TF scan. The chart polls this endpoint on a
	// timer, so without single-flight every TTL expiry let each poll tick that
	// landed during the recompute start its OWN full scan — none of them able
	// to see the others. Followers wait for the leader's result instead.
	if call, ok := biasInflight[short]; ok {
		biasCacheMu.Unlock()
		select {
		case <-call.done:
			if call.resp == nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "bias recompute failed"})
				return
			}
			c.JSON(http.StatusOK, call.resp)
		case <-c.Request.Context().Done():
			// Caller gave up; nothing to write.
		}
		return
	}
	call := &biasCall{done: make(chan struct{})}
	biasInflight[short] = call
	biasCacheMu.Unlock()
	defer func() {
		biasCacheMu.Lock()
		delete(biasInflight, short)
		biasCacheMu.Unlock()
		// Always closed, including on a panic, so followers are never stranded.
		// call.resp stays nil in that case and they get a 502.
		call.once.Do(func() { close(call.done) })
	}()

	// Deliberately NOT c.Request.Context(): this result is shared with every
	// follower waiting on call.done, so one client navigating away must not
	// cancel work the others are still waiting for.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One scanOne per TF, and they are independent — the 5m read does not feed
	// the 4h read. Run sequentially this loop WAS the cache miss: five scans at
	// ~470ms each, ~2.4s total. Run together it costs one scan.
	//
	// Results are written by index rather than appended, so the strip keeps its
	// 5m→4h order no matter which scan finishes first. Distinct indices of a
	// pre-sized slice are safe to write concurrently. refCandles/refPrice are
	// written only by the "1h" iteration — a single writer — and read after
	// Wait.
	tfs := make([]gin.H, len(biasStripTFs))
	var refCandles []market.Candle
	var refPrice float64
	var wg sync.WaitGroup
	for i, tfStr := range biasStripTFs {
		wg.Add(1)
		go func(i int, tfStr string) {
			defer wg.Done()
			tf := market.Timeframe(tfStr)
			view := s.scanOne(ctx, sym, tf)
			if view.Err != "" {
				tfs[i] = gin.H{"tf": tfStr, "dir": "na", "score": 0}
				return
			}
			st := signal.AnalyzeStructure(view.Candles, 2)
			b := computeTFBias(tf, st, view.Signal)
			b["tf"] = tfStr
			tfs[i] = b
			if tfStr == "1h" {
				refCandles, refPrice = view.Candles, view.Signal.Price
			}
		}(i, tfStr)
	}
	wg.Wait()

	align := computeAlignment(tfs)
	resp := gin.H{"symbol": short, "tfs": tfs, "alignment": align}
	if h := computeHVNTargets(refCandles, refPrice); h != nil {
		resp["hvn"] = h
	}
	if rg := computeRange(refCandles, refPrice, fmt.Sprint(align["state"])); rg != nil {
		resp["range"] = rg
	}
	if o := computeChartOI(sym); o != nil {
		resp["oi"] = o
	}
	call.resp = resp // published to followers when the deferred close fires
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
//
// CONVICTION GATE: an earlier version only looked at the SIGN of the HTF/LTF
// score sums, so a single leaning TF (15m -1) with four neutrals printed a
// green "✓ TF 一致偏空" — and because computeRange keys isRange off this state,
// that phantom trend also switched the range read-aid OFF in a dead-flat chop
// ("趨勢中,別 fade 邊緣" while price sat mid-box). A real "一致" now needs
// breadth: ≥2 TFs leaning the same way, more agreeing than opposing, and
// either an HTF on board or the whole LTF stack (≥3). Anything thinner is
// weak-long/weak-short — treated as no-trend, which lets range mode engage.
func computeAlignment(tfs []gin.H) gin.H {
	htf := map[string]bool{"2h": true, "4h": true}
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

	htfSum, ltfSum := 0, 0
	signs := make(map[string]int, len(tfs))
	for _, t := range tfs {
		sc, _ := t["score"].(int)
		tf, _ := t["tf"].(string)
		signs[tf] = sign(sc)
		if htf[tf] {
			htfSum += sc
		} else {
			ltfSum += sc
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
	// HTF and LTF pulling opposite ways is a real signal at any magnitude —
	// it stays the top-priority read, unchanged.
	if h != 0 && l != 0 && h != l {
		return gin.H{
			"state": "conflict", "label": "⚠ TF 衝突 HTF" + word(h) + "/LTF" + word(l),
			"htfSum": htfSum, "ltfSum": ltfSum, "tfCount": len(tfs),
		}
	}

	dom := sign(htfSum + ltfSum)
	if dom == 0 {
		return gin.H{
			"state": "mixed", "label": "TF 混合/中性",
			"htfSum": htfSum, "ltfSum": ltfSum, "tfCount": len(tfs),
		}
	}

	agree, against, htfAgree := 0, 0, 0
	for tf, s := range signs {
		switch s {
		case dom:
			agree++
			if htf[tf] {
				htfAgree++
			}
		case -dom:
			against++
		}
	}

	// Breadth test. Three conditions, and the last two were added on
	// 2026-09-11 after the strip printed "✓ TF 一致偏多 (2/5·反1)" — a label
	// whose own parenthetical contradicted it.
	//
	// A contested minority is not 一致. The shipped rule accepted agree>=2
	// with an HTF on board, which printed "✓ TF 一致偏多 (2/5·反1)" — a label
	// its own parenthetical contradicted.
	//
	// A blunt majority-of-all test was the first fix and it was too blunt: it
	// also downgraded {2h +2, 4h +1, LTFs all neutral}, where both higher
	// timeframes agree and NOTHING opposes them. That is a real alignment;
	// neutral is not opposition. What the defect actually was is calling a
	// CONTESTED minority aligned.
	//
	// So: a genuine majority of the strip, or an unopposed HTF-led lean.
	broad := agree >= 3 || (agree >= 2 && against == 0 && htfAgree >= 1)
	// 1h HOLDS A VETO. Every shipped edge is validated on 1h and nothing
	// else: isStructureTF() returns true only for "1h", the daemon runs 1h,
	// the sweep-reject A/B is 1h. On the 2026-09-11 case the dissenter WAS
	// 1h and it was outvoted by 15m and 4h — 15m being the timeframe the TF
	// decisions call poison. A consensus that the only validated lens
	// disagrees with is not a consensus worth acting on.
	oneHourDissents := signs["1h"] != 0 && signs["1h"] == -dom
	strong := broad && agree > against && !oneHourDissents

	breadth := fmt.Sprintf(" (%d/%d)", agree, len(tfs))
	if against > 0 {
		breadth = fmt.Sprintf(" (%d/%d·反%d)", agree, len(tfs), against)
	}
	// Say WHY it was downgraded, or the operator reads "弱共識" and cannot
	// tell whether breadth was thin or the 1h lens objected.
	if oneHourDissents {
		breadth += "·1h 反對"
	}

	state, label := "weak-", "⚠ TF 弱共識偏"
	if strong {
		state, label = "aligned-", "✓ TF 一致偏"
	}
	if dom > 0 {
		state += "long"
	} else {
		state += "short"
	}

	return gin.H{
		"state": state, "label": label + word(dom) + breadth,
		"agree": agree, "against": against, "htfAgree": htfAgree,
		"htfSum": htfSum, "ltfSum": ltfSum, "tfCount": len(tfs),
	}
}

// computeRange detects the recent trading box (last ~24×1h swing hi/lo) and
// where price sits in it. In a NEUTRAL/chop regime (alignment mixed/conflict)
// the right play is mean-reversion at the EDGES — buy the bottom third, short
// the top third, DON'T trade the middle (equidistant = worst R:R, whipsawed
// both ways). This is the "range mode" complement to trend-retrace zone entries.
func computeRange(candles []market.Candle, price float64, alignState string) gin.H {
	if len(candles) < 30 || price <= 0 {
		return nil
	}
	n := 24
	if len(candles) < n {
		n = len(candles)
	}
	seg := candles[len(candles)-n:]
	lo, hi := seg[0].Low, seg[0].High
	for _, c := range seg {
		if c.Low < lo {
			lo = c.Low
		}
		if c.High > hi {
			hi = c.High
		}
	}
	if hi <= lo {
		return nil
	}
	pos := (price - lo) / (hi - lo) // 0 = floor, 1 = ceiling
	// No usable trend = range mode. "weak-*" counts: a thin one-TF lean is
	// not a trend, and treating it as one used to suppress this read-aid
	// exactly when chop made it most useful.
	isRange := alignState == "mixed" || alignState == "conflict" ||
		strings.HasPrefix(alignState, "weak-")
	zone := "middle"
	switch {
	case pos <= 0.34:
		zone = "bottom"
	case pos >= 0.66:
		zone = "top"
	}
	action := ""
	if isRange {
		switch zone {
		case "bottom":
			action = "邊緣做多 (止損箱外)"
		case "top":
			action = "邊緣做空 (止損箱外)"
		default:
			action = "中間別做"
		}
	} else {
		action = "趨勢中 (等回踩,別 fade 邊緣)"
	}
	return gin.H{
		"lo": lo, "hi": hi, "pos": int(pos*100 + 0.5),
		"widthPct": (hi - lo) / lo * 100, "zone": zone,
		"isRange": isRange, "action": action,
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

const tickerCacheTTL = 3 * time.Second

// symbolCategory buckets a short ticker for the market-overview table tabs.
func symbolCategory(short string) string {
	switch short {
	case "XAU", "XAG":
		return "metal"
	case "SNDK", "NVDA", "SPCX", "MSTR", "APP":
		return "stock"
	default:
		return "crypto"
	}
}

// handleTickers — GET /api/tickers
//
// Kept as the polling fallback for clients without EventSource, and as the
// data source the SSE hub broadcasts (see stream.go). The build itself lives
// in buildTickers so the two transports cannot serve different numbers.
func (s *server) handleTickers(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	c.JSON(http.StatusOK, s.tickerSnapshot(ctx))
}

// tickerSnapshot returns the cached snapshot when it is still fresh, else
// rebuilds it. The cache is what keeps N pollers from becoming N fan-outs.
func (s *server) tickerSnapshot(ctx context.Context) gin.H {
	tickerCacheMu.Lock()
	if tickerCache.resp != nil && time.Since(tickerCache.at) < tickerCacheTTL {
		resp := tickerCache.resp
		tickerCacheMu.Unlock()
		return resp
	}
	tickerCacheMu.Unlock()
	return s.buildTickers(ctx)
}

// buildTickers fans out across the symbol set and assembles one snapshot,
// then stores it in the shared cache. Always does the work — callers that
// want the cache should go through tickerSnapshot.
func (s *server) buildTickers(ctx context.Context) gin.H {
	// Fan out across the (now larger) symbol set so the strip stays snappy —
	// each symbol is 2 API calls (mark + klines); sequential over 11 symbols
	// would crawl. Output order is preserved by index.
	out := make([]gin.H, len(tickerSymbols))
	var wg sync.WaitGroup
	for i, short := range tickerSymbols {
		wg.Add(1)
		go func(idx int, short string) {
			defer wg.Done()
			row := gin.H{"symbol": short, "cat": symbolCategory(short)}
			sym, err := resolveWebSymbol(short)
			if err == nil && s.client != nil {
				// Price = live mark. The chart header now reads the same mark
				// (it used to prefer the forming kline's close, which lags and
				// sticks — measured 8-14 pts behind on BTC 2026-09-03, which is
				// what made the strip and the header disagree). Klines are only
				// for the 24h reference + range. Funding is free (same
				// FundingRate call) — surfaced for the market-overview table.
				price := 0.0
				if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil && fr.MarkPrice > 0 {
					price = fr.MarkPrice
					row["funding"] = fr.Rate * 100 // as %
				}
				// 26 hourly bars ≈ 25h: enough to look back a full 24h.
				if ks, kerr := s.client.KlinesWithForming(ctx, sym, market.Timeframe("1h"), 26); kerr == nil && len(ks) > 0 {
					if price == 0 {
						price = ks[len(ks)-1].Close // mark fetch failed — fall back to last close
					}
					// Reference = the OPEN of the bar 24h back, not its close.
					// On a 1h series that bar's open IS the price 24h ago,
					// while its close is the price 23h ago — and the header
					// tile computes its change % from that same open, so
					// using Close here made the two show different percentages
					// next to each other.
					ref := ks[0].Open
					if j := len(ks) - 1 - 24; j >= 0 {
						ref = ks[j].Open
					}
					if ref > 0 && price > 0 {
						row["changePct"] = (price - ref) / ref * 100
					}
					// 24h high/low over the looked-back window.
					hi, lo := ks[0].High, ks[0].Low
					start := len(ks) - 25
					if start < 0 {
						start = 0
					}
					for _, k := range ks[start:] {
						if k.High > hi {
							hi = k.High
						}
						if k.Low < lo {
							lo = k.Low
						}
					}
					row["high24"], row["low24"] = hi, lo
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
	return resp
}
