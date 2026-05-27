package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"myFirstGo/trading/ansi"
	"myFirstGo/trading/bingx"
	"myFirstGo/trading/config"
	"myFirstGo/trading/market"
	"myFirstGo/trading/signal"
)

func main() {
	config.LoadDotEnv()
	tf := flag.String("tf", "1h", "timeframe (1m, 5m, 15m, 1h, 4h, 1d)")
	biasTfFlag := flag.String("bias-tf", "", "higher timeframe for MTF bias filter (empty = auto)")
	useBias := flag.Bool("bias", false, "enable MTF bias filter (disabled by default — backtest shows it hurts this mean-reversion strategy)")
	minScore := flag.Int("min-score", 3, "score considered tradeable in the summary")
	flag.Parse()

	timeframe := market.Timeframe(*tf)
	biasTF := signal.DefaultBiasTF(timeframe)
	if *biasTfFlag != "" {
		biasTF = market.Timeframe(*biasTfFlag)
	}
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if *useBias {
		log.Printf("MTF bias filter enabled: %s base, %s bias", timeframe, biasTF)
	}

	var results []symbolResult
	for _, sym := range market.All() {
		candles, err := client.Klines(ctx, sym, timeframe, 300)
		if err != nil {
			log.Printf("%s: klines failed: %v", sym, err)
			continue
		}
		bias := signal.Flat
		if *useBias {
			biasCandles, err := client.Klines(ctx, sym, biasTF, 100)
			if err != nil {
				log.Printf("%s: bias klines failed: %v", sym, err)
			} else {
				bias = signal.Bias(biasCandles)
			}
		}
		sigCtx := signal.Context{}
		if fr, err := client.FundingRate(ctx, sym); err == nil {
			sigCtx.FundingRate = fr.Rate
		}
		if oi, err := client.OpenInterest(ctx, sym); err == nil {
			sigCtx.OpenInterest = oi
		}
		s := signal.Evaluate(signal.Inputs{
			Symbol: sym, Timeframe: timeframe, Candles: candles, Ctx: sigCtx, Bias: bias,
		})
		results = append(results, symbolResult{s, sigCtx, bias})
		printDetails(s, sigCtx, bias)
	}
	printSummary(results, *minScore)
}

type symbolResult struct {
	Signal signal.Signal
	Ctx    signal.Context
	Bias   signal.Side
}

func printDetails(s signal.Signal, ctx signal.Context, bias signal.Side) {
	header := fmt.Sprintf("=== %s %s @ %.4f ===", s.Symbol, s.Timeframe, s.Price)
	fmt.Printf("\n%s\n", ansi.Wrap(header, ansi.BoldC))
	fmt.Printf("Side: %s   Score: %s   Bias: %s   Funding: %.4f%%   OI: %.0f\n",
		colorSide(s.Side), colorScore(s.Score, 3), colorSide(bias),
		ctx.FundingRate*100, ctx.OpenInterest)
	if s.VP.POC != 0 {
		hvnStr := make([]string, 0, len(s.VP.HVN))
		for _, h := range s.VP.HVN {
			hvnStr = append(hvnStr, fmt.Sprintf("%.4f", h))
		}
		fmt.Printf("HVN (籌碼密集區): POC=%s   nodes=[%s]\n",
			ansi.Wrap(fmt.Sprintf("%.4f", s.VP.POC), ansi.BoldC),
			strings.Join(hvnStr, ", "))
	}
	if s.Opens.Daily != 0 || s.Opens.Weekly != 0 {
		fmt.Printf("Opens: daily=%s (%+.2f%%)  weekly=%s (%+.2f%%)  monthly=%s (%+.2f%%)\n",
			fmtOpen(s.Opens.Daily), pctDiff(s.Price, s.Opens.Daily),
			fmtOpen(s.Opens.Weekly), pctDiff(s.Price, s.Opens.Weekly),
			fmtOpen(s.Opens.Monthly), pctDiff(s.Price, s.Opens.Monthly))
	}
	for _, r := range s.Reasons {
		fmt.Printf("  %s %s\n", ansi.Wrap("+", ansi.Green), ansi.Wrap(r, ansi.Green))
	}
	for _, n := range s.Notes {
		fmt.Printf("  %s %s\n", ansi.Wrap("·", ansi.Cyan), ansi.Wrap(n, ansi.Cyan))
	}
	for _, w := range s.Warnings {
		fmt.Printf("  %s %s\n", ansi.Wrap("!", ansi.Yellow), ansi.Wrap(w, ansi.Yellow))
	}
	if s.Plan.Entry != 0 {
		p := s.Plan
		fmt.Printf("  %s %s %s  entry=%s  stop=%s  risk=%.4f  anchor=%s\n",
			ansi.Wrap(">>", ansi.Bold), ansi.Wrap(p.OrderType.String(), ansi.Bold), colorSide(s.Side),
			ansi.Wrap(fmt.Sprintf("%.4f", p.Entry), ansi.BoldG),
			ansi.Wrap(fmt.Sprintf("%.4f", p.StopLoss), ansi.Red),
			p.Risk(), p.Anchor)
		for i, tp := range p.TakeProfit {
			fmt.Printf("     TP%d=%s (%.1fR)\n",
				i+1, ansi.Wrap(fmt.Sprintf("%.4f", tp), ansi.Green), p.RR[i])
		}
		if p.Note != "" {
			fmt.Printf("     %s %s\n", ansi.Wrap("note:", ansi.Yellow), ansi.Wrap(p.Note, ansi.Yellow))
		}
	}
}

