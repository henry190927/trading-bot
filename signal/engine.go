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
	Score     int // count of confluent factors that fired
	Reasons   []string
	Notes     []string // observations shown for context, do NOT vote (range expansion, double patterns, etc.)
	Warnings  []string // crowd/funding warnings — caller should size down or skip
	Price     float64
	Fib       indicator.FibRetracement
	VP        indicator.VolumeProfile // chip-concentration map (籌碼密集區)
	POCMig    indicator.POCMigration  // POC drift across 50/100/200 windows (display + validator only)
	Opens     Opens                   // daily / weekly / monthly opening prices (display-only)
	Plan      Plan                    // execution plan (entry/stop/TP)
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
	bullVotes, bearVotes := 0, 0

	if rsi[last] < 30 {
		bullVotes++
		sig.Reasons = append(sig.Reasons, "RSI oversold")
	} else if rsi[last] > 70 {
		bearVotes++
		sig.Reasons = append(sig.Reasons, "RSI overbought")
	}

	if m := macd[last]; m.Histogram > 0 && macd[last-1].Histogram <= 0 {
		bullVotes++
		sig.Reasons = append(sig.Reasons, "MACD bullish cross")
	} else if m.Histogram < 0 && macd[last-1].Histogram >= 0 {
		bearVotes++
		sig.Reasons = append(sig.Reasons, "MACD bearish cross")
	}

	if b := boll[last]; b.Lower != 0 {
		if price <= b.Lower {
			bullVotes++
			sig.Reasons = append(sig.Reasons, "Price at lower Bollinger")
		} else if price >= b.Upper {
			bearVotes++
			sig.Reasons = append(sig.Reasons, "Price at upper Bollinger")
		}
	}

	for _, lvl := range fib.Levels {
		if lvl.Ratio == 0.618 && nearPct(price, lvl.Price, 0.003) {
			if fib.Uptrend {
				bullVotes++
				sig.Reasons = append(sig.Reasons, "Pullback to fib 0.618 in uptrend")
			} else {
				bearVotes++
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
		if sw.Side == analyzer.SweepLow {
			if !sweepBull {
				bullVotes++
				sweepBull = true
			}
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("Liquidity grab at %.2f (low side)", sw.Level))
		} else {
			if !sweepBear {
				bearVotes++
				sweepBear = true
			}
			sig.Reasons = append(sig.Reasons, fmt.Sprintf("Liquidity grab at %.2f (high side)", sw.Level))
		}
	}

	switch rsiDiv.Kind {
	case analyzer.BullishRegular:
		bullVotes++
		sig.Reasons = append(sig.Reasons, "RSI bullish divergence")
	case analyzer.BearishRegular:
		bearVotes++
		sig.Reasons = append(sig.Reasons, "RSI bearish divergence")
	}
	switch cvdDiv.Kind {
	case analyzer.BullishRegular:
		bullVotes++
		sig.Reasons = append(sig.Reasons, "CVD bullish divergence")
	case analyzer.BearishRegular:
		bearVotes++
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
				bullVotes++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("Range expansion + volume bullish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
			case bar.Close < bar.Open:
				bearVotes++
				sig.Reasons = append(sig.Reasons,
					fmt.Sprintf("Range expansion + volume bearish bar (range %.4f, vol %.0f)", barRange, bar.Volume))
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
		if p.Side == analyzer.DoubleTop && !seenTop {
			seenTop = true
			sig.Notes = append(sig.Notes, fmt.Sprintf("Double top @ %.4f being retested (bearish bias)", p.Level))
		}
		if p.Side == analyzer.DoubleBottom && !seenBot {
			seenBot = true
			sig.Notes = append(sig.Notes, fmt.Sprintf("Double bottom @ %.4f being retested (bullish bias)", p.Level))
		}
	}

	switch {
	case bullVotes > bearVotes:
		sig.Side = Long
		sig.Score = bullVotes
	case bearVotes > bullVotes:
		sig.Side = Short
		sig.Score = bearVotes
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
