// Command sweepbt A/B-tests the "sweep-reject" entry extracted from the group's
// SMC method: price runs above an EQH pool (grabs buy-side liquidity) then CLOSES
// back below it (failed breakout) → short; mirror for EQL → long. Entry on the
// reject bar's close; stop just BEYOND the sweep wick (the 🪝 lesson: off the
// magnet); TP a fixed R multiple. Scored with the tested EvaluateFire + one-
// position DedupFires. Ship-gate: prove +R across 60/90/120d before wiring live.
//
// Usage: go run ./cmd/sweepbt --days 90   (also 60, 120)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	tol := flag.Float64("tol", 0.15, "EQH/EQL cluster tolerance (%)")
	bufATR := flag.Float64("buf-atr", 0.15, "stop buffer beyond the sweep wick, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as R multiple of the stop distance")
	liqTP := flag.Bool("liq-tp", false, "A/B (a): TP at the nearest OPPOSITE liquidity pool (magnet) instead of fixed R; fall back to R if none / if it's a worse-than-1R target")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}, {"XAU", market.XAUUSDT}, {"XAG", market.XAGUSDT}}

	fmt.Printf("=== sweep-reject A/B · %dd · %s · tol %.2f%% · stop=sweep+%.2fATR · TP %.1fR ===\n", *days, *tfStr, *tol, *bufATR, *rMult)
	fmt.Printf("%-5s %6s %6s %5s %5s %7s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR")
	var aggR float64
	var aggN int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genSweepFires(cs, atr, s.short, *tol/100.0, *bufATR, *rMult, *liqTP)
		positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
			return autotrade.EvaluateFire(f, cs, 6)
		})
		var netR float64
		var tp, stop, filled int
		for _, p := range positions {
			switch p.Outcome.Status {
			case autotrade.OutTP:
				tp++
				filled++
				netR += p.Outcome.NetR
			case autotrade.OutStop:
				stop++
				filled++
				netR += p.Outcome.NetR
			case autotrade.OutOpen:
				filled++
			}
		}
		win := 0.0
		if tp+stop > 0 {
			win = float64(tp) / float64(tp+stop) * 100
		}
		fp := 0.0
		if len(positions) > 0 {
			fp = float64(filled) / float64(len(positions)) * 100
		}
		fmt.Printf("%-5s %6d %5.0f%% %5d %5d %6.0f%% %+8.2f\n", s.short, len(positions), fp, tp, stop, win, netR)
		aggR += netR
		aggN += len(positions)
	}
	fmt.Printf("--- aggregate: %d positions, netR %+.2f ---\n", aggN, aggR)
}

// genSweepFires walks closed bars; at each bar it detects a sweep-and-reject of a
// pre-existing EQH/EQL pool and emits a marketable fire at the reject close.
func genSweepFires(cs []market.Candle, atr []float64, short string, tolFrac, bufATR, rMult float64, liqTP bool) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac) // pools formed BEFORE this bar
		bar := cs[i]
		a := atr[i]
		above, below := signal.NearestLiquidity(pools, bar.Close)
		for _, p := range pools {
			if p.Kind == signal.EQH && bar.High > p.Hi && bar.Close < p.Lo {
				// swept buy-side liquidity above, closed back below → short
				stop := bar.High + bufATR*a
				risk := stop - bar.Close
				if risk <= 0 {
					continue
				}
				tp := bar.Close - rMult*risk
				if liqTP && below != nil && below.Price < bar.Close-risk { // magnet at least 1R away
					tp = below.Price
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "short",
					Market: true, Entry: bar.Close, Stop: stop, TP: tp,
				})
				break // one setup per bar
			}
			if p.Kind == signal.EQL && bar.Low < p.Lo && bar.Close > p.Hi {
				stop := bar.Low - bufATR*a
				risk := bar.Close - stop
				if risk <= 0 {
					continue
				}
				tp := bar.Close + rMult*risk
				if liqTP && above != nil && above.Price > bar.Close+risk {
					tp = above.Price
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "sweep-reject", Side: "long",
					Market: true, Entry: bar.Close, Stop: stop, TP: tp,
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
		} else {
			out[i] = arr[i-off]
		}
	}
	return out
}
