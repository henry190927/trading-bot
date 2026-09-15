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
//	 V4 long-only / V5 short-only / V6 event-filter (added 2026-09-07)
//
// ---------------------------------------------------------------------------
// SIDE + EVENT-FILTER ARMS RUN AND REJECTED 2026-09-07. Ship nothing; V0 wins.
//
//	arm              60d      90d     120d   all>0   beat V0
//	V0-baseline    +3.04   +20.27   +20.96     yes      0/3
//	V1-mom        -24.42   -29.20   -12.59      no      0/3
//	V2-vol        -10.23    -0.17    -9.88      no      0/3
//	V3-both       -24.42   -28.90   -14.05      no      0/3
//	V4-long-only  +12.32   +20.64   -26.95      no      2/3
//	V5-short-only -14.67   -17.85   +10.47      no      0/3
//	V6-event-filt -11.16   -25.02   -19.34      no      0/3
//
// cmd/gate: V0 AS SHIPPED **PASSES** the absolute floor (medR/t +0.084,
// min-n 122). long-only FAILS on 120d. event-filter FAILS all three windows.
//
// WHAT PROMPTED THE RUN, AND WHY IT WAS A MISREAD. The forward paper log
// showed range-edge at -0.384 R/trade over 13 trades, with all 5 shorts
// losing -1.00R each. That looked like "A/B passed, forward failing". It was
// not: against the 120d backtest's +0.084 R/trade, a 13-trade sample has a
// standard error of ~0.333, so -0.384 is 1.41 SE away — squarely inside
// noise. It would take ~25 trades for that gap to mean anything. Five losing
// shorts in a row is unremarkable in a +/-1R system.
//
// The 120d window says the opposite of the forward log outright: short-only
// is +10.47 and long-only is -26.95. The sides swap sign between windows,
// which is what a 13-trade read cannot see.
//
// AND THE PROPOSED FIX WAS WORSE. Diagnosis of the mechanism was correct —
// the shipped guard tests `Trend != StructDowntrend`, and Trend reads
// `neutral` on 77-87% of bars (all 13 live fires had neutral), so it has
// never blocked anything, while Event fires on 25-37% and two losing shorts
// were taken on a CHoCH-up. Adding the Event check (V6) still lost in all
// three windows. That is the THIRD directional filter to fail on this
// mean-reversion box-fade, after the momentum filter and vol gate of
// 2026-08-27. The standing conclusion from that day holds: MR box-fade and a
// directional filter are contradictory, because MR wants to buy the drop.
//
// A true observation about the code does not imply the change it suggests.
// ---------------------------------------------------------------------------
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

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

type variant struct {
	name      string
	momFilter bool
	volGate   bool

	// noLong / noShort disable a SIDE entirely. Added 2026-09-07 after the
	// forward log showed range-edge's loss is all on the short side: 5 shorts,
	// 5 x -1.00R, against 8 longs netting +0.01R.
	//
	// short-only is included on purpose rather than inferred. Under
	// one-position-per-rule dedup, removing shorts FREES slots for longs, so
	// long-only is NOT baseline minus short-only — the arms interact and each
	// has to be run.
	noLong  bool
	noShort bool

	// eventFilter also blocks a side when the latest structural EVENT points
	// against it (CHoCH-up / BOS-up kills a short, and the mirror for longs).
	//
	// The shipped filter tests Trend only, and Trend reads `neutral` on 77-87%
	// of bars — all 13 live range-edge fires had Trend == neutral, so it has
	// never blocked anything. Event fires on 25-37% of bars, and two of the
	// five losing shorts were taken on a CHoCH-up.
	eventFilter bool
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 60, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	stopPct := flag.Float64("stop-pct", 0.5, "stop buffer beyond box edge (%)")
	volThresh := flag.Float64("vol-thresh", 1.2, "ATR%% ceiling for the vol gate")
	concTest := flag.Bool("conc-test", false, "A/B max-concurrent 1 vs 2 vs 3 on baseline (relax one-position)")
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
		{name: "V0-baseline"},
		{name: "V1-mom", momFilter: true},
		{name: "V2-vol", volGate: true},
		{name: "V3-both", momFilter: true, volGate: true},
		{name: "V4-long-only", noShort: true},
		{name: "V5-short-only", noLong: true},
		{name: "V6-event-filt", eventFilter: true},
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

		if *concTest {
			base := genFires(cs, ema, atr, variant{name: "V0"}, *stopPct/100.0, *volThresh/100.0, s.short)
			for _, n := range []int{1, 2, 3} {
				sum := summarize(scoreConcurrent(base, n, cs))
				fmt.Printf("%-5s conc=%-2d %6d %5.0f%% %5d %5d %6.0f%% %+7.2f\n",
					s.short, n, sum.total, sum.fillPct*100, sum.tp, sum.stop, sum.winPct*100, sum.netR)
			}
			fmt.Println()
			continue
		}
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

		if pos <= 0.34 && st.Trend != signal.StructDowntrend &&
			!v.noLong && !(v.eventFilter && bearishEvent(st.Event)) {
			if !v.momFilter || emaUp {
				out = append(out, autotrade.PaperFire{
					Time: cs[i].CloseTime, Symbol: short, TF: "1h", Strategy: "range-edge", Side: "long",
					Market: true, Entry: px, Stop: lo * (1 - stopBuf), TP: hi,
				})
				continue
			}
		}
		if pos >= 0.66 && st.Trend != signal.StructUptrend &&
			!v.noShort && !(v.eventFilter && bullishEvent(st.Event)) {
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

// scoreConcurrent allows up to maxN open positions per rule at once (vs DedupFires'
// strict 1) to A/B whether relaxing the one-position rule helps. Fires oldest-first.
// A new fire is taken if fewer than maxN positions are still open at its time.
func scoreConcurrent(fires []autotrade.PaperFire, maxN int, cs []market.Candle) []autotrade.Position {
	type openPos struct{ exit time.Time }
	var open []openPos
	var out []autotrade.Position
	for _, f := range fires {
		// purge resolved
		live := open[:0]
		for _, o := range open {
			if o.exit.After(f.Time) {
				live = append(live, o)
			}
		}
		open = live
		if len(open) >= maxN {
			continue
		}
		oc := autotrade.EvaluateFire(f, cs, 6)
		out = append(out, autotrade.Position{Fire: f, Outcome: oc})
		exit := f.Time.Add(1_000_000 * time.Hour) // open/no-fill hold the slot ~forever
		switch oc.Status {
		case autotrade.OutTP, autotrade.OutStop:
			exit = oc.ExitAt
		case autotrade.OutNoFill:
			exit = f.Time.Add(6 * time.Hour)
		}
		open = append(open, openPos{exit})
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

// bullishEvent / bearishEvent name the structural shifts that argue against
// fading an edge. Kept as functions rather than inlined so the two sides
// cannot drift apart — an asymmetric filter would be indistinguishable from a
// directional bias in the results.
func bullishEvent(e signal.StructEventKind) bool {
	return e == signal.EvCHoCHUp || e == signal.EvBOSUp
}

func bearishEvent(e signal.StructEventKind) bool {
	return e == signal.EvCHoCHDown || e == signal.EvBOSDown
}
