package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/notify"
	"myFirstGo/trading-bot/signal"
)

// autoSymbols maps the friendly ticker in autotrade.json to the BingX symbol.
var autoSymbols = map[string]market.Symbol{
	"BTC": market.BTCUSDT, "ETH": market.ETHUSDT, "XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
	"SNDK": market.SNDKUSDT, "NVDA": market.NVDAUSDT,
	"SOL": market.SOLUSDT, "LINK": market.LINKUSDT, "SUI": market.SUIUSDT, "NEAR": market.NEARUSDT, "HYPE": market.HYPEUSDT,
}

type autoTrigger struct {
	fire   bool
	side   string
	entry  float64
	stop   float64
	tp     float64
	market bool // true = marketable at fire (fills immediately); false = resting limit
	why    string
}

type autoState struct {
	lastFireBar time.Time // dedup: one placement per rule per bar
}

// runAutoExecutor is the deterministic auto-order daemon (docs/auto_executor_design.md).
// PHASE 1: it evaluates triggers + all safety guards and, in PAPER mode (the default
// — needs BOTH autotrade.json enabled+paper=false AND env AUTOTRADE_ENABLED=true to go
// live), only LOGS + ntfy-pushes the intended order (no API call). Live placement is
// wired but triple-gated. Position P&L tracking / real fills = Phase 2.
func runAutoExecutor(ctx context.Context, client *bingx.Client) {
	cfg := autotrade.Load()
	log.Printf("autoexec: up — enabled=%v paper=%v live-armed=%v rules=%d (config=%s)",
		cfg.Enabled, cfg.Paper, cfg.LiveArmed(), len(cfg.Rules), autotrade.Path())

	var n *notify.Ntfy
	if topic := os.Getenv("NTFY_TOPIC"); topic != "" {
		n = notify.NewNtfy(os.Getenv("NTFY_SERVER"), topic)
	}
	state := map[string]*autoState{}

	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		cfg := autotrade.Load() // hot-reload
		if !cfg.Enabled {
			continue
		}
		live := cfg.LiveArmed()

		for i := range cfg.Rules {
			r := cfg.Rules[i]
			if !r.Enabled {
				continue
			}
			sym, ok := autoSymbols[r.Symbol]
			if !ok {
				continue
			}
			key := r.Symbol + "|" + r.TF + "|" + r.Strategy

			// --- trigger ---
			trig := evalAutoTrigger(ctx, client, sym, r)
			if !trig.fire {
				continue
			}

			// --- guards ---
			// dedup: one placement per rule per closed bar
			cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 3)
			if err != nil || len(cs) == 0 {
				continue
			}
			barTime := cs[len(cs)-1].CloseTime
			st := state[key]
			if st == nil {
				st = &autoState{}
				state[key] = st
			}
			if st.lastFireBar.Equal(barTime) {
				continue // already acted this bar
			}
			// blackout gate (macro + earnings via engine's own gate on a Flat probe)
			if autoInBlackout(sym, barTime) {
				continue
			}
			// one-position-per-rule: live checks the exchange; paper replays the
			// prior fire so a persistent setup doesn't re-enter every bar. Also
			// enforces the post-stop cooldown.
			if live {
				if pos, perr := client.OpenPositions(ctx, sym); perr == nil && len(pos) > 0 {
					continue
				}
			} else if paperBlocked(ctx, client, sym, r, barTime) {
				continue
			}

			qty := r.MarginUSDT * float64(r.Leverage) / trig.entry
			st.lastFireBar = barTime

			if !live {
				// PAPER: log + ntfy the intended order, no API call.
				msg := fmt.Sprintf("%s %s @%.4f stop %.4f tp %.4f (qty %.4f, %du×%d) — %s",
					r.Symbol, trig.side, trig.entry, trig.stop, trig.tp, qty, int(r.MarginUSDT), r.Leverage, trig.why)
				log.Printf("[AUTO-PAPER] would place %s", msg)
				autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, Side: trig.side, Market: trig.market,
					Entry: trig.entry, Stop: trig.stop, TP: trig.tp, Qty: qty, Margin: r.MarginUSDT, Lev: r.Leverage, Why: trig.why, Live: false})
				if n != nil {
					_ = n.Push(ctx, "🤖 auto (paper)", msg, "robot")
				}
				continue
			}

			// LIVE (triple-gated). Set leverage then place the bracket.
			if err := client.SetLeverage(ctx, sym, "BOTH", r.Leverage); err != nil {
				log.Printf("autoexec: SetLeverage %s: %v", r.Symbol, err)
			}
			res, perr := client.PlaceLimit(ctx, sym, trig.side, qty, trig.entry, trig.stop, trig.tp, false)
			if perr != nil {
				log.Printf("autoexec: PlaceLimit %s FAILED: %v", r.Symbol, perr)
				if n != nil {
					_ = n.Push(ctx, "⚠ auto place FAILED", r.Symbol+": "+perr.Error(), "warning")
				}
				continue
			}
			id := ""
			if res != nil {
				id = res.OrderID
			}
			log.Printf("[AUTO-LIVE] placed %s %s @%.4f stop %.4f tp %.4f qty %.4f orderId=%s",
				r.Symbol, trig.side, trig.entry, trig.stop, trig.tp, qty, id)
			autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, Side: trig.side, Market: trig.market,
				Entry: trig.entry, Stop: trig.stop, TP: trig.tp, Qty: qty, Margin: r.MarginUSDT, Lev: r.Leverage, Why: trig.why + " id=" + id, Live: true})
			if n != nil {
				_ = n.Push(ctx, "🤖 auto PLACED", fmt.Sprintf("%s %s @%.4f (stop %.4f tp %.4f) id=%s", r.Symbol, trig.side, trig.entry, trig.stop, trig.tp, id), "robot")
			}
		}
	}
}

