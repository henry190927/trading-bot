// Command shadowbt measures what the CLOSED-BAR entry rule costs.
//
// The engine evaluates closed bars only, deliberately (backtest parity —
// feedback-engine-closed-bar-only). So when price wicks into a key level and
// the bar closes back on the other side, the engine sees a reject-close and
// enters THERE, at a price well away from the level it was reacting to. The
// open question has been how much that costs: ETH 2415 and BTC 77831 both
// looked like good wick-reclaims the closed-bar entry priced badly.
//
// This is a MEASUREMENT, not a strategy. Nothing here is proposed for
// shipping, and it must not be read as "enter intra-bar instead" — see the
// upper-bound caveat below.
//
// Two arms over the same detected events:
//
//	CLOSE  enter at the reject bar's close      (what the engine actually does)
//	LEVEL  enter at the level price itself      (what a resting limit AT the
//	                                             level would have got)
//
// Same stop for both (beyond the wick + buffer), so each arm carries its own
// risk and TP is rMult of that risk. That is what R is for: a better entry
// shows up as less risk for the same structural stop.
//
// ── The caveat that decides how to read this ────────────────────────────────
//
// The LEVEL arm assumes a limit resting exactly at the level FILLED. That is
// an UPPER BOUND, not an achievable result: a wick that merely kisses the
// level may not fill a limit sitting on it, and this week four pullback limits
// missed by 3.15 / 0.55 / 1.17 / 169 points (feedback-entry-event-not-guess).
// So the run also reports wick DEPTH past the level, and --min-depth-atr
// restricts the LEVEL arm to events where the wick went far enough past that a
// limit plausibly filled. Read the restricted number, not the raw one.
//
// ── RESULT 2026-09-04: the closed-bar entry is NOT costing money ────────────
//
// It is buying stop-distance, and the naive comparison hides that.
//
// Run WITHOUT --same-tp (each arm targeting 2R of its OWN risk), the LEVEL arm
// looks like a large free win: aggregate +132R vs -187R, R/trade +0.048 vs
// -0.073, better on 6 of 8 symbols. That number is an artifact. Entering at
// the level puts the entry ~0.32 ATR from the stop where the close is ~0.62
// ATR from it, so "2R" means a target half as far away. It is a smaller,
// tighter trade aiming at a nearer target — not a better-priced version of the
// same trade.
//
// --same-tp holds the stop AND the target fixed so entry price is the only
// difference. The result reverses, and stays reversed:
//
//	window / subset             CLOSE R/trade   LEVEL R/trade    delta
//	60d                             -0.053         -0.061       -0.008
//	90d                             -0.073         -0.134       -0.061
//	120d                            -0.068         -0.104       -0.036
//	90d, wick >=0.25 ATR past       -0.038         -0.038        0.000
//	90d, session opens only         -0.092         -0.292       -0.200
//
// LEVEL is worse or equal in every configuration, never better. Win rate is
// the mechanism: 30% -> 17% on the 90d run. Half the distance to the stop is
// roughly twice the stop-outs for the same target. The min-depth 0.25 row
// confirms it — restrict to wicks that ran well past the level and the arms
// converge exactly, because a deep wick leaves the level entry almost as far
// from the stop as the close is.
//
// So the motivating hypothesis is refuted: "the closed-bar rule misses good
// wick-reclaims" is not visible across 60/90/120 days of eight symbols. ETH
// 2415 and BTC 77831 may well have been good individually; they do not
// generalise — the same shape as cmd/basebt's early-entry anecdote.
//
// TWO CAVEATS that bound how far this travels:
//
//  1. BOTH arms are NEGATIVE in every configuration. The detector fires ~28
//     trades/day across 8 symbols (sweepbt: ~0.1-0.2/day) because it treats any
//     bar poking through any of ~20 nearby levels as an event. It is a
//     measurement instrument, not a strategy, and nothing here should be wired
//     anywhere.
//  2. The arms do not take identical trade sets. DedupFires absorbs repeats
//     differently once entry prices differ, so the LEVEL arm resolves ~12%
//     fewer positions and carries a ~21% no-fill rate the CLOSE arm does not
//     have (a marketable close always fills). Compare R/trade, never netR —
//     the tool warns when the resolved counts diverge.
//
// Usage:
//
//	go run ./cmd/shadowbt --days 90 --same-tp        <- the meaningful run
//	go run ./cmd/shadowbt --days 90                     (naive, for contrast)
//	go run ./cmd/shadowbt --days 90 --same-tp --min-depth-atr 0.25
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"time"

	"myFirstGo/trading-bot/autotrade"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// levelHit is one wick-only interaction with a key level.
