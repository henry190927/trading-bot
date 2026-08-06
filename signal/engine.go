package signal

import (
	"fmt"
	"time"

	"myFirstGo/trading-bot/analyzer"
	"myFirstGo/trading-bot/dxy"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/market"
)

type Side int

const (
	Flat Side = iota
	Long
	Short
)

func (s Side) String() string {
	switch s {
	case Long:
		return "LONG"
	case Short:
		return "SHORT"
	default:
		return "FLAT"
	}
}

type Signal struct {
	Symbol    market.Symbol
	Timeframe market.Timeframe
	Side      Side
	// Score is the TOTAL confluence score — count of all winning-side
	// votes across both axes (MR + MOM). Existing thresholds
	// (MIN_SCORE=3 in daemon/monitor) and journal rows interpret this
	// as before; sub-scores below break it down by axis.
	//
	// Invariant: Score == MRScore + MomentumScore.
	Score int
	// MRScore is the mean-reversion confluence sub-score — count of
	// [MR]-tagged winning-side votes (RSI extreme, BOLL band touch,
	// Fib pullback, sweep, divergence, MACD cross, range expansion).
	MRScore int
	// MomentumScore is the trend / breakout / pattern confluence
	// sub-score — count of [MOM]-tagged winning-side votes (volume
	// anomaly, LH-LL / HH-HL structure, time-of-day, double-top /
	// double-bottom). It exists because these signals are orthogonal
	// to mean-reversion: they catch setups the MR axis is structurally
	// blind to (the 2026-06-23 ETH 1680 cascade case), and stacking
	// them into a single Score blurred semantics.
	//
	// Side determination considers both axes — see Evaluate's tail logic.
	// Each vote's Reason carries a [MR] or [MOM] tag so the trader / AI
	// advisor can attribute confluence per axis.
	MomentumScore int
	Reasons       []string
	Notes         []string // observations shown for context, do NOT vote (display-only patterns, etc.)
	Warnings      []string // crowd/funding warnings — caller should size down or skip
	Price         float64
	Fib           indicator.FibRetracement
	VP            indicator.VolumeProfile // chip-concentration map (籌碼密集區)
	POCMig        indicator.POCMigration  // POC drift across 50/100/200 windows (display + validator only)
	Opens         Opens                   // daily / weekly / monthly opening prices (display-only)
	Plan          Plan                    // execution plan (entry/stop/TP)
}

// Context carries optional perpetual-market context (funding, OI) that the
// engine uses for sanity filters. All fields are optional.
type Context struct {
	FundingRate      float64 // fraction per funding interval; >0 = longs pay shorts
	OpenInterest     float64 // current OI notional; meaningful with prior snapshot
	PrevOpenInterest float64 // optional previous OI for delta check
}

// Inputs is the full data bundle Evaluate consumes.
type Inputs struct {
	Symbol    market.Symbol
	Timeframe market.Timeframe
	Candles   []market.Candle
	Trades    []market.Trade // optional, for CVD divergence
	Ctx       Context        // optional perp context

	// Bias is the higher-timeframe directional bias (Long/Short/Flat).
	// When non-Flat, signals against this direction are suppressed.
	// Compute with signal.Bias(higherTfCandles).
	Bias Side

	// Phase 2 降維入局 (dimension-reduction entry): higher-TF structure
	// context, precomputed by the caller with no look-ahead (see
	// backtest.precomputeHTFStruct). HTFZoneDir is the direction of the
	// active HTF 樞紐區 (Flat = none); HTFInZone is whether the current
	// price sits inside that HTF zone. Only consulted when
	// MTFStructEnabled / isMTFStructSymbol; both zero-value by default so
	// callers that don't populate these are unaffected.
	HTFZoneDir Side
	HTFInZone  bool

	// DXYTrend is the current U.S. Dollar Index direction. Used as a macro
	// veto on XAU/XAG only: long XAU into a strengthening DXY is fighting
	// macro flow. Compute with dxy.Classify(dxyCandles). Empty/Flat = no
	// veto applied. Other symbols (BTC/ETH) ignore this field.
	DXYTrend dxy.Trend

	// LiveMarkPrice is the BingX continuously-updated mark price (or any
	// equivalent live mid-market reference). When > 0, the engine's
	// post-BuildPlan check refuses to emit plans whose entry is on the
	// wrong side of live market — i.e., a LONG plan whose entry is above
	// mark, or a SHORT plan whose entry is below mark. These plans would
	// fill at market on submission rather than wait for the structural
	// pullback the engine anticipated, so the original thesis is dead.
	//
	// The check is strict directional (not percentage-based) — percentage
	// thresholds don't translate across symbols where prices span orders
	// of magnitude (BTC $75000 vs XAG $75). Symbol-agnostic strict
	// comparison matches the trader's intuitive rule: "LONG entry must
	// not be higher than market; SHORT must not be lower."
	//
	// Backtest passes 0 (no live mark concept), so the check is a no-op
	// there — preserving backtest/live consistency for signal generation
	// while still preventing operationally-stale plans from being shown
	// to the trader or pushed via ntfy.
	LiveMarkPrice float64
}

// Thresholds applied by the engine.
const (
	FundingCrowdedLong  = 0.0005  // > 0.05% per interval → longs crowded
	FundingCrowdedShort = -0.0005 // < -0.05% per interval → shorts crowded
	// Extreme = 2× crowded. When funding doubles past the "crowded"
	// threshold, the contrarian side is much more likely to squeeze.
	// Empirically ≥0.10%/8h ≈ 110% annualized → unsustainable.
	FundingExtremeLong  = 0.0010  // longs paying extreme; flush risk
	FundingExtremeShort = -0.0010 // shorts paying extreme; squeeze fuel
)

