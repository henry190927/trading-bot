// Command rangebt is an A/B backtest for the auto-executor's range-edge strategy.
// range-edge lives in cmd/monitor (not the signal engine), so it isn't covered by
// the main backtest — this reconstructs it over historical klines and scores each
// variant with the SAME tested primitives the live panel uses (autotrade.DedupFires
// for one-position-per-rule + autotrade.EvaluateFire for closed-bar outcomes).
//
// Variants layered on the baseline box trigger:
//
//	V0 baseline  — pos<=0.34 long (trend!=Down) / pos>=0.66 short (trend!=Up)
//	V1 +mom      — also require EMA20 not sloping against the trade (blocks
//	               longs in a neutral-but-declining chop — the whipsaw killer)
//	V2 +vol      — skip range-edge when ATR% > threshold (MR only in calm ranges)
//	V3 both
//
// Usage: go run ./cmd/rangebt --days 60   (also run 90, 120)
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

type variant struct {
	name             string
	momFilter        bool
	volGate          bool
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 60, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	stopPct := flag.Float64("stop-pct", 0.5, "stop buffer beyond box edge (%)")
	volThresh := flag.Float64("vol-thresh", 1.2, "ATR%% ceiling for the vol gate")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)

	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}}

	variants := []variant{
		{"V0-baseline", false, false},
		{"V1-mom", true, false},
		{"V2-vol", false, true},
		{"V3-both", true, true},
	}

	fmt.Printf("=== range-edge A/B · %dd · %s · stop %.2f%% · vol-gate ATR%%>%.1f ===\n", *days, *tfStr, *stopPct, *volThresh)
	fmt.Printf("%-5s %-12s %6s %6s %5s %5s %7s %7s\n", "sym", "variant", "pos", "fill%", "tp", "stop", "win%", "netR")

	// aggregate netR per variant across symbols
	agg := map[string]float64{}
	aggPos := map[string]int{}

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 60 {
			log.Printf("%s: klines: %v (len %d)", s.short, err, len(cs))
			continue
		}
		closes := make([]float64, len(cs))
		for i, c := range cs {
			closes[i] = c.Close
		}
		ema := alignRight(indicator.EMA(closes, 20), len(cs))
		atr := alignRight(indicator.ATR(cs, 14), len(cs))

		for _, v := range variants {
			fires := genFires(cs, ema, atr, v, *stopPct/100.0, *volThresh/100.0, s.short)
			resolve := func(f autotrade.PaperFire) autotrade.Outcome {
				return autotrade.EvaluateFire(f, cs, 6)
			}
			positions := autotrade.DedupFires(fires, 6, 6, time.Hour, resolve)
			sum := summarize(positions)
			fmt.Printf("%-5s %-12s %6d %5.0f%% %5d %5d %6.0f%% %+7.2f\n",
				s.short, v.name, sum.total, sum.fillPct*100, sum.tp, sum.stop, sum.winPct*100, sum.netR)
			agg[v.name] += sum.netR
			aggPos[v.name] += sum.total
		}
		fmt.Println()
	}

	fmt.Println("--- aggregate (BTC+ETH+SOL) ---")
	for _, v := range variants {
		fmt.Printf("%-12s positions %-4d netR %+7.2f\n", v.name, aggPos[v.name], agg[v.name])
	}
}

// genFires walks bar-by-bar (closed-bar) reconstructing range-edge triggers under
// the variant's filters. entry = bar close, marketable (range-edge is a market-ish
// fill at the box edge on close).
func genFires(cs []market.Candle, ema, atr []float64, v variant, stopBuf, volThresh float64, short string) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	const boxN = 24
	for i := boxN; i < len(cs); i++ {
		seg := cs[i-boxN+1 : i+1]
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
			continue
		}
		px := cs[i].Close
		pos := (px - lo) / (hi - lo)

		// vol gate: skip entirely when too volatile for mean-reversion
		if v.volGate && atr[i] > 0 && atr[i]/px > volThresh {
			continue
		}

		// structure over a 250-bar lookback (matches live Klines(250))
		lb := i - 249
		if lb < 0 {
			lb = 0
		}
		st := signal.AnalyzeStructure(cs[lb:i+1], 2)

		emaUp := i >= 3 && ema[i] >= ema[i-3]
		emaDown := i >= 3 && ema[i] <= ema[i-3]

		if pos <= 0.34 && st.Trend != signal.StructDowntrend {
			if !v.momFilter || emaUp {
				out = append(out, autotrade.PaperFire{
					Time: cs[i].CloseTime, Symbol: short, TF: "1h", Strategy: "range-edge", Side: "long",
					Market: true, Entry: px, Stop: lo * (1 - stopBuf), TP: hi,
				})
				continue
			}
		}
		if pos >= 0.66 && st.Trend != signal.StructUptrend {
			if !v.momFilter || emaDown {
				out = append(out, autotrade.PaperFire{
					Time: cs[i].CloseTime, Symbol: short, TF: "1h", Strategy: "range-edge", Side: "short",
					Market: true, Entry: px, Stop: hi * (1 + stopBuf), TP: lo,
				})
			}
		}
	}
	return out
}

type stats struct {
	total, tp, stop, nofill int
	netR, fillPct, winPct   float64
}

func summarize(ps []autotrade.Position) stats {
	var s stats
	s.total = len(ps)
	filled := 0
	for _, p := range ps {
		switch p.Outcome.Status {
		case autotrade.OutTP:
			s.tp++
			filled++
			s.netR += p.Outcome.NetR
		case autotrade.OutStop:
			s.stop++
			filled++
			s.netR += p.Outcome.NetR
		case autotrade.OutOpen:
			filled++
		case autotrade.OutNoFill:
			s.nofill++
		}
	}
	if s.total > 0 {
		s.fillPct = float64(filled) / float64(s.total)
	}
	if res := s.tp + s.stop; res > 0 {
		s.winPct = float64(s.tp) / float64(res)
	}
	return s
}

// alignRight pads a possibly-shorter indicator series to length n by left-padding
// with the first value's index offset, so arr[i] lines up with cs[i].
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