// paperBlocked reports whether a PAPER fire should be suppressed because the rule
// already holds a live position (the prior fire is still open/pending) or is inside
// its post-stop cooldown. This makes paper behave like one-position-per-rule instead
// of re-entering the same persistent setup on every bar close.
func paperBlocked(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule, barTime time.Time) bool {
	var latest *autotrade.PaperFire
	for _, f := range autotrade.ReadFires(300) { // newest-first
		if f.Symbol == r.Symbol && f.Strategy == r.Strategy {
			ff := f
			latest = &ff
			break
		}
	}
	if latest == nil {
		return false
	}
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 300)
	if err != nil {
		return false // can't determine — don't block
	}
	oc := autotrade.EvaluateFire(*latest, cs, 6)
	switch oc.Status {
	case autotrade.OutOpen, autotrade.OutPending:
		return true // still in a position / waiting to fill
	case autotrade.OutStop:
		return barTime.Before(oc.ExitAt.Add(time.Duration(r.CooldownBars) * barDurationTF(r.TF)))
	default:
		return false // tp / no-fill → free to re-enter
	}
}

// barDurationTF maps a timeframe string to its bar duration for cooldown math.
func barDurationTF(tf string) time.Duration {
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
	case "1d":
		return 24 * time.Hour
	default:
		return time.Hour
	}
}

// evalAutoTrigger dispatches on strategy. "range-edge" = the box mean-reversion
// trigger; "engine" runs the FULL confluence engine (signal.Evaluate → per-symbol
// strategyFor, so e.g. HYPE/SOL 1h fire StructMomentum, not a box fade) and places
// the engine's own structure-based Plan bracket.
func evalAutoTrigger(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	switch r.Strategy {
	case "range-edge":
		return evalRangeEdge(ctx, client, sym, r)
	case "engine":
		return evalEngine(ctx, client, sym, r)
	case "sweep-reject":
		return evalSweepReject(ctx, client, sym, r)
	case "htf-snr":
		return evalHTFSNR(ctx, client, sym, r)
	default:
		return autoTrigger{}
	}
}