func Evaluate(in Inputs) Signal {
	// Macro blackout gate. If we're inside the [-before, +after] window of
	// a CPI/FOMC/NFP/PPI release, suppress the signal regardless of what
	// the engine thinks: data-driven wicks dominate setup mechanics during
	// these windows (see #23: XAG short stopped on a CPI wick before mark
	// reverted past TP2). Daemon, monitor, dashboard, validator all see a
	// Flat signal and the Reason carries the event name so the user knows
	// why nothing is being suggested.
	if evt := macro.ActiveAt(time.Now().UTC()); evt != nil {
		return Signal{
			Symbol:    in.Symbol,
			Timeframe: in.Timeframe,
			Reasons:   []string{fmt.Sprintf("macro blackout: %s (window %dmin before / %dmin after)", evt.Name, evt.BeforeMinutes, evt.AfterMinutes)},
		}
	}

	closes := market.Closes(in.Candles)
	if len(closes) < 60 {
		return Signal{Symbol: in.Symbol, Timeframe: in.Timeframe}
	}
	last := len(closes) - 1
	price := closes[last]

	rsi := indicator.RSI(closes, 14)
	macd := indicator.MACD(closes, 12, 26, 9)
	boll := indicator.Bollinger(closes, 20, 2)
	fib := indicator.FindSwing(in.Candles, 100)
	sweeps := analyzer.DetectSweeps(in.Candles, 0.0008, 2)
	rsiDiv := analyzer.Detect(closes, rsi, 60, 2)
	vpStart := len(in.Candles) - 200
	if vpStart < 0 {
		vpStart = 0
	}
	vp := indicator.BuildVolumeProfile(in.Candles[vpStart:], 80, 5)
	pocMig := indicator.ComputePOCMigration(in.Candles, 50, 100, 200)
	opens := ComputeOpens(in.Candles, in.Candles[len(in.Candles)-1].OpenTime)

	var cvdDiv analyzer.Divergence
	if len(in.Trades) > 0 {
		cvd := analyzer.CVDByCandle(in.Candles, in.Trades)
		cvdDiv = analyzer.Detect(closes, cvd, 60, 2)
	}

	sig := Signal{Symbol: in.Symbol, Timeframe: in.Timeframe, Price: price, Fib: fib, VP: vp, POCMig: pocMig, Opens: opens}
	// Dual-axis vote accumulators. MR (mean-reversion) is the legacy axis:
	// RSI extreme, MACD cross, BOLL band touch, Fib pullback, sweep, RSI/CVD
	// divergence, range expansion. MOM (momentum) is the new axis for
	// trend-/breakout-/pattern-style votes: volume anomaly, LH-LL/HH-HL
	// structure, time-of-day, double-top/bottom. Side determination at the
	// bottom of Evaluate combines both axes.
	bullMR, bearMR := 0, 0
	bullMOM, bearMOM := 0, 0

	// Volume confirmation gate — METALS ONLY (per 2026-06-22 backtest A/B).
	//
	// The mechanic: ratio of the signal bar's volume vs the avg of the
	// prior 19 bars. Below 1.0 = below-average volume; the gate suppresses
	// bar-event votes (sweep, MACD cross) when relVol < 1.0. POSITION-based
	// votes (RSI, BOLL, Fib) and SPAN-based votes (divergence) are NOT
	// gated — their quality doesn't correlate with single-bar volume the
	// same way.
	//
	// Per-symbol enable: 60/90/120d backtest showed asymmetric impact —
	// crypto (BTC/ETH) DEGRADED across all windows (−17R aggregate, the
	// gate threw away genuinely tradeable signals because crypto's
	// baseline volume is high enough that even "low" bars carry
	// follow-through). Metals (XAU/XAG) IMPROVED in 5/6 windows (+25R
	// aggregate, gate cleanly removes drift-style fake-outs because NCCO*
	// CFD perps have high volume variance). So enable for XAU/XAG only.
	volumeConfirmEnabled := isVolumeConfirmSymbol(in.Symbol)
	const volumeConfirmThreshold = 1.0
	var relVol float64 = 1.0
	if last >= 20 {
		var avgVol float64
		for i := last - 19; i < last; i++ {
			avgVol += in.Candles[i].Volume
		}
		avgVol /= 19.0
		if avgVol > 0 {
			relVol = in.Candles[last].Volume / avgVol
		}
	}
	// volumeConfirmed: when the gate is disabled for this symbol (crypto),
	// always true so the vote runs unconditionally. When enabled (metals),
	// require relVol >= threshold.
	volumeConfirmed := !volumeConfirmEnabled || relVol >= volumeConfirmThreshold

	if rsi[last] < 30 {
		bullMR++
		sig.Reasons = append(sig.Reasons, "RSI oversold")
	} else if rsi[last] > 70 {
		bearMR++
		sig.Reasons = append(sig.Reasons, "RSI overbought")
	}

	if m := macd[last]; m.Histogram > 0 && macd[last-1].Histogram <= 0 {
		if volumeConfirmed {
			bullMR++
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("MACD bullish cross (vol %.2fx)", relVol))
		} else {
			sig.Notes = append(sig.Notes, fmt.Sprintf("MACD bullish cross suppressed — low volume %.2fx (<%.1fx threshold)", relVol, volumeConfirmThreshold))
		}
	} else if m.Histogram < 0 && macd[last-1].Histogram >= 0 {
		if volumeConfirmed {
			bearMR++
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("MACD bearish cross (vol %.2fx)", relVol))
		} else {
			sig.Notes = append(sig.Notes, fmt.Sprintf("MACD bearish cross suppressed — low volume %.2fx (<%.1fx threshold)", relVol, volumeConfirmThreshold))
		}
	}

	if b := boll[last]; b.Lower != 0 {
		if price <= b.Lower {
			bullMR++
			sig.Reasons = append(sig.Reasons, "Price at lower Bollinger")
		} else if price >= b.Upper {
			bearMR++
			sig.Reasons = append(sig.Reasons, "Price at upper Bollinger")
		}
	}

	for _, lvl := range fib.Levels {
		if lvl.Ratio == 0.618 && nearPct(price, lvl.Price, 0.003) {
			if fib.Uptrend {
				bullMR++
				sig.Reasons = append(sig.Reasons, "Pullback to fib 0.618 in uptrend")
			} else {
				bearMR++
				sig.Reasons = append(sig.Reasons, "Pullback to fib 0.618 in downtrend")
			}
		}
	}

	// HVN (籌碼密集區) is computed and stored on sig.VP for the analyze
	// dashboard, but does NOT contribute to the confluence vote. Backtest
	// showed that voting on HVN proximity (with a crude 5-bar approach
	// heuristic) dilutes the score threshold with marginal setups and
	// reduces net edge. The data is still useful to the trader as context
	// when sizing positions or staging entries — surfaced in the output
	// without polluting signal generation.

	sweepBull, sweepBear := false, false
	for _, sw := range sweeps {
		if sw.SweepIdx != last {
			continue
		}
		// Volume confirmation: a sweep is a liquidity event where price
		// punches through a level. Without volume, it's drift past the
		// level rather than absorbtion of stops — historically lower
		// follow-through. Gate the sweep VOTE on relVol >= threshold;
		// the sweep itself still surfaces in Reasons either way so the
		// trader sees it.
		if !volumeConfirmed {
			sig.Notes = append(sig.Notes, fmt.Sprintf("Liquidity grab at %.2f suppressed — low volume %.2fx (<%.1fx threshold)", sw.Level, relVol, volumeConfirmThreshold))
			continue
		}
		if sw.Side == analyzer.SweepLow {
			if !sweepBull {
				bullMR++
				sweepBull = true
			}
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("Liquidity grab at %.2f (low side, vol %.2fx)", sw.Level, relVol))
		} else {
			if !sweepBear {
				bearMR++
				sweepBear = true
			}
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("Liquidity grab at %.2f (high side, vol %.2fx)", sw.Level, relVol))
		}
	}

	switch rsiDiv.Kind {
	case analyzer.BullishRegular:
		bullMR++
		sig.Reasons = append(sig.Reasons, "RSI bullish divergence")
	case analyzer.BearishRegular:
		bearMR++
		sig.Reasons = append(sig.Reasons, "RSI bearish divergence")
	}
	switch cvdDiv.Kind {
	case analyzer.BullishRegular:
		bullMR++
		sig.Reasons = append(sig.Reasons, "CVD bullish divergence")
	case analyzer.BearishRegular:
		bearMR++
		sig.Reasons = append(sig.Reasons, "CVD bearish divergence")
	}

	// Range expansion + volume confirmation. Voting symmetric: bullish bar
	// votes long, bearish bar votes short — BUT only when RSI is in the
	// neutral band. At RSI extremes, mean-rev oscillators already drive the
	// decision and a counter-trend range bar would tie/cancel the score
	// (e.g. RSI<30 votes long, simultaneous bearish range bar votes short →
	// Flat, even though one side was right). Gated on neutral RSI per the
	// 2026-05-28 review (Patch 2). A/B over 60/90/120d on all 4 symbols
	// passed the doc's decision rule: +3.86 / +9.71 / +6.91R total deltas,
	// no symbol regressed by >10R, ≥3 of 4 symbols flat-or-positive in
	// every window. ETH was the biggest beneficiary (+4-6R per window).
	if last >= 20 && rsi[last] >= 30 && rsi[last] <= 70 {
		bar := in.Candles[last]
		barRange := bar.High - bar.Low
		atrSeries := indicator.ATR(in.Candles, 14)
		a14 := atrSeries[last]
		var avgVol float64
		for i := last - 19; i <= last; i++ {
			avgVol += in.Candles[i].Volume
		}
		avgVol /= 20.0
		if a14 > 0 && avgVol > 0 && barRange > 1.5*a14 && bar.Volume > 1.5*avgVol {
			switch {
			case bar.Close > bar.Open:
				bullMR++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("Range expansion + volume bullish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
			case bar.Close < bar.Open:
				bearMR++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("Range expansion + volume bearish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
			}
		}
	}

	// Volume anomaly vote — catches momentum/breakout situations the
	// mean-reversion votes structurally miss. Trigger: signal bar volume
	// > 3.0× 20-bar average (a much stronger threshold than the
	// range-expansion vote above at 1.5×, which gates on neutral RSI;
	// this one is NOT RSI-gated by design — extreme-volume bars at RSI
	// extremes can be capitulation flushes or breakout continuations,
	// both of which the engine wants to see).
	//
	// Direction: bar close vs open. Bearish bar with anomalous volume
	// is a continuation-short signal; bullish bar with anomalous volume
	// is a continuation-long. Conflicting with mean-rev votes (e.g. RSI<30
	// bullish + 3× vol bearish bar at the same time) → the bullVotes /
	// bearVotes max() naturally cancels — that's the desired behavior
	// for genuinely ambiguous setups.
	//
	// Motivation: 2026-06-23 ETH 15:55-16:15 cascade (-2.7% in 20min)
	// passed our engine with 0/10 across all TFs because RSI / MACD /
	// BOLL / sweep were all silent. The 14:20 and 16:15 bars had 9-10×
	// volume but the existing range-expansion vote required RSI in
	// neutral band AND range > 1.5×ATR simultaneously, which the user's
	// observation moment didn't satisfy. This vote backs up the
	// observation regardless of those gates.
	if isVolumeAnomalySymbol(in.Symbol) && last >= 20 {
		bar := in.Candles[last]
		var avgVol float64
		for i := last - 19; i <= last; i++ {
			avgVol += in.Candles[i].Volume
		}
		avgVol /= 20.0
		if avgVol > 0 && bar.Volume > 3.0*avgVol {
			ratio := bar.Volume / avgVol
			switch {
			case bar.Close > bar.Open:
				bullMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] Volume anomaly + bullish bar (vol %.1fx 20-bar avg)", ratio))
			case bar.Close < bar.Open:
				bearMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] Volume anomaly + bearish bar (vol %.1fx 20-bar avg)", ratio))
			}
		}
	}

	// Trend-structure vote (ETH only) — adds a directional bias when the
	// last 3 swing highs AND the last 3 swing lows are all monotonic in
	// the same direction (strict LH-LL = downtrend, HH-HL = uptrend).
	// Neutral structures don't vote.
	//
	// Per 2026-06-23 backtest A/B (60/90/120d × 4 symbols at 1h):
	//
	//	Symbol  60d Δ   90d Δ   120d Δ  3-window total
	//	BTC     -0.06   -3.34   -14.19  -17.59R ❌ (120d fails >±10R rule)
	//	ETH     +3.93   +3.67   -1.13    +6.47R ✓
	//	XAU     -2.25   -2.94   +5.66    +0.47R ≈ flat
	//	XAG     -1.56   +3.10   -5.50    -3.96R
	//
	// BTC: vote misfires in choppy regime, adds +6/+7/+6 new signals per
	// window most of which net negative — the engine's existing edge on
	// BTC is at extremes, and structure votes against it dilute that.
	// ETH: 2/3 windows clean win (+3-4R per window) — exactly the
	// 2026-06-23 1680 case structurally (LH-LL series 1712→1693→1690
	// would have triggered a bear vote even when other votes were silent).
	//
	// Enable ETH only. Same per-symbol allowlist pattern as the
	// volume-anomaly vote.
	if isStructureVoteSymbol(in.Symbol) {
		if structure, tops, bots := ClassifyTrendStructure(in.Candles); structure != StructNeutral && len(tops) >= 3 && len(bots) >= 3 {
			switch structure {
			case StructUptrend:
				bullMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] HH-HL uptrend (highs %.4f→%.4f→%.4f, lows %.4f→%.4f→%.4f)",
						tops[0].Price, tops[1].Price, tops[2].Price,
						bots[0].Price, bots[1].Price, bots[2].Price))
			case StructDowntrend:
				bearMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] LH-LL downtrend (highs %.4f→%.4f→%.4f, lows %.4f→%.4f→%.4f)",
						tops[0].Price, tops[1].Price, tops[2].Price,
						bots[0].Price, bots[1].Price, bots[2].Price))
			}
		}
	}

	// N 字 zone-confluence vote (樞紐區 順向回檔). Additive — never vetoes.
	// When the latest close has pulled back INTO the current impulse
	// leg's pivot zone in the leg direction (InZone) and structure hasn't
	// flipped (Zone is nil'd on an opposing CHoCH), vote in the leg
	// direction: the buy-the-pullback / sell-the-rally continuation entry.
	// Complements the HH-HL / LH-LL trend vote by firing at the entry
	// LOCATION rather than on the trend label. Gated per-symbol via
	// isStructureZoneVoteSymbol, or forced across the whole universe by
	// StructureZoneVoteEnabled (--struct-zone) for the A/B.
	if StructureZoneVoteEnabled || (isStructureZoneVoteSymbol(in.Symbol) && isStructureTF(in.Timeframe)) {
		if zst := AnalyzeStructure(in.Candles, 2); zst.Zone != nil && zst.InZone {
			switch zst.Zone.Dir {
			case StructUptrend:
				bullMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] 樞紐區 pullback %.4f–%.4f (up-leg intact)", zst.Zone.Lo, zst.Zone.Hi))
			case StructDowntrend:
				bearMOM++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("[MOM] 樞紐區 rally %.4f–%.4f (down-leg intact)", zst.Zone.Lo, zst.Zone.Hi))
			}
		}
	}

	// Time-of-day momentum vote — when the signal bar opens during the
	// NY pre-market / active session (12:00-21:00 UTC, roughly 20:00 TPE
	// to 05:00 TPE next day) AND the bar has clear directional body,
	// vote in the bar direction.
	//
	// Hypothesis: crypto / metals see meaningful USD-correlated flow
	// during NY session; bars with strong bodies in this window are more
	// likely continuation rather than range noise. Captures the pattern
	// the 2026-06-24 ETH 1650.7 short hit (user noted "rapidly pulled up
	// to grab liquidity before New York times" — that exact regime).
	//
	// Thresholds (subject to backtest tuning):
	//   body_pct  = |close - open| / open > 0.3%  (meaningful absolute move)
	//   body_frac = |close - open| / (high - low) > 0.55  (mostly body, not wicks)
	//
	// Window inclusive of NY pre-market positioning (12:00 UTC = 08:00 EDT)
	// through NY equity close (21:00 UTC = 17:00 EDT). Avoids the lower-
	// liquidity Asia/Europe handover.
	if last >= 1 && isTimeOfDaySymbol(in.Symbol) {
		bar := in.Candles[last]
		hourUTC := bar.OpenTime.UTC().Hour()
		inNYWindow := hourUTC >= 12 && hourUTC < 21
		if inNYWindow {
			body := bar.Close - bar.Open
			absBody := body
			if absBody < 0 {
				absBody = -absBody
			}
			barRange := bar.High - bar.Low
			bodyPct := absBody / bar.Open
			var bodyFrac float64
			if barRange > 0 {
				bodyFrac = absBody / barRange
			}
			if bodyPct > 0.003 && bodyFrac > 0.55 {
				switch {
				case body > 0:
					bullMOM++
					sig.Reasons = append(sig.Reasons,
						fmt.Sprintf("[MOM] NY-session bullish body (%02d:00 UTC, %.2f%% body, %.0f%% of range)",
							hourUTC, bodyPct*100, bodyFrac*100))
				case body < 0:
					bearMOM++
					sig.Reasons = append(sig.Reasons,
						fmt.Sprintf("[MOM] NY-session bearish body (%02d:00 UTC, %.2f%% body, %.0f%% of range)",
							hourUTC, bodyPct*100, bodyFrac*100))
				}
			}
		}
	}

	// Double top / bottom. Same story as range expansion — informative but
	// symbol-regime dependent in backtest, so display-only.
	dPatterns := analyzer.DetectDoublePatterns(in.Candles, 80, 2, 5, 5, 0.003, 0.01)
	seenTop, seenBot := false, false
	for _, p := range dPatterns {
		if !nearPct(price, p.Level, 0.005) {
			continue
		}
		// Promoted from display-only Note → momentum vote 2026-06-25.
		// Double top retest near price = bearish-reversal pattern;
		// double bottom retest = bullish-reversal pattern. Per-symbol
		// gate (isDoublePatternSymbol) will narrow after backtest A/B
		// per the established precedent.
		if p.Side == analyzer.DoubleTop && !seenTop {
			seenTop = true
			if isDoublePatternSymbol(in.Symbol) {
				bearMOM++
				sig.Reasons = append(sig.Reasons, fmt.Sprintf("[MOM] Double top @ %.4f retest (bearish pattern)", p.Level))
			} else {
				sig.Notes = append(sig.Notes, fmt.Sprintf("Double top @ %.4f retest (display-only, vote disabled for %s)", p.Level, in.Symbol))
			}
		}
		if p.Side == analyzer.DoubleBottom && !seenBot {
			seenBot = true
			if isDoublePatternSymbol(in.Symbol) {
				bullMOM++
				sig.Reasons = append(sig.Reasons, fmt.Sprintf("[MOM] Double bottom @ %.4f retest (bullish pattern)", p.Level))
			} else {
				sig.Notes = append(sig.Notes, fmt.Sprintf("Double bottom @ %.4f retest (display-only, vote disabled for %s)", p.Level, in.Symbol))
			}
		}
	}

	// Per-axis sides + scores tracked for visibility (dashboard, AI advisor,
	// journal). The axes are LABELING — they don't gate the trade.
	var mrSide, momSide Side
	switch {
	case bullMR > bearMR:
		mrSide = Long
	case bearMR > bullMR:
		mrSide = Short
	}
	switch {
	case bullMOM > bearMOM:
		momSide = Long
	case bearMOM > bullMOM:
		momSide = Short
	}

	// Side determination: SUM both axes — preserves the historical
	// max(bullVotes, bearVotes) behavior that ship gates were tuned on.
	// 2026-06-25 A/B test: conflict-suppression (Flat when axes disagree)
	// cost −9.5R on ETH and −3R on BTC over 60/90/120d, because MR-vs-MOM
	// disagreement at sweep-anchored RSI-extreme setups is actually the
	// engine's bread and butter — RSI says oversold (long), LH-LL structure
	// says downtrend (short), the LONG mean-rev play often wins. Don't
	// gate it; surface the disagreement as a Warning so the trader / AI
	// can size down or skip on judgment.
	bullSum := bullMR + bullMOM
	bearSum := bearMR + bearMOM
	switch {
	case bullSum > bearSum:
		sig.Side = Long
	case bearSum > bullSum:
		sig.Side = Short
	}

	// Score = sum of winning-side votes across both axes — keeps the
	// "score is total confluence" semantic that all existing thresholds
	// (MIN_SCORE=3 in daemon, monitor) depend on. MomentumScore exposes
	// the MOM-axis contribution separately for downstream visibility
	// without changing the trade-selection contract.
	switch sig.Side {
	case Long:
		sig.Score = bullSum
		sig.MRScore = bullMR
		sig.MomentumScore = bullMOM
	case Short:
		sig.Score = bearSum
		sig.MRScore = bearMR
		sig.MomentumScore = bearMOM
	}

	// Surface axis disagreement as a Warning so the AI advisor / trader
	// can attribute confluence quality. Doesn't change trade direction.
	if mrSide != Flat && momSide != Flat && mrSide != momSide {
		sig.Warnings = append(sig.Warnings,
			fmt.Sprintf("Axis disagreement: MR=%s (mr_votes %d/%d)  MOM=%s (mom_votes %d/%d) — sum-of-axes resolves to %s",
				mrSide, bullMR, bearMR, momSide, bullMOM, bearMOM, sig.Side))
	}

	// MTF bias filter: suppress contra-bias signals before context filters
	// and before building the plan. We keep the original Reasons so the user
	// can see what would've fired; the suppressed direction is recorded as a
	// warning instead of an actionable signal.
	if in.Bias != Flat && sig.Side != Flat && sig.Side != in.Bias {
		sig.Warnings = append(sig.Warnings,
			fmt.Sprintf("Against higher-TF %s bias — suppressed", in.Bias))
		sig.Side = Flat
		sig.Score = 0
	}

	// Phase 2 降維入局 (dimension-reduction entry) gate. Require the
	// base-TF entry to sit inside an aligned higher-TF 樞紐區 (same
	// direction): HTF structure blesses the pullback zone, the base TF
	// provides the trigger. Filter (take/skip) using the precomputed,
	// no-look-ahead HTF context. A/B behind MTFStructEnabled before any
	// per-symbol allowlist is baked in.
	if (MTFStructEnabled || isMTFStructSymbol(in.Symbol)) && sig.Side != Flat {
		if in.HTFZoneDir != sig.Side || !in.HTFInZone {
			sig.Warnings = append(sig.Warnings, "降維入局: no aligned HTF 樞紐區 — suppressed")
			sig.Side = Flat
			sig.Score = 0
		}
	}

	// DXY macro veto — applies only to precious metals (XAU/XAG).
	//
	// 2026-05-27 backtest finding: enabling this veto HURT XAU/XAG by ~46R
	// over 60d at 1h. The trades it vetoed averaged ~+1.16R each — the
	// macro-divergent dips are exactly where this mean-reversion strategy
	// has its best edge. Kept available behind in.DXYTrend (callers must
	// explicitly populate to enable), default off in all CLIs and the daemon.
	//
	// Direction-of-veto logic is correct on macro/daily scale (long XAU
	// into rising DXY = fighting macro flow) but the strategy is too short-
	// horizon to benefit. Useful future use: as a confidence multiplier or
	// position sizer rather than a veto.
	if isPreciousMetal(in.Symbol) && sig.Side != Flat {
		switch {
		case sig.Side == Long && in.DXYTrend == dxy.Up:
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("DXY uptrend — long %s fights USD strength", shortName(in.Symbol)))
			sig.Side = Flat
			sig.Score = 0
		case sig.Side == Short && in.DXYTrend == dxy.Down:
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("DXY downtrend — short %s fights USD weakness", shortName(in.Symbol)))
			sig.Side = Flat
			sig.Score = 0
		}
	}

	// N 字 counter-trend structure veto (逆勢否決) — A/B only, default OFF.
	// Removes signals taken against a confirmed opposing structure: a
	// BOS/CHoCH against the side on the latest closed bar, or an
	// established HH-HL / LH-LL trend against the side. Closed-bar (uses
	// AnalyzeStructure on in.Candles). Applies to all symbols while
	// enabled so the backtest can measure per-symbol Δ before any
	// allowlist is baked in.
	if (StructureVetoEnabled || (isStructureVetoSymbol(in.Symbol) && isStructureTF(in.Timeframe))) && sig.Side != Flat {
		st := AnalyzeStructure(in.Candles, 2)
		var veto bool
		switch sig.Side {
		case Long:
			veto = st.Trend == StructDowntrend || st.Event == EvCHoCHDown || st.Event == EvBOSDown
		case Short:
			veto = st.Trend == StructUptrend || st.Event == EvCHoCHUp || st.Event == EvBOSUp
		}
		if veto {
			label := st.Trend.String()
			if st.Event != EvNone {
				label = st.Event.String()
			}
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("Structure veto — %s entry against %s", sig.Side, label))
			sig.Side = Flat
			sig.Score = 0
		}
	}

	annotateContextWarnings(&sig, in.Ctx)
	// NOTE 2026-06-08: applyFundingContrarianVote was added then reverted
	// after the 60/90/120d A/B showed it earned its complexity only on
	// XAG, and even there with mixed sign (−2R aggregate). The function
	// + constants + backtest plumbing remain in case future work wants
	// to revisit with tighter thresholds or a longer data window.
	if sig.Side != Flat {
		sig.Plan = BuildPlan(sig, in.Candles, sweeps)
		applyPerSymbolStopBuffer(&sig.Plan, sig.Side, in.Symbol)
	}

	// Live-mark plan validity check. Refuse to emit plans where market has
	// already moved past the planned entry in the chase direction — those
	// would fill at market rather than wait for the structural anchor.
	// Strict directional comparison (no percentage threshold): symbol-
	// agnostic, matches the trader's stated rule directly.
	// Backtest passes LiveMarkPrice=0, so this is a no-op there.
	if in.LiveMarkPrice > 0 && sig.Plan.Entry > 0 && sig.Side != Flat {
		var wrongSide bool
		switch sig.Side {
		case Long:
			wrongSide = sig.Plan.Entry > in.LiveMarkPrice
		case Short:
			wrongSide = sig.Plan.Entry < in.LiveMarkPrice
		}
		if wrongSide {
			diff := sig.Plan.Entry - in.LiveMarkPrice
			if sig.Side == Short {
				diff = -diff
			}
			pct := diff / in.LiveMarkPrice * 100
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("Plan entry %.4f vs live mark %.4f — wrong side by %.4f (%.2f%%), plan suppressed",
					sig.Plan.Entry, in.LiveMarkPrice, diff, pct))
			sig.Side = Flat
			sig.Score = 0
			sig.Plan = Plan{}
		}
	}
	return sig
}

