// Command monitor is a secondary daemon that watches multiple TFs for
// CONFLUENCE — events where two or more timeframes simultaneously agree
// on the same direction with score ≥ threshold. Designed to complement
// (not replace) the primary trading-bot daemon, which alerts on single-TF
// signals at 1h.
//
// Cadence: aligned to UTC-minute boundaries. At each minute we determine
// which monitored TFs (30m, 1h, 2h, 4h — 15m intentionally skipped per
// backtest evidence that it's net-negative on every symbol) have just
// closed a fresh bar, then scan all symbols on those TFs in parallel.
// Results are grouped by (symbol, side); the group is emitted as a
// single alert when ≥ min-tfs of them clear min-score.
//
// Dedup: (symbol, side, sorted-TF-set, latest-candle-CloseTime). A new
// TF joining the agreement set produces a fresh alert because that's
// informative. Same TF set on the same candle does not re-alert.
//
// Notification: synthesized signal.Signal with Timeframe="multi-TF" so
// formatAlert renders cleanly through the existing Stdout / ntfy / Mac
// sinks. The canonical plan is taken from the highest-TF contributor.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/earnings"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/notify"
	sig "myFirstGo/trading-bot/signal"
	"myFirstGo/trading-bot/validator"
)

func main() {
	config.LoadDotEnv()
	minScore := flag.Int("min-score", 3, "per-TF engine score threshold; contributors below this don't count toward confluence")
	minTFs := flag.Int("min-tfs", 2, "minimum number of agreeing TFs to fire an alert. min=1 makes it a multi-TF single-alerter; min=2+ enforces confluence.")
	minRatio := flag.Float64("min-ratio", 0, "per-TF validator /10 threshold (structural fit). 0 = disabled. e.g. 6 means each contributing TF also needs validator.Total >= 6.0. Filters in addition to --min-score, not instead of.")
	fetchDelay := flag.Duration("fetch-delay", 10*time.Second, "wait after minute boundary before fetching klines (gives BingX time to publish the just-closed bar)")
	includeTFs := flag.String("tfs", "30m,1h,2h,4h", "comma-separated TFs to monitor. 15m is intentionally excluded by default — backtest shows net-negative on every symbol regardless of threshold.")
	noMac := flag.Bool("no-mac", true, "disable macOS Notification Center; default off because this is a server-side daemon")
	flag.Parse()

	// Apply env-var tweaks shared with the primary daemon.
	if v := os.Getenv("VOL_PROFILE_BODY_WEIGHT"); v != "" {
		var bw float64
		if _, err := fmt.Sscanf(v, "%f", &bw); err == nil && bw > 0 && bw < 1 {
			indicator.BodyWeight = bw
			log.Printf("volume profile body-weighting enabled: %.2f", bw)
		}
	}

	// Env-var overrides for monitor-specific config so the Ops page
	// can persist changes by editing /opt/trading/.env. Env wins over
	// CLI flag if both are set — same convention as the primary daemon.
	if v := os.Getenv("MONITOR_TFS"); strings.TrimSpace(v) != "" {
		*includeTFs = v
		log.Printf("MONITOR_TFS env override: %s", v)
	}
	if v := os.Getenv("MONITOR_MIN_SCORE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8 {
			*minScore = n
			log.Printf("MONITOR_MIN_SCORE env override: %d", n)
		}
	}
	if v := os.Getenv("MONITOR_MIN_TFS"); v != "" {
		// Floor lowered 2026-06-17 from 2 → 1 so user can use the monitor
		// as a higher-quality single-TF alerter (gated by ratio/score).
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 5 {
			*minTFs = n
			log.Printf("MONITOR_MIN_TFS env override: %d", n)
		}
	}
	if v := os.Getenv("MONITOR_MIN_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 10 {
			*minRatio = f
			if f > 0 {
				log.Printf("MONITOR_MIN_RATIO env override: %.1f", f)
			}
		}
	}

	monitoredTFs, err := parseTFList(*includeTFs)
	if err != nil {
		log.Fatalf("invalid --tfs: %v", err)
	}
	if *minTFs < 1 {
		*minTFs = 1
	}
	if len(monitoredTFs) < *minTFs {
		log.Fatalf("--min-tfs=%d but only %d TFs monitored — would never fire", *minTFs, len(monitoredTFs))
	}

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	notifier := notify.Multi{Sinks: []notify.Notifier{notify.Stdout{}}}
	if !*noMac {
		notifier.Sinks = append(notifier.Sinks, notify.Mac{})
	}
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		notifier.Sinks = append(notifier.Sinks, notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic))
		log.Printf("ntfy push configured for topic %q", topic)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForShutdown(cancel)

	// Earnings-blackout calendar (F3). signal.Evaluate reads earnings.Default()
	// to gate stock symbols around a report; without this load that gate is a
	// SILENT no-op here — autoInBlackout dutifully checks for an "earnings"
	// reason that the engine could never produce, because this process's
	// calendar was empty. Only the web binary loaded it before, so the gate
	// looked armed while the auto-executor ran unprotected. A missing file is
	// still fine (empty calendar = no-op by construction, not by omission).
	earnings.LoadDefaultAndWatch(ctx, earnings.Path(), 10*time.Minute, log.Printf)
	// Ad-hoc blackouts (a Fed speech announced days ahead) without a rebuild.
	// 1 minute, not 10 like earnings: an earnings date is known weeks out, but
	// the overlay exists precisely for something you are adding minutes before
	// it matters.
	macro.LoadOverlayAndWatch(ctx, macro.OverlayPath(), time.Minute, log.Printf)

	// Zone-alert channel: fast live-price watcher for pivot-zone-fade
	// entries (separate cadence from the confluence scan). No-op if
	// NTFY_TOPIC is unset. Zones live in /opt/trading/zones.json.
	go runZoneAlerts(ctx, client)
	go runAutoExecutor(ctx, client) // auto-order daemon (paper by default; triple-gated for live)

	// macrowarn starts BEFORE the zone-only branch on purpose. It used to sit
	// below it, so MONITOR_ZONE_ONLY=1 silently took the NFP/FOMC pre-blackout
	// heads-up down with the confluence noise — two unrelated concerns on one
	// switch. It fires at most once per calendar event (~monthly), which is
	// not "noise" under any reading of that flag, and its absence is invisible
	// until the morning an event arrives unannounced. Self-gating on
	// NTFY_TOPIC, so starting it unconditionally costs nothing when push is
	// muted.
	go runMacroWarn(ctx)

	// Bracket guard: watches every OPEN journal trade against the exchange
	// and reports (or, opted in, fixes) a live position with no reduce-only
	// stop resting on it. Starts here, above the zone-only branch, for the
	// same reason macrowarn does — MONITOR_ZONE_ONLY exists to silence
	// confluence noise, and a naked-position alarm is not that. See
	// cmd/monitor/bracket.go for why this cannot live in cmd/web.
	go runBracketGuard(ctx, client)

	// OI sampler: the producer for signal.Context.PrevOpenInterest, which had
	// no producer at all until now, so the engine's two OI crowding warnings
	// were unreachable code. Above the zone-only branch for the same reason
	// as the two guards — MONITOR_ZONE_ONLY exists to silence confluence
	// noise, and this writes a data file rather than pushing anything. It is
	// also the only process that CAN sample: cmd/analyze and the web handlers
	// are too short-lived to remember an hour ago.
	go runOISampler(ctx, client)

	// BLS actuals refresher. Above the zone-only branch like the other
	// data-only loops, and the ONLY caller of bls.Fetch: the keyless v1 API
	// allows 25 queries a day, so cmd/web reads the cache this writes and
	// never fetches from a page load.
	go runBLSRefresh(ctx)

	// Binance positioning cross-reference. Unlike the oi/ sampler this does
	// not accumulate — Binance publishes the OI series itself, so the loop
	// only refreshes an expiring cache.
	go runBinFutRefresh(ctx)

	// MONITOR_ZONE_ONLY=1 keeps the zone-fade + breakout-tripwire channel
	// (zonealert), autoexec and macrowarn, and skips the multi-TF confluence
	// scan / structalert — for when the user wants the zone alerts without the
	// confluence noise. Block on ctx so those goroutines keep polling; never
	// reach the confluence loop below.
	if os.Getenv("MONITOR_ZONE_ONLY") == "1" {
		log.Printf("MONITOR_ZONE_ONLY=1 — zonealert + autoexec + macrowarn + bracket only (confluence / structalert disabled)")
		<-ctx.Done()
		log.Printf("monitor shutting down")
		return
	}

	go runStructureAlerts(ctx, client)

	dedup := newDedupSet()
	ratioMsg := "off"
	if *minRatio > 0 {
		ratioMsg = fmt.Sprintf("%.1f", *minRatio)
	}
	log.Printf("multi-TF monitor up — TFs=%v min-score=%d min-tfs=%d min-ratio=%s", tfList(monitoredTFs), *minScore, *minTFs, ratioMsg)

	for {
		// Sleep to the next UTC minute boundary.
		now := time.Now().UTC()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			log.Printf("monitor shutting down")
			return
		case <-time.After(time.Until(next)):
		}

		closed := tfsThatJustClosed(time.Now().UTC(), monitoredTFs)
		if len(closed) == 0 {
			continue
		}
		// Wait for BingX to publish the just-closed bar.
		select {
		case <-ctx.Done():
			return
		case <-time.After(*fetchDelay):
		}

		log.Printf("tick %s — scanning TFs %v", time.Now().UTC().Format("15:04:05Z"), tfList(closed))
		scanTick(ctx, client, closed, *minScore, *minTFs, *minRatio, notifier, dedup)
	}
}

