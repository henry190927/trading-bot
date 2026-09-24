// Command snrbt A/B-tests the auto-executor's "htf-snr" strategy: fade a touch
// of a HIGHER-timeframe swing level that the current bar rejects — price pokes
// a 4h swing high and closes back below it → short; mirror at a swing low →
// long.
//
// Why it exists: htf-snr was the only rule in autotrade.json with no backtest.
// The 2026-09-23 audit could gate every other rule and had to disable this one
// for being unmeasurable rather than for being bad, which is a poor reason to
// turn something off and a worse reason to leave it on.
//
// The fire logic is copied from autostrat.evalHTFSNR deliberately, constants
// included, rather than reimplemented from the idea. A backtest that measures a
// different rule than the daemon runs is worse than no backtest, because it
// produces a number people trust. Scored with autotrade.DedupFires +
// autotrade.EvaluateFire, the same primitives the live panel uses.
//
// NO LOOK-AHEAD: at each base bar only HTF swing points whose confirming candle
// CLOSED before that bar opened are eligible — the same `!CloseTime.Before(
// bar.OpenTime)` test the live path applies. A swing is only a swing once the
// `strength` bars after it have printed, and forgetting that is how an
// HTF-context backtest quietly reads the future.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

func main() {
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "base timeframe (the HTF is autostrat.EngineBiasTF of it)")
	strength := flag.Int("strength", 3, "HTF swing-point fractal strength")
	tolPct := flag.Float64("tol", 0.20, "touch tolerance around the HTF level (%)")
	bufATR := flag.Float64("buf-atr", 0.25, "stop buffer beyond the reject wick, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as an R multiple")
	side := flag.String("side", "auto", "long | short | auto")
	symbols := flag.String("symbols", "", "comma-separated symbols; short names or raw contract codes")
	flag.Parse()

	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	htf := autostrat.EngineBiasTF(tf)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)

	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"SUI", market.SUIUSDT}}
	if strings.TrimSpace(*symbols) != "" {
		syms = syms[:0]
		for _, tok := range strings.Split(*symbols, ",") {
			tok = strings.ToUpper(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			sym, ok := market.Resolve(tok)
			if !ok {
				sym = market.Symbol(tok)
				if !strings.Contains(tok, "-USDT") && !strings.Contains(tok, "-USDC") {
					sym = market.Symbol(tok + "-USDT")
				}
			}
			sh := market.Short(sym)
			if sh == "" {
				sh = string(sym)
			}
			syms = append(syms, struct {
				short string
				sym   market.Symbol
			}{sh, sym})
		}
	}

	fmt.Printf("=== htf-snr A/B · %dd · base %s · HTF %s · strength %d · tol %.2f%% · stop=wick+%.2fATR · TP %.1fR · side %s ===\n",
		*days, tf, htf, *strength, *tolPct, *bufATR, *rMult, *side)
	fmt.Printf("%-5s %6s %6s %5s %5s %7s %9s %8s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR", "R/trade", "trd/day")

	var aggR float64
	var aggN int
	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: base klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		// The HTF series is fetched over a LONGER window: a swing near the
		// start of the base range needs bars before it to be a swing at all.
		hcs, herr := client.KlinesRange(context.Background(), s.sym, htf, start.AddDate(0, 0, -*days), end)
		if herr != nil || len(hcs) < 40 {
			log.Printf("%s: htf klines %v (len %d)", s.short, herr, len(hcs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genSNRFires(cs, hcs, atr, s.short, *strength, *tolPct/100.0, *bufATR, *rMult, *side)
		positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
			return autotrade.EvaluateFire(f, cs, 6)
		})
		var netR float64
		var tp, stop, filled int
		for _, p := range positions {
			switch p.Outcome.Status {
			case autotrade.OutTP:
				tp, filled = tp+1, filled+1
				netR += p.Outcome.NetR
			case autotrade.OutStop:
				stop, filled = stop+1, filled+1
				netR += p.Outcome.NetR
			case autotrade.OutOpen:
				filled++
			}
		}
		win, fp, rpt, tpd := 0.0, 0.0, 0.0, 0.0
		if tp+stop > 0 {
			win = float64(tp) / float64(tp+stop) * 100
		}
		if len(positions) > 0 {
			fp = float64(filled) / float64(len(positions)) * 100
			rpt = netR / float64(len(positions))
			tpd = float64(len(positions)) / float64(*days)
		}
		fmt.Printf("%-5s %6d %5.0f%% %5d %5d %6.0f%% %+9.2f %+8.3f %8.2f\n",
			s.short, len(positions), fp, tp, stop, win, netR, rpt, tpd)
		aggR += netR
		aggN += len(positions)
	}
	aggRPT, aggTPD := 0.0, 0.0
	if aggN > 0 {
		aggRPT = aggR / float64(aggN)
		aggTPD = float64(aggN) / float64(*days)
	}
	fmt.Printf("--- aggregate: %d positions, netR %+.2f, R/trade %+.3f, trd/day %.2f ---\n", aggN, aggR, aggRPT, aggTPD)
}

// genSNRFires walks closed base bars and emits a fire wherever the live
// evalHTFSNR would have. Mirrors that function's shape, including taking the
// FIRST matching swing rather than the best one.
func genSNRFires(cs, hcs []market.Candle, atr []float64, short string, strength int,
	tolFrac, bufATR, rMult float64, side string) []autotrade.PaperFire {

	sw := signal.FindSwingPoints(hcs, strength, 0)
	wantShort := side == "short" || side == "auto"
	wantLong := side == "long" || side == "auto"

	var out []autotrade.PaperFire
	for i := 30; i < len(cs); i++ {
		bar, prev, a := cs[i], cs[i-1], atr[i]
		if a <= 0 {
			continue
		}
		for _, p := range sw {
			// Confirmation index: a swing at p.Index is only known once
			// `strength` further HTF bars have closed.
			ci := p.Index + strength
			if ci >= len(hcs) {
				ci = len(hcs) - 1
			}
			if !hcs[ci].CloseTime.Before(bar.OpenTime) {
				continue // not yet confirmed when this bar opened
			}
			tolAbs := p.Price * tolFrac
			if wantShort && p.IsTop && prev.Close < p.Price && bar.High >= p.Price-tolAbs && bar.Close < p.Price {
				stop := bar.High + bufATR*a
				risk := stop - bar.Close
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, Strategy: "htf-snr", Side: "short",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close - rMult*risk,
				})
				break
			}
			if wantLong && !p.IsTop && prev.Close > p.Price && bar.Low <= p.Price+tolAbs && bar.Close > p.Price {
				stop := bar.Low - bufATR*a
				risk := bar.Close - stop
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, Strategy: "htf-snr", Side: "long",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close + rMult*risk,
				})
				break
			}
		}
	}
	return out
}

func alignRight(arr []float64, n int) []float64 {
	if len(arr) == n {
		return arr
	}
	out := make([]float64, n)
	off := n - len(arr)
	for i := range out {
		if i < off {
			if len(arr) > 0 {
				out[i] = arr[0]
			}
			continue
		}
		out[i] = arr[i-off]
	}
	return out
}