// FundingContrarianVoteEnabled gates the contrarian-funding vote so
// backtest A/Bs can flip it off without code changes. Default true =
// shipped on. Set to false from cmd/backtest via --no-funding-vote
// to compare to the pre-2026-06-08 baseline.
var FundingContrarianVoteEnabled = true

// StructureLiveOff, when true, short-circuits ALL live per-symbol
// structure allowlists (veto / zone / mtf) back to off. Used by
// cmd/backtest --struct-off to establish a clean no-structure baseline
// so a single structure feature can be A/B'd in isolation via its own
// --struct-* flag (otherwise the already-shipped BTC/ETH zone + metals
// veto contaminate the baseline). Never set in live daemon/web.
var StructureLiveOff = false

// MTFStructEnabled forces the Phase 2 降維入局 HTF-structure gate ON for
// ALL symbols. Default false. Set true from cmd/backtest via
// --htf-struct to A/B it. Live enablement is per-symbol via
// isMTFStructSymbol. Requires the caller to populate Inputs.HTFZoneDir /
// HTFInZone (no-look-ahead HTF 樞紐區 context).
var MTFStructEnabled = false

// StructureZoneVoteEnabled forces the N 字 zone-confluence vote (樞紐區
// 順向回檔) ON for ALL symbols. Default false. Set true from
// cmd/backtest via --struct-zone to A/B the vote across the universe.
// Live enablement is per-symbol via isStructureZoneVoteSymbol.
var StructureZoneVoteEnabled = false