// ─── Scheduling ────────────────────────────────────────────────────

// tfsThatJustClosed returns the subset of monitored TFs whose bar
// closed at the start of t's minute. Uses UTC-aligned boundaries:
//   - 30m: minute 0 or 30
//   - 1h:  minute 0
//   - 2h:  minute 0, hour even
//   - 4h:  minute 0, hour divisible by 4
//   - 15m: not in default list, but supported if user adds it
func tfsThatJustClosed(t time.Time, monitored []market.Timeframe) []market.Timeframe {
	out := make([]market.Timeframe, 0, len(monitored))
	min, hr := t.Minute(), t.Hour()
	for _, tf := range monitored {
		switch tf {
		case market.TF5m:
			if min%5 == 0 {
				out = append(out, tf)
			}
		case market.TF15m:
			if min%15 == 0 {
				out = append(out, tf)
			}
		case market.TF30m:
			if min%30 == 0 {
				out = append(out, tf)
			}
		case market.TF1h:
			if min == 0 {
				out = append(out, tf)
			}
		case market.TF2h:
			if min == 0 && hr%2 == 0 {
				out = append(out, tf)
			}
		case market.TF4h:
			if min == 0 && hr%4 == 0 {
				out = append(out, tf)
			}
		}
	}
	return out
}