func fmtOpen(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4f", v)
}

func pctDiff(price, ref float64) float64 {
	if ref == 0 {
		return 0
	}
	return (price - ref) / ref * 100
}

func colorSide(s signal.Side) string {
	switch s {
	case signal.Long:
		return ansi.Wrap("LONG", ansi.BoldG)
	case signal.Short:
		return ansi.Wrap("SHORT", ansi.BoldR)
	default:
		return ansi.Wrap("FLAT", ansi.Dim)
	}
}

func colorScore(score, threshold int) string {
	str := fmt.Sprintf("%d", score)
	switch {
	case score >= threshold:
		return ansi.Wrap(str, ansi.BoldG)
	case score == threshold-1:
		return ansi.Wrap(str, ansi.Yellow)
	default:
		return ansi.Wrap(str, ansi.Dim)
	}
}

func colorVerdict(v string) string {
	switch {
	case strings.HasPrefix(v, "TRADEABLE"):
		return ansi.Wrap(v, ansi.BoldG)
	case strings.HasPrefix(v, "tradeable"):
		return ansi.Wrap(v, ansi.Yellow)
	case strings.HasPrefix(v, "borderline"):
		return ansi.Wrap(v, ansi.Yellow)
	case strings.HasPrefix(v, "skip"):
		return ansi.Wrap(v, ansi.Dim)
	}
	return v
}

func printSummary(results []symbolResult, minScore int) {
	header := fmt.Sprintf("=== Summary (threshold ≥ %d) ===", minScore)
	fmt.Printf("\n\n%s\n", ansi.Wrap(header, ansi.BoldC))
	hdr := fmt.Sprintf("%-22s %-5s %-3s %-5s %-12s %-12s %-12s %-12s %s",
		"SYMBOL", "SIDE", "SCR", "BIAS", "ENTRY", "STOP", "TP1", "TP2", "VERDICT")
	fmt.Println(ansi.Wrap(hdr, ansi.Bold))
	fmt.Println(strings.Repeat("-", 125))
	for _, r := range results {
		s := r.Signal
		entry, stop, tp1, tp2 := "-", "-", "-", "-"
		if s.Plan.Entry != 0 {
			entry = fmt.Sprintf("%.4f", s.Plan.Entry)
			stop = fmt.Sprintf("%.4f", s.Plan.StopLoss)
			if len(s.Plan.TakeProfit) >= 1 {
				tp1 = fmt.Sprintf("%.4f", s.Plan.TakeProfit[0])
			}
			if len(s.Plan.TakeProfit) >= 2 {
				tp2 = fmt.Sprintf("%.4f", s.Plan.TakeProfit[1])
			}
		}
		fmt.Printf("%-22s %s %s %s %-12s %-12s %-12s %-12s %s\n",
			s.Symbol,
			ansi.PadR(colorSide(s.Side), 5),
			ansi.PadR(colorScore(s.Score, minScore), 3),
			ansi.PadR(colorSide(r.Bias), 5),
			entry, stop, tp1, tp2,
			colorVerdict(verdict(s, minScore)))
	}
}

func verdict(s signal.Signal, minScore int) string {
	if s.Side == signal.Flat {
		return "skip — no direction"
	}
	if s.Score < minScore-1 {
		return fmt.Sprintf("skip — score %d < %d", s.Score, minScore)
	}
	if s.Score == minScore-1 {
		return "borderline — watch for one more confluence"
	}
	if s.Plan.IsSweepAnchored() {
		return "TRADEABLE — sweep-anchored ✓"
	}
	return "tradeable but anchor isn't a sweep — size down"
}