// StructureVetoEnabled forces the N 字 counter-trend structure veto
// (逆勢否決) ON for ALL symbols. Default false. Set true from
// cmd/backtest via --struct-veto to A/B the veto across the whole
// universe. Live enablement is per-symbol via isStructureVetoSymbol
// (currently the precious metals) — this global flag is purely the
// backtest override that ignores the allowlist. The veto only ever
// REMOVES trades taken against a confirmed opposing structure — never
// adds — so worst case is lost edge, not new bad entries.
var StructureVetoEnabled = false

// applyFundingContrarianVote adds confluence votes when the trade side
// is OPPOSITE the crowded side — the side getting paid to hold a
// position, with squeeze fuel building behind it.
//
//   LONG  + funding ≤ FundingExtremeShort (-0.10%/8h):  +2 (extreme squeeze setup)
//   LONG  + funding ≤ FundingCrowdedShort (-0.05%/8h):  +1 (crowded shorts)
//   SHORT + funding ≥ FundingExtremeLong  (+0.10%/8h):  +2 (extreme flush setup)
//   SHORT + funding ≥ FundingCrowdedLong  (+0.05%/8h):  +1 (crowded longs)
//
// Same-side crowding is handled by annotateContextWarnings as an
// advisory warning (display-only; doesn't affect trade selection).
func applyFundingContrarianVote(sig *Signal, ctx Context) {
	if !FundingContrarianVoteEnabled || sig.Side == Flat || ctx.FundingRate == 0 {
		return
	}
	fr := ctx.FundingRate
	switch {
	case sig.Side == Long && fr <= FundingExtremeShort:
		sig.Score += 2
		sig.Reasons = append(sig.Reasons,
			fmt.Sprintf("Extreme negative funding %+.4f%% — short squeeze fuel", fr*100))
	case sig.Side == Long && fr <= FundingCrowdedShort:
		sig.Score += 1
		sig.Reasons = append(sig.Reasons,
			fmt.Sprintf("Crowded shorts (funding %+.4f%%) — contrarian long", fr*100))
	case sig.Side == Short && fr >= FundingExtremeLong:
		sig.Score += 2
		sig.Reasons = append(sig.Reasons,
			fmt.Sprintf("Extreme positive funding %+.4f%% — long flush risk", fr*100))
	case sig.Side == Short && fr >= FundingCrowdedLong:
		sig.Score += 1
		sig.Reasons = append(sig.Reasons,
			fmt.Sprintf("Crowded longs (funding %+.4f%%) — contrarian short", fr*100))
	}
}

