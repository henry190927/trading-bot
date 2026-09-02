package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"myFirstGo/trading-bot/autostrat"
	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
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
	var halt haltState

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

		// Global risk caps need a whole-book view, so assemble it ONCE per
		// tick. The kline cache is shared with paperBlocked below, which
		// otherwise refetches the same candles per rule.
		now := time.Now().UTC()
		kc := newKlineCache(ctx, client)
		book, unscored := autotrade.BuildBook(autotrade.ReadFires(500), kc.forFire, now, maxCooldownBars(cfg))
		logBook(book, cfg, unscored)

		// The daily-loss breaker is a whole-executor stop, not a per-rule one:
		// evaluate it before touching any rule so a halted day does no work
		// and makes no API calls for triggers.
		if v := autotrade.CheckCaps(cfg, book, 0); v.Halt {
			if halt.announce(now) {
				log.Printf("autoexec: HALTED — %s", v.Reason)
				if n != nil {
					_ = n.Push(ctx, "🛑 auto HALTED", v.Reason, "octagonal_sign")
				}
			}
			continue
		}

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
			trig := autostrat.EvalAutoTrigger(ctx, client, sym, r)
			if !trig.Fire {
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
			} else if paperBlocked(kc, r, barTime) {
				continue
			}

			// Global caps, with THIS rule's margin as the candidate. Checked
			// after the per-rule guards so a rule that was going to be skipped
			// anyway doesn't consume a slot in the reasoning, and re-checked
			// per rule because an earlier rule in this same tick may have just
			// taken the last slot.
			if v := autotrade.CheckCaps(cfg, book, r.MarginUSDT); v.Blocked {
				log.Printf("autoexec: %s %s/%s BLOCKED by caps — %s", r.Symbol, r.TF, r.Strategy, v.Reason)
				continue
			}

			qty := r.MarginUSDT * float64(r.Leverage) / trig.Entry
			st.lastFireBar = barTime
			// Account for it immediately: buildBook only runs once per tick, so
			// without this two rules firing in the same tick would both see an
			// empty slot and the cap would be breached by one.
			book.OpenCount++
			book.OpenMargin += r.MarginUSDT
			score := autostrat.FireScore100(ctx, client, sym, r.TF, trig.Side, trig.Entry)

			if !live {
				// PAPER: log + ntfy the intended order, no API call.
				msg := fmt.Sprintf("%s %s @%.4f stop %.4f tp %.4f (qty %.4f, %du×%d) — %s",
					r.Symbol, trig.Side, trig.Entry, trig.Stop, trig.TP, qty, int(r.MarginUSDT), r.Leverage, trig.Why)
				log.Printf("[AUTO-PAPER] would place %s", msg)
				autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, Side: trig.Side, Market: trig.Market,
					Entry: trig.Entry, Stop: trig.Stop, TP: trig.TP, Qty: qty, Margin: r.MarginUSDT, Lev: r.Leverage, Why: trig.Why, Score: score, Live: false})
				if n != nil {
					_ = n.Push(ctx, "🤖 auto (paper)", msg, "robot")
				}
				continue
			}

			// LIVE (triple-gated). Set leverage then place the bracket.
			if err := client.SetLeverage(ctx, sym, "BOTH", r.Leverage); err != nil {
				log.Printf("autoexec: SetLeverage %s: %v", r.Symbol, err)
			}
			res, perr := client.PlaceLimit(ctx, sym, trig.Side, qty, trig.Entry, trig.Stop, trig.TP, false)
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
				r.Symbol, trig.Side, trig.Entry, trig.Stop, trig.TP, qty, id)
			autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, TF: r.TF, Strategy: r.Strategy, Side: trig.Side, Market: trig.Market,
				Entry: trig.Entry, Stop: trig.Stop, TP: trig.TP, Qty: qty, Margin: r.MarginUSDT, Lev: r.Leverage, Why: trig.Why + " id=" + id, Score: score, Live: true})
			if n != nil {
				_ = n.Push(ctx, "🤖 auto PLACED", fmt.Sprintf("%s %s @%.4f (stop %.4f tp %.4f) id=%s", r.Symbol, trig.Side, trig.Entry, trig.Stop, trig.TP, id), "robot")
			}
		}
	}
}

// paperBlocked reports whether a PAPER fire should be suppressed because the rule
// already holds a live position (the prior fire is still open/pending) or is inside
// its post-stop cooldown. This makes paper behave like one-position-per-rule instead
// of re-entering the same persistent setup on every bar close.
func paperBlocked(kc *klineCache, r autotrade.Rule, barTime time.Time) bool {
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
	// Shared with the tick's book assembly, so these candles are already
	// fetched by the time we get here.
	cs := kc.forFire(r.Symbol, r.TF)
	if len(cs) == 0 {
		return true // can't determine → assume still in a position (fail-safe)
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
