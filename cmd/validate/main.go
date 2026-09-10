// Command validate scores a user-proposed trade against the engine state
// and HVN structure. Use when you want to know "should I take this entry?"
// rather than waiting for the engine to fire its own signal.
//
// Scoring lives in trading-bot/validator (shared with the /validate web form);
// this binary only handles CLI args and terminal rendering.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"myFirstGo/trading-bot/analyzer"
	"myFirstGo/trading-bot/ansi"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/earnings"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/session"
	"myFirstGo/trading-bot/signal"
	"myFirstGo/trading-bot/validator"
)

func main() {
	config.LoadDotEnv()
	symbolFlag := flag.String("symbol", "", "BTC, ETH, XAU, XAG (or full BingX name)")
	sideFlag := flag.String("side", "", "long or short")
	entryFlag := flag.Float64("entry", 0, "proposed entry price")
	tfFlag := flag.String("tf", "1h", "timeframe (1m, 5m, 15m, 1h, 4h, 1d)")
	feeFlag := flag.Float64("fee-bps", 6, "round-trip fee in basis points for fee_R calc")
	flag.Parse()

	if *symbolFlag == "" || *sideFlag == "" || *entryFlag <= 0 {
		fmt.Fprintln(os.Stderr, "usage: validate -symbol=BTC -side=long -entry=74500 -tf=1h")
		flag.PrintDefaults()
		os.Exit(2)
	}

	sym, err := resolveSymbol(*symbolFlag)
	if err != nil {
		log.Fatal(err)
	}
	var side signal.Side
	switch strings.ToLower(*sideFlag) {
	case "long", "l":
		side = signal.Long
	case "short", "s":
		side = signal.Short
	default:
		log.Fatalf("side must be long or short, got %q", *sideFlag)
	}
	tf := market.Timeframe(*tfFlag)

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candles, err := client.Klines(ctx, sym, tf, 300)
	if err != nil {
		log.Fatalf("fetch klines: %v", err)
	}
	if len(candles) < 60 {
		log.Fatalf("only %d candles, need 60+", len(candles))
	}

	// Live mark for engine's plan-validity check + validator's chase math.
	var markPrice float64
	if fr, err := client.FundingRate(ctx, sym); err == nil {
		markPrice = fr.MarkPrice
	}

	res := validator.Validate(sym, tf, side, *entryFlag, *feeFlag, candles, markPrice)

	// Cash-open-bar check, PRE-TRADE. The same advisory exists in the web app,
	// where BOTH call sites are post-hoc (the place-stop response and
	// /ops/verify). Journal #67 is why that is not enough: a SNDK short with a
	// stop 1/7.4 of that symbol's median cash-open-bar range, filled 7 seconds
	// into the open and stopped 24 seconds later. A warning that arrives after
	// the fill cannot be acted on.
	//
	// Checked against SuggStop because this command takes no stop of its own.
	// session.OpenBarLookbackBars rather than a local 400 so this and the web
	// guard cannot report different medians for the same symbol.
	sessWarn := ""
	if earnings.IsStockSymbol(string(sym)) {
		if cs1h, err := client.Klines(ctx, sym, market.TF1h, session.OpenBarLookbackBars); err == nil {
			if pct, n := session.MedianOpenBarRangePct(cs1h); pct > 0 {
				now := time.Now()
				sessWarn = session.StopWarning(*entryFlag, res.SuggStop, pct, n,
					session.NextCashOpen(now).Sub(now))
			}
		}
	}
	print(res, sessWarn)
}