type levelHit struct {
	Bar      int
	Kind     string // "EQH" | "EQL" | "dailyOpen" | "weeklyOpen"
	Level    float64
	Side     string  // implied trade side: poke-above-and-reject = short
	Close    float64 // where the CLOSE arm enters
	Wick     float64 // the extreme reached (High for a short, Low for a long)
	DepthATR float64 // how far past the level the wick went, in ATR
	Slip     float64 // |Close - Level|, the price the closed-bar rule gives up
	SlipATR  float64
}

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	tol := flag.Float64("tol", 0.15, "EQH/EQL cluster tolerance (%)")
	bufATR := flag.Float64("buf-atr", 0.15, "stop buffer beyond the wick, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as an R multiple of each arm's own risk")
	minDepth := flag.Float64("min-depth-atr", 0, "restrict to events whose wick went at least this many ATR PAST the level — the subset where a resting limit plausibly filled. 0 = all events (upper bound)")
	opensOnly := flag.Bool("opens-only", false, "only session opens (daily/weekly), skipping EQH/EQL pools")
	poolsOnly := flag.Bool("pools-only", false, "only EQH/EQL pools, skipping session opens")
	sameTP := flag.Bool("same-tp", false, "CONTROL: give both arms the SAME absolute TP price (derived from the CLOSE arm), so entry price is the ONLY difference. Without this the LEVEL arm's risk is ~half the CLOSE arm's, its 2R target is correspondingly nearer, and it can win on hold-window reachability rather than entry quality.")
	hist := flag.Bool("hist", false, "print the wick-depth distribution — the fill-plausibility evidence")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"XAU", market.XAUUSDT}, {"XAG", market.XAGUSDT},
		{"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}, {"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}}

	scope := "opens+pools"
	if *opensOnly {
		scope = "opens only"
	} else if *poolsOnly {
		scope = "pools only"
	}
	tpMode := "own-risk"
	if *sameTP {
		tpMode = "SAME-TP control"
	}
	fmt.Printf("=== shadowbt · closed-bar cost · %dd · %s · %s · stop=wick+%.2fATR · TP %.1fR (%s) · min-depth %.2fATR ===\n",
		*days, *tfStr, scope, *bufATR, *rMult, tpMode, *minDepth)
	// fills / nofill printed for BOTH arms: the LEVEL arm is a resting limit
	// back at the level, so some of its entries never fill. Comparing netR
	// without that column would be comparing a full sample against a
	// survivor-selected one.
	fmt.Printf("%-5s %7s | %5s %5s %5s %8s | %5s %5s %5s %8s | %8s\n",
		"sym", "events", "cFill", "cNoF", "cWR%", "C R/trd", "lFill", "lNoF", "lWR%", "L R/trd", "dR/trd")

	var aggClose, aggLevel float64
	var aggN, aggLevelN, aggCloseUF, aggLevelUF, aggEvents int
	var allDepth, allSlip []float64

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		hits := detectHits(cs, atr, *tol/100.0, *opensOnly, *poolsOnly)

		// Filter to the plausibly-fillable subset before scoring, not after —
		// a post-hoc split of already-scored positions does not transfer
		// (feedback-bucket-split-not-ab).
		var kept []levelHit
		for _, h := range hits {
			if h.DepthATR >= *minDepth {
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 {
			fmt.Printf("%-5s %7d | %5s %5s %5s %8s | %5s %5s %5s %8s | %8s\n", s.short, 0, "—", "—", "—", "—", "—", "—", "—", "—", "—")
			continue
		}

		closeR, closeN, closeUF, closeW := score(cs, atr, kept, s.short, *bufATR, *rMult, false, *sameTP)
		levelR, levelN, levelUF, levelW := score(cs, atr, kept, s.short, *bufATR, *rMult, true, *sameTP)

		var depths, slips []float64
		for _, h := range kept {
			depths = append(depths, h.DepthATR)
			slips = append(slips, h.SlipATR)
		}
		allDepth = append(allDepth, depths...)
		allSlip = append(allSlip, slips...)

		cRPT, lRPT := 0.0, 0.0
		if closeN > 0 {
			cRPT = closeR / float64(closeN)
		}
		if levelN > 0 {
			lRPT = levelR / float64(levelN)
		}
		cWR, lWR := 0.0, 0.0
		if closeN > 0 {
			cWR = float64(closeW) / float64(closeN) * 100
		}
		if levelN > 0 {
			lWR = float64(levelW) / float64(levelN) * 100
		}
		fmt.Printf("%-5s %7d | %5d %5d %5.0f %+8.3f | %5d %5d %5.0f %+8.3f | %+8.3f\n",
			s.short, len(kept),
			closeN, closeUF, cWR, cRPT,
			levelN, levelUF, lWR, lRPT,
			lRPT-cRPT)

		aggClose += closeR
		aggLevel += levelR
		aggN += closeN
		aggLevelN += levelN
		aggCloseUF += closeUF
		aggLevelUF += levelUF
		aggEvents += len(kept)
	}

	aggCRPT, aggLRPT := 0.0, 0.0
	if aggN > 0 {
		aggCRPT = aggClose / float64(aggN)
		aggLRPT = aggLevel / float64(aggN)
	}
	aggLRPT2 := 0.0
	if aggLevelN > 0 {
		aggLRPT2 = aggLevel / float64(aggLevelN)
	}
	_ = aggLRPT
	fmt.Printf("--- aggregate: %d events\n", aggEvents)
	fmt.Printf("    CLOSE  filled %d  no-fill %d  netR %+.2f  R/trade %+.3f  trd/day %.2f\n",
		aggN, aggCloseUF, aggClose, aggCRPT, float64(aggN)/float64(*days))
	fmt.Printf("    LEVEL  filled %d  no-fill %d  netR %+.2f  R/trade %+.3f  trd/day %.2f\n",
		aggLevelN, aggLevelUF, aggLevel, aggLRPT2, float64(aggLevelN)/float64(*days))
	if aggN > 0 && aggLevelN < aggN {
		fmt.Printf("    ** the LEVEL arm resolved %d FEWER trades (%.0f%%): its unfilled entries are excluded from netR,\n",
			aggN-aggLevelN, float64(aggN-aggLevelN)/float64(aggN)*100)
		fmt.Printf("       so its netR is survivor-selected and NOT directly comparable to CLOSE's. Compare R/trade, not netR.\n")
	}
	if aggN > 0 {
		fmt.Printf("    median wick depth past level %.2f ATR · median closed-bar slip %.2f ATR\n",
			median(allDepth), median(allSlip))
	}
	if *minDepth == 0 {
		fmt.Println("    NOTE: min-depth 0 — the LEVEL arm is an UPPER BOUND (assumes a limit at the level always filled).")
		fmt.Println("          Re-run with --min-depth-atr 0.10 / 0.25 for the subset where a limit plausibly filled.")
	}
	if *hist {
		printHist("wick depth past level (ATR)", allDepth)
		printHist("closed-bar slip (ATR)", allSlip)
	}
}

