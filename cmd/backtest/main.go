package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"myFirstGo/trading/backtest"
	"myFirstGo/trading/bingx"
	"myFirstGo/trading/config"
	"myFirstGo/trading/dxy"
	"myFirstGo/trading/market"
	"myFirstGo/trading/signal"
)

func main() {
	config.LoadDotEnv()
	tf := flag.String("tf", "1h", "timeframe (1m, 5m, 15m, 1h, 4h, 1d)")
	biasTfFlag := flag.String("bias-tf", "", "higher timeframe for MTF bias filter (empty = auto)")
	useBias := flag.Bool("bias", false, "enable MTF bias filter (off by default — backtest shows it hurts mean-reversion edge)")
	days := flag.Int("days", 60, "history window in days")
	threshold := flag.Int("min-score", 3, "minimum confluence score to take a trade")
	maxHold := flag.Int("hold", 24, "max bars to hold before timing out")
	feeBps := flag.Float64("fee-bps", 6, "round-trip fee in basis points (BingX maker 4, mixed 6, taker 10)")
	sweepOnly := flag.Bool("sweep-only", false, "skip trades whose entry isn't anchored to a liquidity sweep")
	stopRefine := flag.Bool("stop-refine", false, "enable widening stops past obstacles (HVN/equal levels). default off — backtest shows it hurts net R within 24-bar hold")
	useDXY := flag.Bool("dxy", false, "enable DXY macro veto on XAU/XAG signals. Default OFF — 2026-05-27 backtest showed it hurt by ~46R (vetoed trades were the best ones; mean-reversion thrives on macro-divergent dips)")
	verbose := flag.Bool("v", false, "print every trade")
	flag.Parse()

	if *stopRefine {
		signal.StopRefineEnabled = true
	}

	timeframe := market.Timeframe(*tf)
	biasTF := signal.DefaultBiasTF(timeframe)
	if *biasTfFlag != "" {
		biasTF = market.Timeframe(*biasTfFlag)
	}

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	end := time.Now()
	start := end.AddDate(0, 0, -*days)

	opts := backtest.Options{
		FeeBpsRoundTrip: *feeBps,
		SweepOnly:       *sweepOnly,
	}

	// DXY macro veto — only useful for XAU/XAG; the engine ignores it for
	// other symbols. Yahoo lookback "6mo" comfortably covers a 60d backtest.
	if *useDXY {
		dxyRange := "6mo"
		if *days > 120 {
			dxyRange = "1y"
		}
		dxyCandles, err := dxy.Fetch(ctx, "1h", dxyRange)
		if err != nil {
			log.Printf("DXY fetch failed (continuing without veto): %v", err)
		} else {
			log.Printf("DXY: fetched %d 1h candles, current trend %s", len(dxyCandles), dxy.Classify(dxyCandles))
			opts.DXYCandles = dxyCandles
		}
	}

	for _, sym := range market.All() {
		candles, err := client.KlinesRange(ctx, sym, timeframe, start, end)
		if err != nil {
			log.Printf("%s: history fetch failed: %v", sym, err)
			continue
		}
		if len(candles) < 200 {
			log.Printf("%s: only %d bars fetched, skipping", sym, len(candles))
			continue
		}
		var biasCandles []market.Candle
		if *useBias {
			biasCandles, err = client.KlinesRange(ctx, sym, biasTF, start, end)
			if err != nil {
				log.Printf("%s: bias history fetch failed: %v", sym, err)
				biasCandles = nil
			}
		}
		res := backtest.Run(sym, timeframe, candles, biasCandles, *threshold, *maxHold, opts)
		fmt.Println(res.Summary())
		if *verbose {
			for _, t := range res.Trades {
				fmt.Printf("  %s %s sc=%d entry=%.4f exit=%.4f gross=%+.2f fee=%.2f net=%+.2f (%s, %s)\n",
					t.SignaledAt.Format("2006-01-02 15:04"), t.Side, t.Score,
					t.Entry, t.Exit, t.RGross, t.FeeR, t.R, t.Outcome, t.Anchor)
			}
		}
	}
}
