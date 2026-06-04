package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"myFirstGo/trading-bot/backtest"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/dxy"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
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
	bodyWeight := flag.Float64("body-weight", 0, "POC/HVN body-weighted distribution: fraction (0,1) of each candle's volume routed to its body range. Default 0 = legacy uniform-over-HL. Try 0.7 to damp wick-hunt distortion during whipsaw.")
	verbose := flag.Bool("v", false, "print every trade")
	stopHunt := flag.Bool("stop-hunt", false, "diagnostic: per-symbol report on stop-hits that reclaimed entry within 6 bars + wick-depth distribution. Helps decide whether buffered-stop A/B is worth running.")
	stopHuntVerbose := flag.Bool("stop-hunt-v", false, "as --stop-hunt but also dump each individual swept-then-reverted trade")
	stopBufferR := flag.Float64("stop-buffer-r", 0, "STRATEGY VARIANT 1: widen the stop by this fraction of R (e.g. 0.3 = stop 0.3R further from entry). Risk per trade grows; TPs re-derived from new R. Goal: survive stop hunts without changing entry.")
	slideOffsetPct := flag.Float64("slide-offset-pct", 0, "STRATEGY VARIANT 2: slide BOTH entry and stop in the side's away direction by this fraction of entry (e.g. 0.002 = 0.2%). Risk distance unchanged. Goal: let the typical sweep play out, then fill past it with stop past the cluster.")
	symFlag := flag.String("symbol", "", "override market.All() with a single BingX contract code, e.g. NCCO1OILBRENT2USD-USDT — for pre-flighting new symbols without polluting the live daemon universe.")
	disablePerSym := flag.Bool("no-per-symbol-buffer", false, "clear signal.PerSymbolStopBuffer for this run — A/B comparison against the pre-2026-06-03 baseline before per-symbol stop buffers shipped.")
	replayValidator := flag.Bool("replay-validator", false, "diagnostic: run validator.Validate on each emitted signal; bucket realized R by validator verdict (STRONG/TAKE/NEUTRAL/WEAK/AVOID). Used to A/B whether validator weight changes improve predictive correlation.")
	flag.Parse()
	if *disablePerSym {
		signal.PerSymbolStopBuffer = nil
	}

	if *stopRefine {
		signal.StopRefineEnabled = true
	}
	indicator.BodyWeight = *bodyWeight

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
		StopBufferR:     *stopBufferR,
		SlideOffsetPct:  *slideOffsetPct,
		ReplayValidator: *replayValidator,
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

	symbols := market.All()
	if *symFlag != "" {
		symbols = []market.Symbol{market.Symbol(*symFlag)}
	}
	for _, sym := range symbols {
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
		if *stopHunt || *stopHuntVerbose {
			fmt.Println(res.StopHuntSummary())
		}
		if *replayValidator {
			fmt.Println(res.ValidatorReplaySummary())
		}
		if *stopHuntVerbose {
			for _, t := range res.Trades {
				if t.Outcome != "stop" || !t.Reclaimed {
					continue
				}
				fmt.Printf("    swept-then-reverted: %s %s entry=%.4f stop=%.4f wickPastStop=%.4f (%.3fR) reclaimedIn=%d bars\n",
					t.SignaledAt.Format("2006-01-02 15:04"), t.Side,
					t.Entry, t.Stop, t.WickPastStop, t.WickPastStopR, t.ReclaimBars)
			}
		}
		if *verbose {
			for _, t := range res.Trades {
				fmt.Printf("  %s %s sc=%d entry=%.4f exit=%.4f gross=%+.2f fee=%.2f net=%+.2f (%s, %s)\n",
					t.SignaledAt.Format("2006-01-02 15:04"), t.Side, t.Score,
					t.Entry, t.Exit, t.RGross, t.FeeR, t.R, t.Outcome, t.Anchor)
			}
		}
	}
}