// detectHits finds wick-only rejections of a key level: the bar traded through
// the level and CLOSED back on the other side, which is the shape the engine
// reacts to one close later and at a worse price.
//
// Levels come from cs[:i] (pools) and cs[:i+1] (opens, whose boundary candle is
// the bar itself) so nothing can be read from the future.
func detectHits(cs []market.Candle, atr []float64, tolFrac float64, opensOnly, poolsOnly bool) []levelHit {
	var out []levelHit
	for i := 60; i < len(cs); i++ {
		bar := cs[i]
		a := atr[i]
		if a <= 0 {
			continue
		}
		type lv struct {
			kind  string
			price float64
		}
		var levels []lv
		if !opensOnly {
			for _, p := range signal.FindLiquidity(cs[:i], 2, 20, tolFrac) {
				kind := "EQL"
				if p.Kind == signal.EQH {
					kind = "EQH"
				}
				levels = append(levels, lv{kind, p.Price})
			}
		}
		if !poolsOnly {
			o := signal.ComputeOpens(cs[:i+1], bar.CloseTime)
			if o.Daily > 0 {
				levels = append(levels, lv{"dailyOpen", o.Daily})
			}
			if o.Weekly > 0 {
				levels = append(levels, lv{"weeklyOpen", o.Weekly})
			}
		}
		for _, l := range levels {
			// Poked ABOVE and closed back below → rejected resistance → short.
			if bar.Open < l.price && bar.High > l.price && bar.Close < l.price {
				out = append(out, levelHit{
					Bar: i, Kind: l.kind, Level: l.price, Side: "short",
					Close: bar.Close, Wick: bar.High,
					DepthATR: (bar.High - l.price) / a,
					Slip:     l.price - bar.Close, SlipATR: (l.price - bar.Close) / a,
				})
			}
			// Poked BELOW and closed back above → reclaimed support → long.
			if bar.Open > l.price && bar.Low < l.price && bar.Close > l.price {
				out = append(out, levelHit{
					Bar: i, Kind: l.kind, Level: l.price, Side: "long",
					Close: bar.Close, Wick: bar.Low,
					DepthATR: (l.price - bar.Low) / a,
					Slip:     bar.Close - l.price, SlipATR: (bar.Close - l.price) / a,
				})
			}
		}
	}
	return out
}

