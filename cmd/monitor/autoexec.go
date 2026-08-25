package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

type autoTrigger struct {
	fire  bool
	side  string
	entry float64
	stop  float64
	tp    float64
	why   string
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
			// one-position-per-symbol (live only; paper Phase 1 relies on per-bar dedup)
			if live {
				if pos, perr := client.OpenPositions(ctx, sym); perr == nil && len(pos) > 0 {
					continue
				}
			}

			qty := r.MarginUSDT * float64(r.Leverage) / trig.entry
			st.lastFireBar = barTime

			if !live {
				// PAPER: log + ntfy the intended order, no API call.
				msg := fmt.Sprintf("%s %s @%.4f stop %.4f tp %.4f (qty %.4f, %du×%d) — %s",
					r.Symbol, trig.side, trig.entry, trig.stop, trig.tp, qty, int(r.MarginUSDT), r.Leverage, trig.why)
				log.Printf("[AUTO-PAPER] would place %s", msg)
				autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, Side: trig.side,
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
			autotrade.AppendFire(autotrade.PaperFire{Time: time.Now().UTC(), Symbol: r.Symbol, Side: trig.side,
				Entry: trig.entry, Stop: trig.stop, TP: trig.tp, Qty: qty, Margin: r.MarginUSDT, Lev: r.Leverage, Why: trig.why + " id=" + id, Live: true})
			if n != nil {
				_ = n.Push(ctx, "🤖 auto PLACED", fmt.Sprintf("%s %s @%.4f (stop %.4f tp %.4f) id=%s", r.Symbol, trig.side, trig.entry, trig.stop, trig.tp, id), "robot")
			}
		}
	}
}

// evalAutoTrigger dispatches on strategy. Phase 1 implements range-edge; the
// others return no-fire until wired.
func evalAutoTrigger(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) autoTrigger {
	switch r.Strategy {
	case "range-edge":
		return evalRangeEdge(ctx, client, sym, r)
	default:
		return autoTrigger{}
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
		return autoTrigger{fire: true, side: "long", entry: px,
			stop: lo * (1 - stopBuf), tp: hi,
			why: fmt.Sprintf("box %.4f-%.4f pos %.0f%% bottom-third", lo, hi, pos*100)}
	}
	if wantShort && pos >= 0.66 && st.Trend != signal.StructUptrend {
		return autoTrigger{fire: true, side: "short", entry: px,
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
