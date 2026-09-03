// Command breakbt A/B-tests a BREAKOUT-CONTINUATION strategy — the complement to
// sweep-reject. Where sweep-reject fades a FAILED break of an EQH/EQL pool, this
// RIDES a break that HOLDS: a bar closes decisively beyond a liquidity pool that
// was resistance/support → momentum long/short. This is the "majors breakout/
// momentum" entry auto lacks. Ship-gate: must be +R across 60/90/120d AND robust
// across params — NOT a single cherry-picked setting (that's overfitting).
//
// ── RESULT 2026-09-04: closed, not parked. Walk-forward retains 7%. ─────────
//
// The 2026-08-27 run rejected this on a param cliff (0.20% collapsed to -10R
// while 0.30-0.40% looked good on 90d/120d) and PARKED the idea, noting that
// "decisive break" MAY hold a weak real edge and should be revisited with
// walk-forward. Both were done here.
//
// SWEEP (aggregate netR, BTC+ETH+SOL+LINK):
//
//	margin    0.10   0.15   0.20   0.25   0.30   0.35   0.40   0.50
//	60d        -42     -4    -10     -7    -11    -16    -10     -8
//	90d        -35     -0    -11    +20    +26    +11     +9     +4
//	120d       -26     +4     +7    +40    +52    +37    +35    +10
//
// Better than 2026-08-27: the plateau above 0.25% is now broader, so the
// neighbourhood objection is weaker than it was. But 60d is NEGATIVE AT EVERY
// MARGIN, so no setting is positive in all three windows and the gate fails
// outright (cmd/gate: "60d netR -11.00 not above +0.00"). SOL alone at 0.30%
// carries the aggregate (+20/+22 on 90/120d) and is still -3 on 60d — the same
// "symbol selection is the real lever" result range-edge produced.
//
// WALK-FORWARD (train 45d / test 15d, margin chosen in-sample from an 8-value
// grid, then scored on the untouched next window, 16 folds):
//
//	in-sample total   +127.00R
//	out-of-sample       +9.00R      <- 7% retention
//	folds             7 positive / 6 negative / 3 zero
//	margins picked    0.30% x7, 0.25% x3, 0.40% x2, 0.50% x2, 0.15% x1, 0.35% x1
//
// A 7% retention with a coin-flip fold split is the overfitting signature. The
// in-sample optimum is noise; 0.30% being the plurality pick (7 of 16) is not
// stability.
//
// Being fair to the number: out-of-sample IS positive, +0.067 R/trade over 135
// trades. So the honest claim is not "no edge" but "an edge indistinguishable
// from noise at this sample size, and far below what is already shipped" —
// sweep-reject runs +0.10 to +0.30 R/trade. Under max_concurrent_total=4 a
// +0.067 strategy does not merely underperform, it DISPLACES a better one, so
// its expected contribution is negative. That is a stronger reason to decline
// it than the original param cliff.
//
// Closed rather than parked: the revisit the note asked for has happened.
//
// Usage:
//
//	go run ./cmd/breakbt --days 120 --sweep 0.10,0.20,0.30,0.40,0.50
//	go run ./cmd/breakbt --days 120 --walk-forward --train-days 45 --test-days 15
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
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
	symbols := flag.String("symbols", "", "comma-separated symbols instead of the default four; accepts short names or raw contract codes")
	sweep := flag.String("sweep", "", "comma-separated break-margin values to sweep, e.g. 0.10,0.20,0.30,0.40,0.50. Prints a per-symbol grid — the point is the NEIGHBOURHOOD, since the 2026-08-27 rejection was a param cliff at 0.20%, not a bad number at 0.30%")
	walkFwd := flag.Bool("walk-forward", false, "the test the 2026-08-27 rejection asked for: pick the best margin IN-SAMPLE, then score it on the NEXT out-of-sample window, rolling forward. A margin that only works when chosen with hindsight fails here")
	trainDays := flag.Int("train-days", 45, "walk-forward: in-sample length per fold")
	testDays := flag.Int("test-days", 15, "walk-forward: out-of-sample length per fold")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}}
	if strings.TrimSpace(*symbols) != "" {
		syms = syms[:0]
		for _, tok := range strings.Split(*symbols, ",") {
			tok = strings.ToUpper(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			code, short := tok, tok
			if !strings.Contains(tok, "-USDT") {
				code = tok + "-USDT"
			} else if i := strings.Index(tok, "2USD-USDT"); i > 4 {
				short = strings.TrimPrefix(tok[:i], "NCSK")
			}
			syms = append(syms, struct {
				short string
				sym   market.Symbol
			}{short, market.Symbol(code)})
		}
	}

	// Fetch once per symbol; the sweep and walk-forward both replay the same
	// candles at different parameters, so refetching per parameter would be
	// pure waste and would also risk the series shifting mid-run.
	series := map[string][]market.Candle{}
	atrs := map[string][]float64{}
	for _, sy := range syms {
		cs, err := client.KlinesRange(context.Background(), sy.sym, tf, start, end)
		if err != nil || len(cs) < 120 {
			log.Printf("%s: klines %v (len %d)", sy.short, err, len(cs))
			continue
		}
		series[sy.short] = cs
		atrs[sy.short] = alignRight(indicator.ATR(cs, 14), len(cs))
	}

	if strings.TrimSpace(*sweep) != "" {
		runSweep(series, atrs, syms, *sweep, *tol/100.0, *bufATR, *rMult, *longOnly, *days)
		return
	}
	if *walkFwd {
		runWalkForward(series, atrs, syms, *tol/100.0, *bufATR, *rMult, *longOnly, *trainDays, *testDays, tf)
		return
	}

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

// score replays one parameter setting and returns netR, resolved count and wins.
func score(cs []market.Candle, atr []float64, short string, tolFrac, margin, bufATR, rMult float64, longOnly bool) (netR float64, n, won int) {
	fires := genBreakFires(cs, atr, short, tolFrac, margin, bufATR, rMult, longOnly)
	positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.EvaluateFire(f, cs, 24)
	})
	for _, p := range positions {
		switch p.Outcome.Status {
		case autotrade.OutTP:
			netR += p.Outcome.NetR
			n++
			won++
		case autotrade.OutStop:
			netR += p.Outcome.NetR
			n++
		}
	}
	return netR, n, won
}