// score turns hits into fires and runs them through the same EvaluateFire +
// one-position DedupFires the rest of the suite uses, so these numbers are
// comparable with sweepbt/rangebt rather than a private scale.
//
// atLevel picks the arm: false = enter at the bar close (the engine's actual
// behaviour), true = enter at the level.
func score(cs []market.Candle, atr []float64, hits []levelHit, short string, bufATR, rMult float64, atLevel, sameTP bool) (netR float64, resolved, unfilled, won int) {
	var fires []autotrade.PaperFire
	for _, h := range hits {
		a := atr[h.Bar]
		entry := h.Close
		if atLevel {
			entry = h.Level
		}
		var stop, tp float64
		if h.Side == "short" {
			stop = h.Wick + bufATR*a
			if stop <= entry {
				continue
			}
			tp = entry - rMult*(stop-entry)
			if sameTP {
				// The CLOSE arm's target, so both arms aim at the same price
				// and only the entry differs.
				tp = h.Close - rMult*(stop-h.Close)
			}
		} else {
			stop = h.Wick - bufATR*a
			if stop >= entry {
				continue
			}
			tp = entry + rMult*(entry-stop)
			if sameTP {
				tp = h.Close + rMult*(h.Close-stop)
			}
		}
		fires = append(fires, autotrade.PaperFire{
			Time: cs[h.Bar].CloseTime, Symbol: short, TF: "1h",
			Strategy: "shadow-" + h.Kind, Side: h.Side,
			Entry: entry, Stop: stop, TP: tp,
			Margin: 35, Lev: 125,
			Why: fmt.Sprintf("%s %s @ %.4f depth %.2fATR", h.Kind, h.Side, h.Level, h.DepthATR),
		})
	}
	positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.EvaluateFire(f, cs, 6)
	})
	n, uf, w := 0, 0, 0
	for _, p := range positions {
		switch p.Outcome.Status {
		case autotrade.OutTP:
			netR += p.Outcome.NetR
			n++
			w++
		case autotrade.OutStop:
			netR += p.Outcome.NetR
			n++
		case autotrade.OutNoFill, autotrade.OutPending:
			uf++
		}
	}
	return netR, n, uf, w
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return (c[m-1] + c[m]) / 2
}

// printHist shows where the mass actually sits, because a median alone cannot
// answer "would a limit have filled" — the tail is the whole question.
func printHist(label string, v []float64) {
	if len(v) == 0 {
		return
	}
	fmt.Printf("\n    %s — n=%d\n", label, len(v))
	buckets := []float64{0.05, 0.10, 0.25, 0.50, 1.00, math.Inf(1)}
	names := []string{"<0.05", "0.05-0.10", "0.10-0.25", "0.25-0.50", "0.50-1.00", ">1.00"}
	counts := make([]int, len(buckets))
	for _, x := range v {
		for i, b := range buckets {
			if x < b {
				counts[i]++
				break
			}
		}
	}
	for i, n := range counts {
		pct := float64(n) / float64(len(v)) * 100
		bar := ""
		for j := 0; j < int(pct/2); j++ {
			bar += "#"
		}
		fmt.Printf("      %-10s %4d  %5.1f%%  %s\n", names[i], n, pct, bar)
	}
}

func alignRight(arr []float64, n int) []float64 {
	if len(arr) >= n {
		return arr[len(arr)-n:]
	}
	out := make([]float64, n)
	copy(out[n-len(arr):], arr)
	return out
}