// evalHTFSNR fades a HIGHER-timeframe support/resistance level on the last
// CLOSED entry-TF bar — the mechanized "2553 short" the group draws by hand.
// Levels = swing highs/lows from the HTF (engineBiasTF, e.g. 1h→4h, strength 3,
// confirmed `strength` bars after they form). Fire when the last closed bar tags
// a confirmed level (High ≥ resistance−tol, approached from below) and closes
// back below it → short; mirror for HTF support → long. Stop = tag wick + ATR
// buffer; TP 2R. A/B-validated on ETH only (cmd/snrbt, +18/+18/+11R across
// 60/90/120d, WR 35-39%); other symbols rejected, so keep this rule ETH-scoped.
func evalHTFSNR(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	const (
		strength = 3      // HTF swing strength
		tolFrac  = 0.0020 // 0.20% tag tolerance beyond the level
		bufATR   = 0.25   // stop buffer beyond the tag wick, in ATR(14)
		rMult    = 2.0    // TP = 2R
	)
	tf := market.Timeframe(r.TF)
	cs, err := client.Klines(ctx, sym, tf, 300)
	if err != nil || len(cs) < 30 {
		return autoTrigger{}
	}
	hcs, herr := client.Klines(ctx, sym, engineBiasTF(tf), 200)
	if herr != nil || len(hcs) < 40 {
		return autoTrigger{}
	}
	atrs := indicator.ATR(cs, 14)
	if len(atrs) == 0 {
		return autoTrigger{}
	}
	a := atrs[len(atrs)-1]
	bar := cs[len(cs)-1]   // last CLOSED entry bar = the fade candidate
	prev := cs[len(cs)-2]  // the bar before it (approach direction)

	// HTF swing levels, each usable only once confirmed (strength bars later).
	sw := signal.FindSwingPoints(hcs, strength, 0)
	wantShort := r.Side == "short" || r.Side == "auto"
	wantLong := r.Side == "long" || r.Side == "auto"
	for _, p := range sw {
		ci := p.Index + strength
		if ci >= len(hcs) {
			ci = len(hcs) - 1
		}
		if !hcs[ci].CloseTime.Before(bar.OpenTime) {
			continue // level not yet confirmed at this bar
		}
		tolAbs := p.Price * tolFrac
		if wantShort && p.IsTop && prev.Close < p.Price && bar.High >= p.Price-tolAbs && bar.Close < p.Price {
			stop := bar.High + bufATR*a
			risk := stop - bar.Close
			if risk <= 0 {
				continue
			}
			return autoTrigger{fire: true, side: "short", entry: bar.Close, market: true,
				stop: stop, tp: bar.Close - rMult*risk,
				why: fmt.Sprintf("HTF-S/R fade: reject %s swing high %.4f", engineBiasTF(tf), p.Price)}
		}
		if wantLong && !p.IsTop && prev.Close > p.Price && bar.Low <= p.Price+tolAbs && bar.Close > p.Price {
			stop := bar.Low - bufATR*a
			risk := bar.Close - stop
			if risk <= 0 {
				continue
			}
			return autoTrigger{fire: true, side: "long", entry: bar.Close, market: true,
				stop: stop, tp: bar.Close + rMult*risk,
				why: fmt.Sprintf("HTF-S/R fade: hold %s swing low %.4f", engineBiasTF(tf), p.Price)}
		}
	}
	return autoTrigger{}
}