func parseTFList(s string) ([]market.Timeframe, error) {
	parts := strings.Split(s, ",")
	out := make([]market.Timeframe, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		tf := market.Timeframe(p)
		switch tf {
		case market.TF5m, market.TF15m, market.TF30m, market.TF1h, market.TF2h, market.TF4h:
			out = append(out, tf)
		default:
			return nil, fmt.Errorf("unsupported TF %q (allowed: 5m,15m,30m,1h,2h,4h)", p)
		}
	}
	return out, nil
}

func tfList(tfs []market.Timeframe) []string {
	out := make([]string, len(tfs))
	for i, t := range tfs {
		out[i] = string(t)
	}
	return out
}

// tfRank lets us sort multi-TF hits by horizon (highest first) so the
// canonical plan attached to the alert comes from the longest TF.
func tfRank(tf market.Timeframe) int {
	switch tf {
	case market.TF5m:
		return 0
	case market.TF15m:
		return 1
	case market.TF30m:
		return 2
	case market.TF1h:
		return 3
	case market.TF2h:
		return 4
	case market.TF4h:
		return 5
	}
	return 0
}

// ─── Scanning ──────────────────────────────────────────────────────

type tfHit struct {
	TF        market.Timeframe
	Sig       sig.Signal
	Candle    time.Time // last closed bar's CloseTime
	Validator float64   // validator.Result.Total at signal moment; 0 if minRatio gating disabled
}