// runSweep prints netR and R/trade per (symbol, margin).
//
// The 2026-08-27 rejection was NOT "0.30% is a bad number" — 0.30-0.40% flipped
// 90d and 120d positive. It was that 0.20% collapsed to -10R, i.e. a cliff
// right next door. So the neighbourhood is the finding, not the best cell, and
// this prints the whole row so a cliff cannot be read as a peak.
func runSweep(series map[string][]market.Candle, atrs map[string][]float64, syms []struct {
	short string
	sym   market.Symbol
}, sweepSpec string, tolFrac, bufATR, rMult float64, longOnly bool, days int) {
	var margins []float64
	for _, tok := range strings.Split(sweepSpec, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(tok), 64); err == nil {
			margins = append(margins, v)
		}
	}
	sort.Float64s(margins)
	fmt.Printf("=== break-margin sweep · %dd · TP %.1fR · read the ROW, not the best cell ===\n", days, rMult)
	fmt.Printf("%-6s", "sym")
	for _, m := range margins {
		fmt.Printf("  %8s", fmt.Sprintf("%.2f%%", m))
	}
	fmt.Println()
	agg := make([]float64, len(margins))
	aggN := make([]int, len(margins))
	for _, sy := range syms {
		cs, ok := series[sy.short]
		if !ok {
			continue
		}
		fmt.Printf("%-6s", sy.short)
		for i, m := range margins {
			r, n, _ := score(cs, atrs[sy.short], sy.short, tolFrac, m/100.0, bufATR, rMult, longOnly)
			agg[i] += r
			aggN[i] += n
			fmt.Printf("  %+8.2f", r)
		}
		fmt.Println()
	}
	fmt.Printf("%-6s", "AGG")
	for i := range margins {
		fmt.Printf("  %+8.2f", agg[i])
	}
	fmt.Println()
	fmt.Printf("%-6s", "R/trd")
	for i := range margins {
		v := 0.0
		if aggN[i] > 0 {
			v = agg[i] / float64(aggN[i])
		}
		fmt.Printf("  %+8.3f", v)
	}
	fmt.Println()
	fmt.Printf("%-6s", "n")
	for i := range margins {
		fmt.Printf("  %8d", aggN[i])
	}
	fmt.Println()
}