// evalSweepReject fires the SMC sweep-and-reject on the last CLOSED bar: price ran
// beyond an EQH/EQL liquidity pool then closed back inside (failed breakout) →
// fade it. Entry at that close, stop just BEYOND the sweep wick (🪝 off the magnet),
// TP a fixed R multiple. A/B-validated on SOL/ETH (cmd/sweepbt, +R across 60/90/120d
// and robust to R∈[1.5,3] / tol∈[0.10,0.20]); params fixed at the sweet spot.
func evalSweepReject(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	const (
		tolFrac = 0.0015 // 0.15% EQH/EQL cluster tolerance
		bufATR  = 0.15   // stop buffer beyond the sweep wick, in ATR(14)
		rMult   = 2.0    // TP = 2R
	)
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 300)
	if err != nil || len(cs) < 80 {
		return autoTrigger{}
	}
	atrs := indicator.ATR(cs, 14)
	if len(atrs) == 0 {
		return autoTrigger{}
	}
	a := atrs[len(atrs)-1]
	bar := cs[len(cs)-1]                                   // last CLOSED bar = the sweep-reject candidate
	pools := signal.FindLiquidity(cs[:len(cs)-1], 2, 20, tolFrac) // pools formed BEFORE it

	wantShort := r.Side == "short" || r.Side == "auto"
	wantLong := r.Side == "long" || r.Side == "auto"
	for _, p := range pools {
		if wantShort && p.Kind == signal.EQH && bar.High > p.Hi && bar.Close < p.Lo {
			stop := bar.High + bufATR*a
			risk := stop - bar.Close
			if risk <= 0 {
				continue
			}
			return autoTrigger{fire: true, side: "short", entry: bar.Close, market: true,
				stop: stop, tp: bar.Close - rMult*risk,
				why: fmt.Sprintf("sweep-reject EQH %.4f (band %.4f-%.4f, %dx)", p.Price, p.Lo, p.Hi, p.Touches)}
		}
		if wantLong && p.Kind == signal.EQL && bar.Low < p.Lo && bar.Close > p.Hi {
			stop := bar.Low - bufATR*a
			risk := bar.Close - stop
			if risk <= 0 {
				continue
			}
			return autoTrigger{fire: true, side: "long", entry: bar.Close, market: true,
				stop: stop, tp: bar.Close + rMult*risk,
				why: fmt.Sprintf("sweep-reject EQL %.4f (band %.4f-%.4f, %dx)", p.Price, p.Lo, p.Hi, p.Touches)}
		}
	}
	return autoTrigger{}
}