// annotateContextWarnings appends advisory Warnings when funding/OI
// suggest the trade is fighting the crowd. ADVISORY ONLY — Score and
// Side are never changed. The dashboard shows these to the trader as
// discretionary context; the engine treats them as informational.
//   - Long signal with very positive funding = chasing crowded longs.
//   - Short signal with very negative funding = chasing crowded shorts.
//   - OI dropping with the signal direction = unwind, not new flow.
//
// Renamed from applyContextFilters (2026-06-08) — the original name
// implied filtering that never happened. Verified by funding-on/off
// A/B: bit-identical trade sets.
func annotateContextWarnings(sig *Signal, ctx Context) {
	if sig.Side == Long && ctx.FundingRate > FundingCrowdedLong {
		sig.Warnings = append(sig.Warnings,
			fmt.Sprintf("Crowded longs (funding %.4f%%); size down or wait", ctx.FundingRate*100))
	}
	if sig.Side == Short && ctx.FundingRate < FundingCrowdedShort {
		sig.Warnings = append(sig.Warnings,
			fmt.Sprintf("Crowded shorts (funding %.4f%%); squeeze risk", ctx.FundingRate*100))
	}
	if ctx.PrevOpenInterest > 0 && ctx.OpenInterest > 0 {
		oiDelta := (ctx.OpenInterest - ctx.PrevOpenInterest) / ctx.PrevOpenInterest
		// Corrected OI semantics (2026-05-28 review): a "squeeze" needs
		// fresh positions piling in on the wrong side. OI falling on a
		// down-move = longs unwinding = confirms a short trade. OI rising
		// = new positions, and on the side opposite the move = squeeze risk
		// for that direction.
		if sig.Side == Long && oiDelta < -0.02 {
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("OI down %.2f%% — long unwind driving the move, not new buying", oiDelta*100))
		}
		if sig.Side == Short && oiDelta > 0.02 {
			sig.Warnings = append(sig.Warnings,
				fmt.Sprintf("OI up %.2f%% — shorts crowding, squeeze risk", oiDelta*100))
		}
	}
}