// runWalkForward is the test the parked note asked for.
//
// Per fold: sweep the margin on the TRAIN slice, take the best by netR, then
// score THAT margin on the TEST slice that follows it. The chosen margin never
// sees the data it is judged on. A parameter that only works when picked with
// hindsight produces a good train column and a bad test column, which is
// exactly the shape a cross-window check cannot see.
func runWalkForward(series map[string][]market.Candle, atrs map[string][]float64, syms []struct {
	short string
	sym   market.Symbol
}, tolFrac, bufATR, rMult float64, longOnly bool, trainDays, testDays int, tf market.Timeframe) {
	grid := []float64{0.10, 0.15, 0.20, 0.25, 0.30, 0.35, 0.40, 0.50}
	barsPerDay := 24.0
	if tf == "2h" {
		barsPerDay = 12
	} else if tf == "4h" {
		barsPerDay = 6
	}
	trainBars := int(float64(trainDays) * barsPerDay)
	testBars := int(float64(testDays) * barsPerDay)

	fmt.Printf("=== breakout-continuation WALK-FORWARD · train %dd / test %dd · TP %.1fR ===\n", trainDays, testDays, rMult)
	fmt.Printf("margin grid: %v\n\n", grid)
	fmt.Printf("%-6s %5s %10s %10s %8s %10s %8s\n", "sym", "fold", "pickedM", "trainR", "trainN", "TEST R", "testN")

	var aggTest float64
	var aggTestN int
	chosen := map[float64]int{}
	for _, sy := range syms {
		cs, ok := series[sy.short]
		if !ok {
			continue
		}
		atr := atrs[sy.short]
		fold := 0
		var symTest float64
		var symTestN int
		for start := 0; start+trainBars+testBars <= len(cs); start += testBars {
			trainCS := cs[start : start+trainBars]
			trainATR := atr[start : start+trainBars]
			testCS := cs[start+trainBars : start+trainBars+testBars]
			testATR := atr[start+trainBars : start+trainBars+testBars]

			bestM, bestR, bestN := 0.0, 0.0, 0
			first := true
			for _, m := range grid {
				r, n, _ := score(trainCS, trainATR, sy.short, tolFrac, m/100.0, bufATR, rMult, longOnly)
				if first || r > bestR {
					bestM, bestR, bestN, first = m, r, n, false
				}
			}
			tr, tn, _ := score(testCS, testATR, sy.short, tolFrac, bestM/100.0, bufATR, rMult, longOnly)
			fold++
			chosen[bestM]++
			symTest += tr
			symTestN += tn
			fmt.Printf("%-6s %5d %9.2f%% %+10.2f %8d %+10.2f %8d\n", sy.short, fold, bestM, bestR, bestN, tr, tn)
		}
		if fold > 0 {
			rpt := 0.0
			if symTestN > 0 {
				rpt = symTest / float64(symTestN)
			}
			fmt.Printf("%-6s %5s %10s %10s %8s %+10.2f %8d   <- out-of-sample total, R/trade %+0.3f\n",
				sy.short, "all", "", "", "", symTest, symTestN, rpt)
		}
		aggTest += symTest
		aggTestN += symTestN
	}
	rpt := 0.0
	if aggTestN > 0 {
		rpt = aggTest / float64(aggTestN)
	}
	fmt.Printf("\n--- OUT-OF-SAMPLE aggregate: netR %+.2f over %d trades, R/trade %+0.3f ---\n", aggTest, aggTestN, rpt)
	fmt.Printf("    margins chosen in-sample: %v\n", chosen)
	fmt.Printf("    A stable edge picks a stable margin. A scattered pick across folds means the\n")
	fmt.Printf("    in-sample optimum is noise, and the out-of-sample number is what it is worth.\n")
}
