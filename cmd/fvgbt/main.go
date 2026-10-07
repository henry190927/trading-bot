// Command fvgbt A/B-tests C3: trading a retrace into an unfilled fair-value
// gap, in the direction the gap was created.
//
// Why this one and not C9: the open-bar/clock family has now failed four
// separate ways (cash-open Layer 1, the open-price gate in all three windows,
// AMD session anchoring 0/3), while the level-quality family keeps producing
// the edges that survive gating — htf-snr, the pivot zones, the NFE structure
// veto. An FVG is a level, judged by how it formed. It belongs to the family
// that works.
//
// Scored with autotrade.DedupFires + autotrade.EvaluateFire, the same
// primitives the live panel and every other *bt command use, so the numbers
// are comparable to the rest of the book rather than to themselves.
//
// This measures a strategy that is NOT wired into autostrat. Unlike snrbt,
// which copies evalHTFSNR verbatim, there is no live function to mirror yet —
// so if this gates PASS, the port into autostrat has to be diffed against
// genFVGFires rather than written from the idea.
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
)

func main() {
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	minGap := flag.Float64("min-gap", 0.25, "minimum gap height in ATR(14), judged at formation")
	maxAge := flag.Int("max-age", 50, "drop a gap price has not revisited within this many bars")
	bufATR := flag.Float64("buf-atr", 0.25, "stop buffer beyond the gap's far edge, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as an R multiple")
	side := flag.String("side", "auto", "long | short | auto")
	trend := flag.Bool("trend", false, "require the gap direction to agree with HTF bias (close vs EMA21 on autostrat.EngineBiasTF)")
	symbols := flag.String("symbols", "", "comma-separated symbols; short names or raw contract codes")
	flag.Parse()

	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
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

	fmt.Printf("=== fvg A/B · %dd · %s · min-gap %.2fATR · max-age %d · stop=edge+%.2fATR · TP %.1fR · side %s · trend %v ===\n",
		*days, tf, *minGap, *maxAge, *bufATR, *rMult, *side, *trend)
	fmt.Printf("%-5s %6s %6s %6s %5s %5s %7s %9s %8s %8s\n",
		"sym", "gaps", "pos", "fill%", "tp", "stop", "win%", "netR", "R/trade", "trd/day")

	var aggR float64
	var aggN int
	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		var bias []int
		if *trend {
			htf := autostrat.EngineBiasTF(tf)
			hcs, herr := client.KlinesRange(context.Background(), s.sym, htf, start.AddDate(0, 0, -30), end)
			if herr != nil || len(hcs) < 40 {
				log.Printf("%s: htf klines %v (len %d)", s.short, herr, len(hcs))
				continue
			}
			bias = htfBias(cs, hcs, 21)
		}
		nGaps := len(FindFVGs(cs))
		fires := genFVGFires(cs, atr, s.short, *minGap, *bufATR, *rMult, *maxAge, *side, bias)
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
		fmt.Printf("%-5s %6d %6d %5.0f%% %5d %5d %6.0f%% %+9.2f %+8.3f %8.2f\n",
			s.short, nGaps, len(positions), fp, tp, stop, win, netR, rpt, tpd)
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

// htfBias maps each base bar to the higher-timeframe direction that was
// READABLE when that bar opened: +1 if the last fully closed HTF candle shut
// above its EMA, -1 below, 0 before the EMA has warmed up.
//
// The `CloseTime.Before(OpenTime)` walk is the same no-look-ahead test snrbt
// applies to HTF swings, and for the same reason: the 4h candle a 1h bar sits
// inside has not closed yet, and reading it is reading the future.
func htfBias(cs, hcs []market.Candle, period int) []int {
	closes := make([]float64, len(hcs))
	for i, c := range hcs {
		closes[i] = c.Close
	}
	ema := alignRight(indicator.EMA(closes, period), len(hcs))

	out := make([]int, len(cs))
	k := -1
	for i, bar := range cs {
		for k+1 < len(hcs) && hcs[k+1].CloseTime.Before(bar.OpenTime) {
			k++
		}
		if k < period || ema[k] <= 0 {
			continue
		}
		switch {
		case hcs[k].Close > ema[k]:
			out[i] = 1
		case hcs[k].Close < ema[k]:
			out[i] = -1
		}
	}
	return out
}