// isPreciousMetal returns true for symbols where DXY trend acts as a macro
// veto. Crypto symbols ignore DXY (correlation is weaker and regime-dependent).
func isPreciousMetal(s market.Symbol) bool {
	return s == market.XAUUSDT || s == market.XAGUSDT
}

// isVolumeConfirmSymbol returns true for symbols where the bar-event
// volume gate (sweep / MACD cross suppression on relVol < 1.0) applies.
// Per the 2026-06-22 backtest, crypto degraded under the gate while
// metals improved — same per-symbol split as the DXY check above, by
// coincidence not design.
func isVolumeConfirmSymbol(s market.Symbol) bool {
	return s == market.XAUUSDT || s == market.XAGUSDT
}

// isVolumeAnomalySymbol returns true for symbols where the volume-anomaly
// vote (signal bar vol > 3× 20-bar avg → vote in bar direction) is
// enabled. Per the 2026-06-23 backtest A/B (60/90/120d × 4 symbols at 1h):
//
//	Symbol  60d Δ  90d Δ  120d Δ  3-window total
//	BTC     +3.56  +4.43  +2.28   +10.27R ✓✓
//	ETH     -4.77  -0.17  -3.54    -8.48R
//	XAU     +2.12  -0.97  -0.11    +1.04R ≈ flat
//	XAG     -7.58 -12.82 -13.58   -33.98R ❌
//
// Asymmetric — BTC improved 3/3 windows consistently; ETH/XAG hurt
// (ETH: noisy volume signature, false positives; XAG: already gates
// via isVolumeConfirmSymbol's 1.0× threshold, so stacking a 3× anomaly
// vote double-counts the same signal). XAU was a wash.
//
// Enable BTC only — ship-gate compliant + captures the breakout-like
// continuations BTC's clean tape rewards. Re-test if BTC's volume
// behavior regime-shifts.
func isVolumeAnomalySymbol(s market.Symbol) bool {
	return s == market.BTCUSDT
}

