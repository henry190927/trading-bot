// Command flipbt A/B-tests POLARITY FLIP on EQH/EQL pools: the idea that a pool
// price has just BROKEN through, and not yet come back to, acts as support (broken
// EQH) or resistance (broken EQL) on its FIRST retest.
//
// This is the last untested pool idea. It is deliberately EVENT-shaped, because
// the 2026-09-02 finding was that DISTANCE to a level cannot score in this engine
// (both HVN- and EQH/EQL-proximity votes diluted the count threshold and were
// rejected) while the one pool edge that works — sweep-reject — is an event.
// So the setup here is three events in sequence, not a proximity test:
//
//  1. BREAK   — some bar within the last -window bars closed beyond the pool band
//  2. RETEST  — the current bar trades back into the band
//  3. HOLD    — the current bar CLOSES back on the breakout side
//
// Entry at the hold close, stop just beyond the far edge of the band (off the
// magnet, the 🪝 lesson), TP a fixed R multiple. Scored with the same
// EvaluateFire + one-position DedupFires as sweepbt so the numbers are directly
// comparable to the shipped sweep-reject edge.
//
// A flip is dropped if price closed back through the band before the retest —
// that is not a flip any more, it is a failed breakout (which is sweep-reject's
// territory, already shipped).
//
// RESULT 2026-09-02 → REJECTED. Aggregate netR over 60/90/120d:
//
//	min-touch 2   -43.00   -70.00   -77.00   (n=368/548/714)
//	min-touch 3   -11.00    +0.00    -4.00   (n=129/178/236)
//
// Loose pools bleed and worsen with data; strong-pools-only hovers at zero.
// Win rates sit at 25-37% against the 33.3% breakeven a 2R target needs — i.e.
// no information. Only ETH is positive in all three windows (+6/+5/+7) and its
// samples are n=9/16/29, far too small to act on.
//
// A polarity-flipped pool is therefore NOT tradeable as a systematic entry, and
// this closes the last untested pool idea. Kept as a harness so the next variant
// of the question is a flag away.
//
// Usage: go run ./cmd/flipbt --days 90   (also 60, 120)
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
	bufATR := flag.Float64("buf-atr", 0.15, "stop buffer beyond the far edge of the pool band, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as R multiple of the stop distance")
	window := flag.Int("window", 12, "how many bars back the BREAK may have happened for a retest to still count as the first flip test")
	minTouch := flag.Int("min-touch", 2, "minimum touches for a pool to qualify (3+ = only the strong pools)")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}, {"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}}

	fmt.Printf("=== polarity-flip A/B · %dd · %s · tol %.2f%% · stop=band+%.2fATR · TP %.1fR · window %d bars · min-touch %d ===\n",
		*days, *tfStr, *tol, *bufATR, *rMult, *window, *minTouch)
	fmt.Printf("%-5s %6s %6s %5s %5s %7s %9s %8s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR", "R/trade", "trd/day")
	var aggR float64
	var aggN int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 100 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genFlipFires(cs, atr, s.short, *tol/100.0, *bufATR, *rMult, *window, *minTouch)
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

// genFlipFires walks closed bars and emits a fire when the current bar is the
// first RETEST-AND-HOLD of a pool broken within the last `window` bars.
//
// Pools come from cs[:i] — bars strictly before the evaluated one — so nothing
// here can see the bar it is scoring, matching sweepbt and autostrat.
func genFlipFires(cs []market.Candle, atr []float64, short string, tolFrac, bufATR, rMult float64, window, minTouch int) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac)
		bar := cs[i]
		a := atr[i]
		if a <= 0 {
			continue
		}
		for _, p := range pools {
			if p.Touches < minTouch {
				continue
			}
			lo := max(i-window, 1)

			// ── Broken EQH → the band should now act as SUPPORT (long side).
			// Require: a break-close above the band inside the window; price
			// never closed back below the band after it (else the flip already
			// failed and this is sweep-reject territory); this bar dips into
			// the band and closes back above it.
			if p.Kind == signal.EQH {
				// A genuine BREAK is a CROSSING: the prior bar closed at/below
				// the band and this one closed above it. Requiring only
				// "some bar closed above" made the -window flag inert, because
				// the retest-and-hold condition already implies the previous bar
				// was above — the detector then degenerated into a plain
				// touch-the-band test, which is the proximity shape that does
				// not work here.
				brokeAt := -1
				for j := lo; j < i; j++ {
					if cs[j].Close > p.Hi && cs[j-1].Close <= p.Hi {
						brokeAt = j // keep the LATEST crossing in the window
					}
				}
				// FIRST retest only: if any bar between the crossing and now
				// already dipped into the band, this is a later test, not the
				// flip. And a close back below the band voids the flip outright.
				if brokeAt != -1 {
					for j := brokeAt + 1; j < i; j++ {
						if cs[j].Low <= p.Hi || cs[j].Close < p.Lo {
							brokeAt = -1
							break
						}
					}
				}
				if brokeAt != -1 && bar.Low <= p.Hi && bar.Close > p.Hi {
					stop := p.Lo - bufATR*a
					risk := bar.Close - stop
					if risk > 0 {
						out = append(out, autotrade.PaperFire{
							Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "flip", Side: "long",
							Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close + rMult*risk,
						})
						break // one setup per bar
					}
				}
			}

			// ── Broken EQL → the band should now act as RESISTANCE (short side).
			if p.Kind == signal.EQL {
				brokeAt := -1
				for j := lo; j < i; j++ {
					if cs[j].Close < p.Lo && cs[j-1].Close >= p.Lo {
						brokeAt = j
					}
				}
				if brokeAt != -1 {
					for j := brokeAt + 1; j < i; j++ {
						if cs[j].High >= p.Lo || cs[j].Close > p.Hi {
							brokeAt = -1
							break
						}
					}
				}
				if brokeAt != -1 && bar.High >= p.Lo && bar.Close < p.Lo {
					stop := p.Hi + bufATR*a
					risk := stop - bar.Close
					if risk > 0 {
						out = append(out, autotrade.PaperFire{
							Time: bar.CloseTime, Symbol: short, TF: "1h", Strategy: "flip", Side: "short",
							Market: true, Entry: bar.Close, Stop: stop, TP: bar.Close - rMult*risk,
						})
						break
					}
				}
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
	for i := range arr {
		if i+off >= 0 && i+off < n {
			out[i+off] = arr[i]
		}
	}
	return out
}
