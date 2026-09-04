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
	structVeto := flag.Bool("struct-veto", false, "STRATEGY VARIANT: enable the N 字 counter-trend structure veto (逆勢否決) for ALL symbols. Removes signals taken against a confirmed opposing BOS/CHoCH or HH-HL/LH-LL trend. Default OFF. A/B this vs baseline to measure per-symbol Δ before baking an allowlist.")
	structZone := flag.Bool("struct-zone", false, "STRATEGY VARIANT: enable the N 字 zone-confluence vote (樞紐區 順向回檔) for ALL symbols. Adds a MOM vote when the close has pulled back into the current leg's pivot zone in the leg direction (structure intact). Additive, never vetoes. Default OFF. A/B vs baseline per symbol.")
	htfStruct := flag.Bool("htf-struct", false, "STRATEGY VARIANT (Phase 2 降維入局): fetch 4h HTF candles and require each base-TF entry to sit inside an aligned 4h 樞紐區 (same direction). Take/skip gate for ALL symbols. Default OFF. Run at -tf=1h for the 4h→1h design; A/B vs baseline per symbol.")
	structOff := flag.Bool("struct-off", false, "Short-circuit ALL live structure allowlists (metals veto + BTC/ETH zone) to establish a clean no-structure baseline. Combine with a single --struct-* flag to A/B that feature in isolation on any TF.")
	structMomentum := flag.Bool("struct-momentum", false, "STRATEGY: replace the MR engine with the trend/structure-aligned StructMomentum strategy (BOS-continuation retrace into 樞紐區) for ALL symbols. A/B vs the MR baseline per symbol/TF before assigning it in strategyFor. See docs/struct_momentum_strategy_design.md.")
	liqVote := flag.Float64("liq-vote", 0, "STRATEGY VARIANT: EQH/EQL pool proximity as an MR confluence vote — an EQH within this many percent ABOVE price votes bear, an EQL that close BELOW votes bull (polarity matches sweep-reject, the only pool use that passed an A/B). Default 0 = off; pools score nothing. Try 0.3/0.5/0.8 and check nearby params, not just one.")
	liqVoteMagnet := flag.Bool("liq-vote-magnet", false, "invert --liq-vote's polarity to test the competing liquidity-MAGNET reading (EQH above = bullish pull, EQL below = bearish pull). Only meaningful with --liq-vote > 0.")
	noStructMomentum := flag.Bool("no-struct-momentum", false, "A/B BASELINE: force the MR engine for every symbol, overriding the strategyFor allowlist. REQUIRED for an SM-vs-MR A/B on a symbol already assigned to StructMomentum (SOL/LINK 1h+2h, SUI/HYPE 1h) — without it BOTH arms run SM and the comparison is inert. Mutually exclusive with --struct-momentum.")
	smStopBuffer := flag.Float64("sm-stop-buffer", 0, "STRATEGY VARIANT: push the StructMomentum stop this many ATR(14) beyond the leg-origin invalidate (sweep buffer). Only affects --struct-momentum. Default 0 = stop on the invalidate line. A/B on SOL/LINK before shipping.")
	useDXY := flag.Bool("dxy", false, "enable DXY macro veto on XAU/XAG signals. Default OFF — 2026-05-27 backtest showed it hurt by ~46R (vetoed trades were the best ones; mean-reversion thrives on macro-divergent dips)")
	bodyWeight := flag.Float64("body-weight", 0, "POC/HVN body-weighted distribution: fraction (0,1) of each candle's volume routed to its body range. Default 0 = legacy uniform-over-HL. Try 0.7 to damp wick-hunt distortion during whipsaw.")
	verbose := flag.Bool("v", false, "print every trade")
	stopHunt := flag.Bool("stop-hunt", false, "diagnostic: per-symbol report on stop-hits that reclaimed entry within 6 bars + wick-depth distribution. Helps decide whether buffered-stop A/B is worth running.")
	stopHuntVerbose := flag.Bool("stop-hunt-v", false, "as --stop-hunt but also dump each individual swept-then-reverted trade")
	stopBufferR := flag.Float64("stop-buffer-r", 0, "STRATEGY VARIANT 1: widen the stop by this fraction of R (e.g. 0.3 = stop 0.3R further from entry). Risk per trade grows; TPs re-derived from new R. Goal: survive stop hunts without changing entry.")
	slideOffsetPct := flag.Float64("slide-offset-pct", 0, "STRATEGY VARIANT 2: slide BOTH entry and stop in the side's away direction by this fraction of entry (e.g. 0.002 = 0.2%). Risk distance unchanged. Goal: let the typical sweep play out, then fill past it with stop past the cluster.")
	sessionVol := flag.Bool("session-vol", false, "ENGINE VARIANT (Layer 1): replace the volume gates' trailing 19/20-bar MEAN baseline with a median over the same exchange-local time-of-day bucket. The trailing window spans nearly a whole 1h day, so it always contains the US cash-open bar and \"vol > 1.5x baseline\" becomes substantially a function of the hour: the open bar is 4.1% of bars but supplies 24-36% of every range-expansion fire on the stock synthetics (17% BTC, 21% XAG). Falls back to the mean wherever a bucket has under 5 samples, so it is a deliberate no-op below 1h. NOT presumed an improvement — the open bar really is a range-expansion-on-volume bar, so the biased denominator may be selecting the day's most informative candle. A/B per symbol and window.")
	symFlag := flag.String("symbol", "", "override market.All() with a single BingX contract code, e.g. NCCO1OILBRENT2USD-USDT — for pre-flighting new symbols without polluting the live daemon universe.")
	disablePerSym := flag.Bool("no-per-symbol-buffer", false, "clear signal.PerSymbolStopBuffer for this run — A/B comparison against the pre-2026-06-03 baseline before per-symbol stop buffers shipped.")
	replayValidator := flag.Bool("replay-validator", false, "diagnostic: run validator.Validate on each emitted signal; bucket realized R by validator verdict (STRONG/TAKE/NEUTRAL/WEAK/AVOID). Used to A/B whether validator weight changes improve predictive correlation.")
	useFunding := flag.Bool("funding", true, "fetch per-symbol funding-rate history and pass to engine via Context. Activates applyContextFilters' crowd penalties + applyFundingContrarianVote's contrarian +1/+2 votes. Default ON (matches shipped engine behavior).")
	noFundingVote := flag.Bool("no-funding-vote", false, "disable signal.FundingContrarianVoteEnabled — the contrarian +1/+2 vote stays off even when --funding is on. A/B switch for the pre-2026-06-08 baseline.")
	flag.Parse()
	if *structMomentum && *noStructMomentum {
		fmt.Fprintln(os.Stderr, "--struct-momentum and --no-struct-momentum are mutually exclusive: one forces StructMomentum on, the other forces it off. Pick the arm you mean.")
		os.Exit(2)
	}
	if *noStructMomentum {
		signal.StructMomentumOff = true
	}
	if *noFundingVote {
		signal.FundingContrarianVoteEnabled = false
	}
	if *disablePerSym {
		signal.PerSymbolStopBuffer = nil
	}

	if *stopRefine {
		signal.StopRefineEnabled = true
	}
	if *structVeto {
		signal.StructureVetoEnabled = true
	}
	if *structZone {
		signal.StructureZoneVoteEnabled = true
	}
	if *htfStruct {
		signal.MTFStructEnabled = true
	}
	if *structOff {
		signal.StructureLiveOff = true
	}
	if *structMomentum {
		signal.StructMomentumEnabled = true
	}
	if *sessionVol {
		signal.SessionVolBaseline = true
	}
	signal.SMStopBufferATR = *smStopBuffer
	signal.LiqVoteProxPct = *liqVote
	signal.LiqVoteMagnet = *liqVoteMagnet
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
		if *htfStruct {
			// Phase 2 降維入局: fetch the 4h HTF series for the structure
			// gate (fixed 4h regardless of base TF, per the 4h→1h design).
			if htf, herr := client.KlinesRange(ctx, sym, market.Timeframe("4h"), start, end); herr == nil && len(htf) > 0 {
				opts.HTFCandles = htf
			} else {
				if herr != nil {
					log.Printf("%s: HTF(4h) fetch failed (降維 gate inert this symbol): %v", sym, herr)
				}
				opts.HTFCandles = nil
			}
		} else {
			opts.HTFCandles = nil
		}
		if *useFunding {
			// 1000 points × 8h ≈ 333 days, covers any window we'd backtest.
			if hist, ferr := client.FundingRateHistory(ctx, sym, 1000); ferr == nil && len(hist) > 0 {
				opts.FundingHistory = hist
			} else {
				if ferr != nil {
					log.Printf("%s: funding history fetch failed (continuing without funding context): %v", sym, ferr)
				}
				opts.FundingHistory = nil
			}
		} else {
			opts.FundingHistory = nil
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