// autoMinScore is the confluence threshold an engine-strategy rule must clear to
// fire (env AUTO_MIN_SCORE, default 3 — same as the daemon's MIN_SCORE).
func autoMinScore() int {
	if v := strings.TrimSpace(os.Getenv("AUTO_MIN_SCORE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

// evalEngine runs the same confluence engine the daemon/web use and fires on its
// Plan. This is the "comprehensive" path: full MR+momentum scoring, N-struct veto,
// pivot zones, and per-symbol StructMomentum dispatch all apply. Entry/stop/TP come
// from the engine's structure-derived Plan (NOT a box), so the stop sits on real
// structure. Gated by side match + a minimum confluence score.
func evalEngine(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	tf := market.Timeframe(r.TF)
	candles, err := client.Klines(ctx, sym, tf, 300)
	if err != nil || len(candles) < 50 {
		return autoTrigger{}
	}
	// HTF bias (mirror the daemon: a higher-TF directional bias enriches Evaluate).
	bias := signal.Flat
	if bc, berr := client.Klines(ctx, sym, engineBiasTF(tf), 100); berr == nil {
		bias = signal.Bias(bc)
	}
	sigCtx := signal.Context{}
	var markPrice float64
	if fr, ferr := client.FundingRate(ctx, sym); ferr == nil {
		sigCtx.FundingRate = fr.Rate
		markPrice = fr.MarkPrice
	}
	if oi, oerr := client.OpenInterest(ctx, sym); oerr == nil {
		sigCtx.OpenInterest = oi
	}
	s := signal.Evaluate(signal.Inputs{
		Symbol: sym, Timeframe: tf, Candles: candles, Ctx: sigCtx, Bias: bias, LiveMarkPrice: markPrice,
	})
	if s.Side == signal.Flat || s.Plan.Entry <= 0 || s.Plan.StopLoss <= 0 {
		return autoTrigger{}
	}
	if s.Score < autoMinScore() {
		return autoTrigger{}
	}
	side := "long"
	if s.Side == signal.Short {
		side = "short"
	}
	// respect the rule's side filter (auto = both)
	if (r.Side == "long" && side != "long") || (r.Side == "short" && side != "short") {
		return autoTrigger{}
	}
	tp := s.Plan.Entry
	if len(s.Plan.TakeProfit) > 0 {
		tp = s.Plan.TakeProfit[0]
	}
	anchor := s.Plan.Anchor
	if anchor == "" {
		anchor = signal.StrategyFor(sym, tf).String()
	}
	// Marketable if the entry sits on the fillable side of the current price
	// (long entry at/above spot, short entry at/below spot) → fills immediately.
	// Otherwise it's a resting limit (e.g. a sweep-high short above spot) → pending
	// until price trades to it.
	curPx := candles[len(candles)-1].Close
	if markPrice > 0 {
		curPx = markPrice
	}
	marketable := (side == "long" && s.Plan.Entry >= curPx) || (side == "short" && s.Plan.Entry <= curPx)
	return autoTrigger{fire: true, side: side, entry: s.Plan.Entry, stop: s.Plan.StopLoss, tp: tp, market: marketable,
		why: fmt.Sprintf("engine score %d — %s", s.Score, anchor)}
}

// engineBiasTF picks the higher-TF used for directional bias in the engine call.
func engineBiasTF(tf market.Timeframe) market.Timeframe {
	switch tf {
	case "5m", "15m", "30m", "1h":
		return "4h"
	case "2h", "4h":
		return "1d"
	default:
		return "1d"
	}
}

// evalRangeEdge fires a mean-reversion entry at the range edge (buy the bottom
// third / short the top third of the recent 24-bar box), stop OUTSIDE the box,
// target the opposite edge. Requires the structure not to fight the side.
func evalRangeEdge(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 250)
	if err != nil || len(cs) < 30 {
		return autoTrigger{}
	}
	px := cs[len(cs)-1].Close
	n := 24
	if len(cs) < n {
		n = len(cs)
	}
	seg := cs[len(cs)-n:]
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
		return autoTrigger{}
	}
	pos := (px - lo) / (hi - lo)
	st := signal.AnalyzeStructure(cs, 2)
	stopBuf := r.StopPct / 100.0

	wantLong := r.Side == "long" || r.Side == "auto"
	wantShort := r.Side == "short" || r.Side == "auto"

	if wantLong && pos <= 0.34 && st.Trend != signal.StructDowntrend {
		return autoTrigger{fire: true, side: "long", entry: px, market: true,
			stop: lo * (1 - stopBuf), tp: hi,
			why: fmt.Sprintf("box %.4f-%.4f pos %.0f%% bottom-third", lo, hi, pos*100)}
	}
	if wantShort && pos >= 0.66 && st.Trend != signal.StructUptrend {
		return autoTrigger{fire: true, side: "short", entry: px, market: true,
			stop: hi * (1 + stopBuf), tp: lo,
			why: fmt.Sprintf("box %.4f-%.4f pos %.0f%% top-third", lo, hi, pos*100)}
	}
	return autoTrigger{}
}

// autoInBlackout reports whether the symbol is inside a macro/earnings blackout
// at barTime — reuse the engine's own gate by running a Flat probe is overkill;
// signal.Evaluate already suppresses in blackout, but here we just need a bool.
func autoInBlackout(sym market.Symbol, barTime time.Time) bool {
	// A no-candle Evaluate is cheap and returns a Reason mentioning "blackout".
	sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: "1h", Candles: []market.Candle{{CloseTime: barTime}}})
	for _, rs := range sig.Reasons {
		if len(rs) >= 8 && (rs[:8] == "macro bl" || rs[:8] == "earnings") {
			return true
		}
	}
	return false
}