// scanTick evaluates every (symbol, tf) in parallel, groups hits by
// (symbol, side), and emits confluence alerts that meet min-tfs +
// min-score thresholds. Skips groups dedup has already marked.
func scanTick(ctx context.Context, client *bingx.Client, tfs []market.Timeframe, minScore, minTFs int, minRatio float64, n notify.Notifier, d *dedupSet) {
	type job struct {
		sym market.Symbol
		tf  market.Timeframe
	}
	var jobs []job
	for _, sym := range market.All() {
		for _, tf := range tfs {
			jobs = append(jobs, job{sym, tf})
		}
	}

	results := make([]tfHit, len(jobs))
	hasResult := make([]bool, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			h, ok := evalOne(ctx, client, j.sym, j.tf, minScore, minRatio)
			results[i] = h
			hasResult[i] = ok
		}(i, j)
	}
	wg.Wait()

	// Group by (symbol, side).
	type groupKey struct {
		Sym  market.Symbol
		Side sig.Side
	}
	groups := map[groupKey][]tfHit{}
	for i, ok := range hasResult {
		if !ok {
			continue
		}
		h := results[i]
		k := groupKey{Sym: h.Sig.Symbol, Side: h.Sig.Side}
		groups[k] = append(groups[k], h)
	}

	for k, hits := range groups {
		if len(hits) < minTFs {
			continue
		}
		// Sort TFs by horizon descending; canonical plan comes from highest.
		sort.Slice(hits, func(i, j int) bool { return tfRank(hits[i].TF) > tfRank(hits[j].TF) })

		// Dedup key uses sorted TF set + latest candle time among contributors.
		var tfStrs []string
		var latest time.Time
		for _, h := range hits {
			tfStrs = append(tfStrs, string(h.TF))
			if h.Candle.After(latest) {
				latest = h.Candle
			}
		}
		sort.Strings(tfStrs)
		tfSetKey := strings.Join(tfStrs, "+")
		if !d.markIfNew(string(k.Sym), k.Side.String(), tfSetKey, latest.UnixMilli()) {
			log.Printf("dedup: %s %s [%s] @ %s — skipped", k.Sym, k.Side, tfSetKey, latest.Format("2006-01-02T15:04Z"))
			continue
		}

		emitConfluence(ctx, k.Sym, k.Side, hits, tfSetKey, n)
	}
}

