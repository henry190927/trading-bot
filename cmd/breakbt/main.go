// Command breakbt A/B-tests a BREAKOUT-CONTINUATION strategy — the complement to
// sweep-reject. Where sweep-reject fades a FAILED break of an EQH/EQL pool, this
// RIDES a break that HOLDS: a bar closes decisively beyond a liquidity pool that
// was resistance/support → momentum long/short. This is the "majors breakout/
// momentum" entry auto lacks. Ship-gate: must be +R across 60/90/120d AND robust
// across params — NOT a single cherry-picked setting (that's overfitting).
//
// Usage: go run ./cmd/breakbt --days 90 [--margin 0.1] [--r 2] [--long-only]
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
	days := flag.Int("days", 90, "history window")
	tfStr := flag.String("tf", "1h", "timeframe")
	tol := flag.Float64("tol", 0.15, "EQH/EQL cluster tolerance (%)")
	margin := flag.Float64("margin", 0.10, "close must break the pool by this %% to count as a decisive breakout")
	bufATR := flag.Float64("buf-atr", 0.25, "stop buffer beyond the broken level, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit R multiple")
	longOnly := flag.Bool("long-only", false, "only test EQH-break longs")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}}

	fmt.Printf("=== breakout-continuation A/B · %dd · %s · tol %.2f%% · break-margin %.2f%% · TP %.1fR%s ===\n",
		*days, *tfStr, *tol, *margin, *rMult, map[bool]string{true: " · long-only", false: ""}[*longOnly])
	fmt.Printf("%-5s %6s %6s %5s %5s %7s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR")
	var aggR float64
	var aggN int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: klines %v", s.short, err)
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genBreakFires(cs, atr, s.short, *tol/100.0, *margin/100.0, *bufATR, *rMult, *longOnly)
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

// genBreakFires: a bar whose PREVIOUS close was below an EQH pool and whose OWN
// close is above pool.Hi*(1+margin) = a decisive upside break → long (stop below
// the broken level, TP R mult). Mirror for EQL → short.
func genBreakFires(cs []market.Candle, atr []float64, short string, tolFrac, margin, bufATR, rMult float64, longOnly bool) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 61; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac)
		bar, prev, a := cs[i], cs[i-1], atr[i]
		for _, p := range pools {
			if p.Kind == signal.EQH && prev.Close <= p.Hi && bar.Close > p.Hi*(1+margin) {
				stop := p.Lo - bufATR*a
				risk := bar.Close - stop
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "breakout", Side: "long",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close + rMult*risk,
				})
				break
			}
			if !longOnly && p.Kind == signal.EQL && prev.Close >= p.Lo && bar.Close < p.Lo*(1-margin) {
				stop := p.Hi + bufATR*a
				risk := stop - bar.Close
				if risk <= 0 {
					continue
				}
				out = append(out, autotrade.PaperFire{
					Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "breakout", Side: "short",
					Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close - rMult*risk,
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