// isStructureVoteSymbol returns true for symbols where the LH-LL /
// HH-HL trend-structure vote is enabled. Per 2026-06-23 backtest A/B:
//
//	Symbol  60d Δ   90d Δ   120d Δ  3-window total
//	BTC     -0.06   -3.34   -14.19  -17.59R ❌
//	ETH     +3.93   +3.67   -1.13    +6.47R ✓
//	XAU     -2.25   -2.94   +5.66    +0.47R ≈ flat
//	XAG     -1.56   +3.10   -5.50    -3.96R
//
// ETH 2/3 windows improved cleanly; BTC 120d −14R fails ship gate.
// Enable ETH only. Rationale matches user's 2026-06-23 ETH 1680
// observation: LH-LL series (1712→1693→1690) should have flagged
// the cascade direction even when oscillator votes were silent.
func isStructureVoteSymbol(s market.Symbol) bool {
	return s == market.ETHUSDT
}

// isStructureVetoSymbol returns true for symbols where the N 字
// counter-trend structure veto (逆勢否決) is enabled live. Per the
// 2026-08-06 backtest A/B (60/90/120d × 4 symbols at 1h, sweep-only,
// fee=6bp) of the BOS/CHoCH + trend veto (Δ = veto − baseline netR):
//
//	Symbol  60d Δ    90d Δ    120d Δ   3-window total
//	BTC     +1.95    +4.71    -3.07     +3.59R   ~ mixed sign
//	ETH     +3.68    -9.29    -9.11    -14.72R   ❌ kills the MR edge
//	XAU     +0.79    +3.07    +3.57     +7.43R   ✓ all-positive harm-reduction
//	XAG    +11.63   +13.02    +5.47    +30.12R   ✓✓ all-positive, flips 1h to green
//
// Mechanistic split: the veto strips counter-structure entries. ETH's
// edge IS mean-reversion (dip-buying into trend) so it hurts — same
// failure mode as the DXY veto (−46R); ETH also already runs the
// structure *vote*, so a veto double-suppresses. The metals had no MR
// edge to protect there — the vetoed trades were pure bleed, so XAG
// flips positive (3/3 windows) and XAU consistently bleeds less.
//
// Enable the precious metals only (= isPreciousMetal). BTC mixed / ETH
// hard-fail stay off. Validated at 1h; re-test before relying on it at
// the daemon's native TF.
// isStructureTF gates the LIVE structure features (veto + zone vote) to
// the timeframe they were validated on. The 2026-08-06 A/Bs show the
// edge is a 1h phenomenon: at 15m the zone vote turns net-negative
// (adds trades in a poison regime) and at 5m everything is catastrophic
// (−76~−151R/60d). Only 1h ships live; lower TFs fall back to the plain
// engine so /chart scores aren't inflated by a known-negative vote. The
// backtest flags (StructureVetoEnabled / StructureZoneVoteEnabled) skip
// this gate so any TF can still be A/B'd.
func isStructureTF(tf market.Timeframe) bool {
	return tf == "1h"
}