// evalOne fetches candles + context for one (symbol, tf) and runs the
// engine. Returns a tfHit if the signal would emit at minScore AND the
// validator.Total >= minRatio (when minRatio > 0). Otherwise ok=false.
// Validator is short-circuited when minRatio=0 (no extra work).
func evalOne(ctx context.Context, client *bingx.Client, sym market.Symbol, tf market.Timeframe, minScore int, minRatio float64) (tfHit, bool) {
	candles, err := client.Klines(ctx, sym, tf, 300)
	if err != nil {
		log.Printf("%s %s: klines: %v", sym, tf, err)
		return tfHit{}, false
	}
	if len(candles) < 200 {
		return tfHit{}, false
	}
	sigCtx := sig.Context{}
	var markPrice float64
	if fr, err := client.FundingRate(ctx, sym); err == nil {
		sigCtx.FundingRate = fr.Rate
		markPrice = fr.MarkPrice
	}
	if oi, err := client.OpenInterest(ctx, sym); err == nil {
		sigCtx.OpenInterest = oi
	}
	s := sig.Evaluate(sig.Inputs{
		Symbol: sym, Timeframe: tf, Candles: candles,
		Ctx: sigCtx, LiveMarkPrice: markPrice,
	})
	if s.Side == sig.Flat || s.Score < minScore || s.Plan.Entry == 0 {
		return tfHit{}, false
	}
	// Optional /10 validator gate. When minRatio > 0, the candidate must
	// also clear the validator's structural-fit score. The signal_ctx
	// snapshot we capture at +record time uses the same Total — so this
	// effectively pre-filters alerts to those that would already get a
	// TAKE-or-better verdict from the dashboard's live diagnose.
	var ratio float64
	if minRatio > 0 {
		r := validator.Validate(sym, tf, s.Side, s.Plan.Entry, 6.0, candles, markPrice)
		ratio = r.Total
		if ratio < minRatio {
			return tfHit{}, false
		}
	}
	return tfHit{TF: tf, Sig: s, Candle: candles[len(candles)-1].CloseTime, Validator: ratio}, true
}

// ─── Alert emission ────────────────────────────────────────────────

// emitConfluence synthesizes a signal.Signal whose Reasons list each
// contributing TF + score + anchor, then routes it through the standard
// notify pipeline. Plan is taken from the highest-TF contributor (hits
// already sorted descending by tfRank in scanTick).
func emitConfluence(ctx context.Context, sym market.Symbol, side sig.Side, hits []tfHit, tfSetKey string, n notify.Notifier) {
	totalScore := 0
	reasons := make([]string, 0, len(hits))
	for _, h := range hits {
		totalScore += h.Sig.Score
		anchor := h.Sig.Plan.Anchor
		if anchor == "" {
			anchor = "-"
		}
		ratioStr := ""
		if h.Validator > 0 {
			ratioStr = fmt.Sprintf(" /10=%.1f", h.Validator)
		}
		reasons = append(reasons, fmt.Sprintf("%s score=%d%s anchor=%s", h.TF, h.Sig.Score, ratioStr, anchor))
	}
	canonical := hits[0].Sig
	synthSig := sig.Signal{
		Symbol:    sym,
		Timeframe: market.Timeframe("multi-TF:" + tfSetKey),
		Side:      side,
		Score:     totalScore,
		Price:     canonical.Price,
		Reasons:   reasons,
		Plan:      canonical.Plan,
	}
	if err := n.Notify(ctx, synthSig, sig.Context{}); err != nil {
		log.Printf("notify: %v", err)
		return
	}
	log.Printf("ALERT %s %s [%s] totalScore=%d canonical=%s anchor=%q",
		sym, side, tfSetKey, totalScore, hits[0].TF, hits[0].Sig.Plan.Anchor)
}

// ─── Dedup ────────────────────────────────────────────────────────

type dedupSet struct {
	mu   sync.Mutex
	seen map[string]int64 // key → candle ts millis
}

func newDedupSet() *dedupSet { return &dedupSet{seen: map[string]int64{}} }

func (d *dedupSet) markIfNew(sym, side, tfSet string, candleMs int64) bool {
	key := sym + "|" + side + "|" + tfSet
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.seen[key]; ok && prev == candleMs {
		return false
	}
	d.seen[key] = candleMs
	// Soft GC: when the map gets large, drop entries older than 7 days.
	if len(d.seen) > 500 {
		cutoff := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
		for k, ts := range d.seen {
			if ts < cutoff {
				delete(d.seen, k)
			}
		}
	}
	return true
}

func waitForShutdown(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	cancel()
}