func print(r validator.Result, sessWarn string) {
	bar := strings.Repeat("=", 70)
	fmt.Printf("\n%s\n", ansi.Wrap(bar, ansi.BoldC))
	header := fmt.Sprintf("Validation: %s %s @ %.4f (%s)", r.Symbol, colorSide(r.Side), r.Entry, r.Timeframe)
	fmt.Println(header)
	fmt.Println(ansi.Wrap(bar, ansi.BoldC))

	mkt := (r.Entry - r.Price) / r.Price * 100
	fmt.Printf("Current market: %.4f  (your entry %+.2f%% vs market)\n", r.Price, mkt)
	fmt.Printf("Engine state:   %s, score %s\n", colorSide(r.EngineSide), colorScore(r.EngineScore))
	if len(r.Reasons) > 0 {
		fmt.Println("Engine reasons:")
		for _, x := range r.Reasons {
			fmt.Printf("  %s %s\n", ansi.Wrap("+", ansi.Green), ansi.Wrap(x, ansi.Green))
		}
	}
	if len(r.Notes) > 0 {
		fmt.Println("Engine notes (display-only, no vote):")
		for _, n := range r.Notes {
			fmt.Printf("  %s %s\n", ansi.Wrap("·", ansi.Cyan), ansi.Wrap(n, ansi.Cyan))
		}
	}

	if r.EnginePlan.Entry != 0 {
		fmt.Println("\n" + ansi.Wrap("Engine's own plan (compare with yours):", ansi.Bold))
		p := r.EnginePlan
		fmt.Printf("  side   %s  (anchor: %s)\n", colorSide(r.EngineSide), p.Anchor)
		fmt.Printf("  order  %s\n", ansi.Wrap(p.OrderType.String(), ansi.Bold))
		fmt.Printf("  entry  %s   (your entry: %.4f → diff %+.2f%%)\n",
			ansi.Wrap(fmt.Sprintf("%.4f", p.Entry), ansi.BoldG),
			r.Entry, (r.Entry-p.Entry)/p.Entry*100)
		fmt.Printf("  stop   %s   (risk %.4f)\n",
			ansi.Wrap(fmt.Sprintf("%.4f", p.StopLoss), ansi.Red), p.Risk())
		if len(p.TakeProfit) >= 1 {
			fmt.Printf("  TP1    %s   (1R)\n",
				ansi.Wrap(fmt.Sprintf("%.4f", p.TakeProfit[0]), ansi.Green))
		}
		if len(p.TakeProfit) >= 2 {
			fmt.Printf("  TP2    %s   (2R)\n",
				ansi.Wrap(fmt.Sprintf("%.4f", p.TakeProfit[1]), ansi.Green))
		}
	}

	fmt.Println("\n" + ansi.Wrap("Entry-level proximity:", ansi.Bold))
	if r.NearestSweep != nil {
		d := (r.NearestSweep.Level - r.Entry) / r.Entry * 100
		side := "high"
		if r.NearestSweep.Side == analyzer.SweepLow {
			side = "low"
		}
		fmt.Printf("  • Nearest sweep: %s @ %.4f (%+.2f%% from entry)\n", side, r.NearestSweep.Level, d)
	} else {
		fmt.Println("  • No recent sweeps detected")
	}
	if r.NearestFib != nil {
		d := (r.NearestFib.Price - r.Entry) / r.Entry * 100
		fmt.Printf("  • Nearest fib %.3f: %.4f (%+.2f%% from entry)\n", r.NearestFib.Ratio, r.NearestFib.Price, d)
	}
	if r.BollLower != 0 {
		dl := (r.BollLower - r.Entry) / r.Entry * 100
		du := (r.BollUpper - r.Entry) / r.Entry * 100
		fmt.Printf("  • BOLL lower: %.4f (%+.2f%%)   upper: %.4f (%+.2f%%)\n", r.BollLower, dl, r.BollUpper, du)
	}

	if r.VP.POC != 0 {
		fmt.Println("\n" + ansi.Wrap("HVN context (籌碼密集區):", ansi.Bold))
		d := (r.VP.POC - r.Entry) / r.Entry * 100
		fmt.Printf("  • POC @ %s (%+.2f%% from entry)\n", ansi.Wrap(fmt.Sprintf("%.4f", r.VP.POC), ansi.BoldC), d)
		strs := make([]string, len(r.VP.HVN))
		for i, h := range r.VP.HVN {
			strs[i] = fmt.Sprintf("%.4f", h)
		}
		fmt.Printf("  • Top HVNs: %s\n", strings.Join(strs, ", "))
		fmt.Printf("  • HVNs above entry: %d   below: %d\n", r.HVNsAbove, r.HVNsBelow)
	}

	fmt.Println("\n" + ansi.Wrap("Suggested levels (1.5×ATR risk):", ansi.Bold))
	fmt.Printf("  Stop:  %s   (risk %.4f)\n", ansi.Wrap(fmt.Sprintf("%.4f", r.SuggStop), ansi.Red), r.Risk)
	fmt.Printf("  TP1:   %s   (1R)\n", ansi.Wrap(fmt.Sprintf("%.4f", r.SuggTP1), ansi.Green))
	fmt.Printf("  TP2:   %s   (2R)\n", ansi.Wrap(fmt.Sprintf("%.4f", r.SuggTP2), ansi.Green))
	fmt.Printf("  Fee:   %s per trade (round-trip)\n", colorFee(r.FeeR))
	if sessWarn != "" {
		fmt.Printf("  %s\n", ansi.Wrap("⚠ "+sessWarn, ansi.Yellow))
	}

	fmt.Println("\n" + ansi.Wrap("Factors:", ansi.Bold))
	for _, f := range r.Factors {
		sign := "+"
		if f.Points < 0 {
			sign = ""
		}
		line := fmt.Sprintf("  %s%.1f  %s — %s", sign, f.Points, f.Name, f.Detail)
		switch {
		case f.Points > 0:
			fmt.Println(ansi.Wrap(line, ansi.Green))
		case f.Points < 0:
			fmt.Println(ansi.Wrap(line, ansi.Red))
		default:
			fmt.Println(ansi.Wrap(line, ansi.Dim))
		}
	}

	fmt.Printf("\n%s %s\n", ansi.Wrap("Total score:", ansi.Bold), colorScoreOutOf10(r.Total))
	fmt.Printf("%s     %s\n", ansi.Wrap("Verdict:", ansi.Bold), colorVerdict(r.Verdict))
	fmt.Println(ansi.Wrap(bar, ansi.BoldC))
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

func colorScore(score int) string {
	str := fmt.Sprintf("%d", score)
	switch {
	case score >= 3:
		return ansi.Wrap(str, ansi.BoldG)
	case score == 2:
		return ansi.Wrap(str, ansi.Yellow)
	default:
		return ansi.Wrap(str, ansi.Dim)
	}
}

func colorScoreOutOf10(score float64) string {
	str := fmt.Sprintf("%.1f / 10", score)
	switch {
	case score >= 8:
		return ansi.Wrap(str, ansi.BoldG)
	case score >= 6:
		return ansi.Wrap(str, ansi.Green)
	case score >= 4:
		return ansi.Wrap(str, ansi.Yellow)
	case score >= 2:
		return ansi.Wrap(str, ansi.Yellow)
	}
	return ansi.Wrap(str, ansi.BoldR)
}

func colorVerdict(v string) string {
	switch {
	case strings.HasPrefix(v, "STRONG"):
		return ansi.Wrap(v, ansi.BoldG)
	case strings.HasPrefix(v, "TAKE"):
		return ansi.Wrap(v, ansi.Green)
	case strings.HasPrefix(v, "NEUTRAL"):
		return ansi.Wrap(v, ansi.Yellow)
	case strings.HasPrefix(v, "WEAK"):
		return ansi.Wrap(v, ansi.Yellow)
	case strings.HasPrefix(v, "AVOID"):
		return ansi.Wrap(v, ansi.BoldR)
	}
	return v
}

func colorFee(feeR float64) string {
	str := fmt.Sprintf("%.2fR", feeR)
	switch {
	case feeR < 0.15:
		return ansi.Wrap(str, ansi.Green)
	case feeR < 0.30:
		return ansi.Wrap(str, ansi.Yellow)
	case feeR < 0.50:
		return ansi.Wrap(str, ansi.Yellow)
	}
	return ansi.Wrap(str, ansi.BoldR)
}

// resolveSymbol delegates to market.ResolveErr rather than keeping a local
// switch.
//
// It WAS a local switch over BTC/ETH/XAU/XAG only — the fourth copy of this
// map, and the one market/resolve.go's own comment did not know about. The
// consequence was not cosmetic: the pre-trade validator REFUSED every stock
// synthetic and every alt, so the one surface that scores a proposed entry
// could not be pointed at SNDK/NVDA/SPCX/MSTR/APP/SOL/SUI/LINK/HYPE/NEAR at
// all. A SNDK short was placed on 2026-09-08 with a stop 1/7.4 of that
// symbol's median cash-open-bar range; `validate -symbol SNDK` would have
// answered "unknown symbol" rather than scoring it.
//
// The aliases the switch carried beyond the short names (GOLD, SILVER,
// BTCUSDT, …) are kept: market.Resolve already accepts the short names, and
// the "-USDT" passthrough still lets a raw contract code through for a symbol
// with no short name (BRENT).
func resolveSymbol(s string) (market.Symbol, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "GOLD":
		return market.XAUUSDT, nil
	case "SILVER":
		return market.XAGUSDT, nil
	}
	if sym, ok := market.Resolve(s); ok {
		return sym, nil
	}
	u := strings.ToUpper(strings.TrimSpace(s))
	if strings.Contains(u, "-USDT") {
		return market.Symbol(s), nil
	}
	// "BTCUSDT" style: no dash, so it is not a contract code — strip the quote
	// asset and try the short name. The old switch spelled these out per
	// symbol; deriving it keeps the alias working for every short name
	// market.Resolve knows, not just the four that were hardcoded.
	if short := strings.TrimSuffix(u, "USDT"); short != u && short != "" {
		if sym, ok := market.Resolve(short); ok {
			return sym, nil
		}
	}
	return market.ResolveErr(s)
}
