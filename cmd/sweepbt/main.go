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
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

func main() {
	config.LoadDotEnv()
	days := flag.Int("days", 90, "history window in days")
	tfStr := flag.String("tf", "1h", "timeframe")
	tol := flag.Float64("tol", 0.15, "EQH/EQL cluster tolerance (%)")
	bufATR := flag.Float64("buf-atr", 0.15, "stop buffer beyond the sweep wick, in ATR(14)")
	rMult := flag.Float64("r", 2.0, "take-profit as R multiple of the stop distance")
	liqTP := flag.Bool("liq-tp", false, "A/B (a): TP at the nearest OPPOSITE liquidity pool (magnet) instead of fixed R; fall back to R if none / if it's a worse-than-1R target")
	// TESTED 2026-09-02 → REJECTED, kept off-by-default as documentation.
	// Worse in EVERY window on BOTH metrics: aggregate netR +58/+58/+78 →
	// +26/+34/+42 (60/90/120d) and R/trade +0.175/+0.113/+0.112 →
	// +0.110/+0.098/+0.099. On the live-wired subset (SOL/ETH/SUI) it cuts
	// netR 60-68% every window and flips ETH from +17/+19/+19 to -2/-3/-4.
	// WHY cmd/openbt suggested otherwise: its "mixed" bucket was a post-hoc
	// partition of ALREADY-DEDUPED positions. Gating at generation frees the
	// one-position slot, so different, later fires enter that were never in
	// that bucket (60d SOL: gated n=40 vs openbt mixed n=20). A bucket split
	// does not transfer to a strategy change under a one-position constraint.
	symbols := flag.String("symbols", "", "comma-separated symbols to test instead of the default universe; accepts short names (BTC) or raw contract codes (NCSKSPCX2USD-USDT)")
	openGate := flag.Bool("open-gate", false, "A/B (c): only fire when entry sits BETWEEN the daily and weekly open (the \"mixed\" bucket cmd/openbt found best). Suppresses fires beyond BOTH opens.")
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	syms := []struct {
		short string
		sym   market.Symbol
	}{{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT}, {"LINK", market.LINKUSDT}, {"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}}
	// --symbols vets candidates without touching the default universe. Codes
	// may be short names or raw contract codes, so an equity synthetic
	// (NCSKSPCX2USD-USDT) can be A/B'd before it is added anywhere.
	//
	// Resolution goes through market.Resolve, the canonical table. It used to
	// append "-USDT", which is right for the native perps and wrong for every
	// synthetic: `-symbols APP` became APP-USDT and 404'd when the contract is
	// NCSKAPP2USD-USDT, and XAU would have become XAU-USDT rather than
	// NCCOGOLD2USD-USDT — while this flag's own help text promised short names
	// worked. The reverse mapping was NCSK-specific too, so a metals code came
	// back labelled with its raw string.
	if strings.TrimSpace(*symbols) != "" {
		syms = syms[:0]
		for _, tok := range strings.Split(*symbols, ",") {
			tok = strings.ToUpper(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			sym, ok := market.Resolve(tok)
			if !ok {
				// Not a known short name, so treat it as a raw contract code:
				// the point of this flag is vetting an instrument BEFORE it
				// earns a roster entry.
				sym = market.Symbol(tok)
				if !strings.Contains(tok, "-USDT") && !strings.Contains(tok, "-USDC") {
					sym = market.Symbol(tok + "-USDT")
				}
			}
			short := market.Short(sym)
			if short == "" {
				short = string(sym)
			}
			syms = append(syms, struct {
				short string
				sym   market.Symbol
			}{short, sym})
		}
	}

	fmt.Printf("=== sweep-reject A/B · %dd · %s · tol %.2f%% · stop=sweep+%.2fATR · TP %.1fR ===\n", *days, *tfStr, *tol, *bufATR, *rMult)
	fmt.Printf("%-5s %6s %6s %5s %5s %7s %9s %8s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR", "R/trade", "trd/day")
	var aggR float64
	var aggN int

	for _, s := range syms {
		cs, err := client.KlinesRange(context.Background(), s.sym, tf, start, end)
		if err != nil || len(cs) < 80 {
			log.Printf("%s: klines %v (len %d)", s.short, err, len(cs))
			continue
		}
		atr := alignRight(indicator.ATR(cs, 14), len(cs))
		fires := genSweepFires(cs, atr, s.short, *tol/100.0, *bufATR, *rMult, *liqTP, *openGate)
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
		rpt, tpd := 0.0, 0.0
		if len(positions) > 0 {
			rpt = netR / float64(len(positions))
			tpd = float64(len(positions)) / float64(*days)
		}
		fmt.Printf("%-5s %6d %5.0f%% %5d %5d %6.0f%% %+9.2f %+8.3f %8.2f\n", s.short, len(positions), fp, tp, stop, win, netR, rpt, tpd)
		aggR += netR
		aggN += len(positions)
	}
	aggRPT, aggTPD := 0.0, 0.0
	if aggN > 0 {
		aggRPT = aggR / float64(aggN)
		aggTPD = float64(aggN) / float64(*days)
	}
	fmt.Printf("--- aggregate: %d positions, netR %+.2f, R/trade %+.3f, trd/day %.2f (open-gate=%v) ---\n", aggN, aggR, aggRPT, aggTPD, *openGate)
}

// genSweepFires walks closed bars; at each bar it detects a sweep-and-reject of a
// pre-existing EQH/EQL pool and emits a marketable fire at the reject close.
func genSweepFires(cs []market.Candle, atr []float64, short string, tolFrac, bufATR, rMult float64, liqTP, openGate bool) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac) // pools formed BEFORE this bar
		bar := cs[i]
		a := atr[i]
		above, below := signal.NearestLiquidity(pools, bar.Close)
		// A/B (c): the open-gate. Applied HERE, at generation, not as a
		// post-hoc filter on positions — in live the gate stops the ORDER, so
		// the one-position slot stays free and a later fire can take it. That
		// makes these numbers legitimately different from cmd/openbt's bucket
		// split (which partitioned already-deduped positions).
		// Opens are computed from cs[:i+1] so the boundary candle can never be
		// in the future.
		if openGate {
			o := signal.ComputeOpens(cs[:i+1], bar.CloseTime)
			if o.Daily == 0 || o.Weekly == 0 {
				continue // can't classify → don't trade it
			}
			px := bar.Close
			beyondBoth := (px > o.Daily && px > o.Weekly) || (px < o.Daily && px < o.Weekly)
			if beyondBoth {
				continue
			}
		}
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