func isStructureVetoSymbol(s market.Symbol) bool {
	if StructureLiveOff {
		return false
	}
	return isPreciousMetal(s)
}

// isStructureZoneVoteSymbol returns true for symbols where the N 字
// zone-confluence vote (樞紐區 順向回檔) is enabled live. Per the
// 2026-08-06 A/B (--struct-zone, 60/90/120d × 4 symbols at 1h,
// sweep-only, fee=6bp; Δ = vote − baseline netR, baseline already
// includes the live metals veto):
//
//	Symbol  60d Δ    90d Δ    120d Δ   3-window total
//	BTC     +5.58    +8.65    +2.83    +17.06R   ✓✓ all-positive, flips BTC green
//	ETH     -6.53    +4.51    +4.92     +2.90R   ~ 2/3 positive (60d dips)
//	XAU     +0.24    -0.18    +2.95     +3.01R   ~ marginal (already has veto)
//	XAG     -1.77    +0.20    +3.00     +1.43R   ~ mixed (already has veto)
//
// Complements the veto perfectly: the veto helps the metals and hurts
// BTC/ETH; this additive with-structure vote helps BTC (the veto's
// weak spot) and ETH. Enable BTC + ETH (user call 2026-08-06: BTC is a
// clean gate pass; ETH taken for its +4.5/+4.9 on the two longer windows
// despite the 60d dip). Metals stay veto-only — don't stack.
func isStructureZoneVoteSymbol(s market.Symbol) bool {
	if StructureLiveOff {
		return false
	}
	return s == market.BTCUSDT || s == market.ETHUSDT
}

// isMTFStructSymbol returns true for symbols where the Phase 2 降維入局
// HTF-structure gate is enabled live. Default: none — pending the
// 2026-08-06 A/B (--htf-struct, 4h→1h). Being a take/skip filter it may
// fight the MR edge (cf. the veto), so results decide the allowlist.
func isMTFStructSymbol(s market.Symbol) bool {
	return false
}

// isTimeOfDaySymbol returns true for symbols where the NY-session
// time-of-day vote is enabled. Initially open to all 4; backtest A/B
// will narrow per asymmetric results per the established precedent.
// isTimeOfDaySymbol gates the NY-session time-of-day momentum vote.
// 2026-06-25 backtest A/B: enabling for all 4 symbols hurt BTC −35R
// and XAG −15R across 60/90/120d windows, was neutral on ETH (≈0R),
// and HELPED XAU +14.57R aggregate (especially 120d window with
// +11.26R). Same vote also lifts XAU 60d from −5.89 → −0.45 (+5.44R).
// Enable XAU only — matches the precedent of vol-anomaly (BTC-only)
// and structure (ETH-only): each symbol has a different signal
// signature that responds best to a different momentum vote.
func isTimeOfDaySymbol(s market.Symbol) bool {
	return s == market.XAUUSDT
}

// isDoublePatternSymbol gates the double-top/bottom retest momentum
// vote. Per same 2026-06-25 A/B as time-of-day: bundled together,
// the two new votes lifted XAU by +14.57R but hurt BTC/XAG. Enable
// XAU only for now; if the user wants per-vote granularity later
// (one vote alone may behave differently), we'll backtest each
// independently.
func isDoublePatternSymbol(s market.Symbol) bool {
	return s == market.XAUUSDT
}

// shortName returns BTC / ETH / XAU / XAG for logging; falls back to the
// full BingX symbol for anything else.
func shortName(s market.Symbol) string {
	switch s {
	case market.BTCUSDT:
		return "BTC"
	case market.ETHUSDT:
		return "ETH"
	case market.XAUUSDT:
		return "XAU"
	case market.XAGUSDT:
		return "XAG"
	}
	return string(s)
}

func nearPct(a, b, pct float64) bool {
	if b == 0 {
		return false
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d/b <= pct
}
