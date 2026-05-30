// Package validator scores a user-proposed trade against the engine's
// independent view + HVN / sweep / fib / BOLL context. Used by the
// `validate` CLI and the web /validate form so both consume identical scoring.
package validator

import (
	"fmt"
	"math"

	"myFirstGo/trading-bot/analyzer"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// Factor is one line item contributing to the total validation score.
type Factor struct {
	Name   string
	Points float64
	Detail string
}

// Result is the full scored validation, suitable for both terminal print
// and HTML rendering.
type Result struct {
	Symbol    market.Symbol
	Timeframe market.Timeframe
	Side      signal.Side
	Entry     float64
	Price     float64

	EngineSide  signal.Side
	EngineScore int
	EnginePlan  signal.Plan
	Reasons     []string
	Notes       []string

	VP  indicator.VolumeProfile
	ATR float64

	NearestSweep   *analyzer.LiquiditySweep
	NearestFib     *indicator.FibLevel
	BollLower      float64
	BollUpper      float64
	NearestHVN     float64
	NearestHVNDist float64
	IsHVNPOC       bool
	HVNsAbove      int
	HVNsBelow      int

	// Value-area position. Stage A surfaces these for the diagnose row
	// but keeps weights modest — no engine vote yet.
	VAH         float64 // value area high
	VAL         float64 // value area low
	InsideVA    bool    // entry within [VAL, VAH]
	AtVAH       bool    // entry within 0.3% of VAH
	AtVAL       bool    // entry within 0.3% of VAL
	OutsideVAUp bool    // entry > VAH (above value)
	OutsideVADn bool    // entry < VAL (below value)

	// POC migration across nested windows — regime classifier. Stage A.
	POCMig indicator.POCMigration

	SuggStop float64
	SuggTP1  float64
	SuggTP2  float64
	Risk     float64
	FeeR     float64

	// RecentFlashBar fields capture whether a range-expansion + volume bar
	// fired in the last 3 candles. Used to penalize trades that fight a
	// fresh strong directional move (e.g. LONG into a recent bearish dump).
	RecentFlashBarBearish bool
	RecentFlashBarBullish bool
	RecentFlashBarAge     int     // 0 = current bar, 1 = previous, etc.
	RecentFlashBarRange   float64 // for display

	Factors []Factor
	Total   float64
	Verdict string
}

// Validate runs the engine + structural checks against a proposed entry
// and returns a scored result. Requires at least 60 candles.
//
// liveMarketPrice (optional, variadic) overrides the "current market"
// reference used for chase computation and Result.Price display. When
// 0 or omitted, the closed-bar close (the engine's reference) is used.
// Pass the BingX mark price in live contexts (dashboard diagnose, web
// /validate form) so chase math reflects where the user would actually
// fill right now. Backtest replay should leave this 0.
func Validate(sym market.Symbol, tf market.Timeframe, side signal.Side, entry, feeBps float64, candles []market.Candle, liveMarketPrice ...float64) Result {
	closes := market.Closes(candles)
	price := closes[len(closes)-1]

	// Pull the optional live-market override up front so we can feed it
	// into the engine's plan-validity check too (it will suppress plans
	// already exceeded by mark) and use it as the chase-reference below.
	var liveMark float64
	if len(liveMarketPrice) > 0 && liveMarketPrice[0] > 0 {
		liveMark = liveMarketPrice[0]
	}

	sig := signal.Evaluate(signal.Inputs{
		Symbol: sym, Timeframe: tf, Candles: candles,
		LiveMarkPrice: liveMark,
	})

	// Override comparison price with live mark if caller provided one.
	// Note: only `price` (chase reference + r.Price display) is overridden;
	// engine math above already consumed closes-based price and is unchanged.
	if liveMark > 0 {
		price = liveMark
	}

	atr := indicator.ATR(candles, 14)
	a := atr[len(atr)-1]
	boll := indicator.Bollinger(closes, 20, 2)
	b := boll[len(boll)-1]
	sweeps := analyzer.DetectSweeps(candles, 0.0008, 2)

	r := Result{
		Symbol:      sym,
		Timeframe:   tf,
		Side:        side,
		Entry:       entry,
		Price:       price,
		EngineSide:  sig.Side,
		EngineScore: sig.Score,
		EnginePlan:  sig.Plan,
		Reasons:     sig.Reasons,
		Notes:       sig.Notes,
		VP:          sig.VP,
		POCMig:      sig.POCMig,
		ATR:         a,
		BollLower:   b.Lower,
		BollUpper:   b.Upper,
	}

	bestSweepDist := math.MaxFloat64
	for i := range sweeps {
		sw := sweeps[i]
		d := math.Abs(sw.Level - entry)
		if d < bestSweepDist {
			bestSweepDist = d
			r.NearestSweep = &sweeps[i]
		}
	}

	if len(sig.Fib.Levels) > 0 {
		bestFib := math.MaxFloat64
		for i, lvl := range sig.Fib.Levels {
			d := math.Abs(lvl.Price - entry)
			if d < bestFib {
				bestFib = d
				r.NearestFib = &sig.Fib.Levels[i]
			}
		}
	}

	if len(r.VP.HVN) > 0 {
		best := math.MaxFloat64
		for _, h := range r.VP.HVN {
			d := math.Abs(h - entry)
			if d < best {
				best = d
				r.NearestHVN = h
				r.NearestHVNDist = d
				r.IsHVNPOC = math.Abs(h-r.VP.POC) < 1e-9
			}
		}
		for _, h := range r.VP.HVN {
			if h > entry {
				r.HVNsAbove++
			} else if h < entry {
				r.HVNsBelow++
			}
		}
	}

	// Value area position. 0.3% tolerance for "at edge" is roughly half an
	// ATR for liquid crypto pairs / a few ticks for XAU/XAG.
	if r.VP.VAH > 0 && r.VP.VAL > 0 {
		r.VAH = r.VP.VAH
		r.VAL = r.VP.VAL
		const edgeTol = 0.003
		if entry > 0 {
			distVAH := math.Abs(entry-r.VAH) / entry
			distVAL := math.Abs(entry-r.VAL) / entry
			r.AtVAH = distVAH <= edgeTol
			r.AtVAL = distVAL <= edgeTol
			r.InsideVA = entry >= r.VAL && entry <= r.VAH
			r.OutsideVAUp = entry > r.VAH
			r.OutsideVADn = entry < r.VAL
		}
	}

	risk := signal.StopATRMul * a
	r.Risk = risk
	if side == signal.Long {
		r.SuggStop = entry - risk
		r.SuggTP1 = entry + risk
		r.SuggTP2 = entry + 2*risk
	} else {
		r.SuggStop = entry + risk
		r.SuggTP1 = entry - risk
		r.SuggTP2 = entry - 2*risk
	}
	if risk > 0 {
		r.FeeR = (feeBps / 10000.0) * entry / risk
	}

	// Detect recent range-expansion + volume bar (look back 3 bars). Same
	// criteria as the engine: range > 1.5×ATR(14) and volume > 1.5×20-bar avg.
	last := len(candles) - 1
	for i := last; i >= last-2 && i >= 19; i-- {
		bar := candles[i]
		barRange := bar.High - bar.Low
		if barRange <= 1.5*a {
			continue
		}
		var avgVol float64
		for j := i - 19; j <= i; j++ {
			avgVol += candles[j].Volume
		}
		avgVol /= 20.0
		if avgVol <= 0 || bar.Volume <= 1.5*avgVol {
			continue
		}
		switch {
		case bar.Close < bar.Open:
			r.RecentFlashBarBearish = true
			r.RecentFlashBarAge = last - i
			r.RecentFlashBarRange = barRange
		case bar.Close > bar.Open:
			r.RecentFlashBarBullish = true
			r.RecentFlashBarAge = last - i
			r.RecentFlashBarRange = barRange
		}
		break
	}

	r.Factors = scoreFactors(&r)
	for _, f := range r.Factors {
		r.Total += f.Points
	}
	if r.Total < 0 {
		r.Total = 0
	}
	if r.Total > 10 {
		r.Total = 10
	}
	r.Verdict = Verdict(r.Total)
	return r
}

// Verdict converts a total score into the human verdict string.
func Verdict(score float64) string {
	switch {
	case score >= 8:
		return "STRONG TAKE — full size"
	case score >= 6:
		return "TAKE — default size"
	case score >= 4:
		return "NEUTRAL — discretionary, half size or wait"
	case score >= 2:
		return "WEAK — consider skipping"
	}
	return "AVOID — multiple red flags"
}

func scoreFactors(r *Result) []Factor {
	var fs []Factor

	switch {
	case r.EngineSide == r.Side:
		fs = append(fs, Factor{"direction aligned with engine", +2.0, fmt.Sprintf("engine: %s score %d", r.EngineSide, r.EngineScore)})
	case r.EngineSide == signal.Flat:
		fs = append(fs, Factor{"engine flat — neutral on direction", 0, "no engine confirmation"})
	default:
		fs = append(fs, Factor{"direction opposes engine", -2.0, fmt.Sprintf("engine: %s, you: %s", r.EngineSide, r.Side)})
	}

	if r.EngineScore >= 3 && r.EngineSide == r.Side {
		fs = append(fs, Factor{"engine has tradeable score", +1.0, fmt.Sprintf("score %d ≥ 3", r.EngineScore)})
	}

	if r.NearestSweep != nil {
		d := math.Abs(r.NearestSweep.Level-r.Entry) / r.Entry
		if d <= 0.002 {
			dirOK := (r.Side == signal.Long && r.NearestSweep.Side == analyzer.SweepLow) ||
				(r.Side == signal.Short && r.NearestSweep.Side == analyzer.SweepHigh)
			if dirOK {
				fs = append(fs, Factor{"entry at recent sweep level", +2.0, fmt.Sprintf("sweep %s @ %.4f", sweepStr(r.NearestSweep.Side), r.NearestSweep.Level)})
			} else {
				fs = append(fs, Factor{"entry at sweep but wrong side", -0.5, fmt.Sprintf("sweep %s @ %.4f doesn't support %s", sweepStr(r.NearestSweep.Side), r.NearestSweep.Level, r.Side)})
			}
		}
	}

	if r.NearestFib != nil && math.Abs(r.NearestFib.Ratio-0.618) < 1e-9 {
		d := math.Abs(r.NearestFib.Price-r.Entry) / r.Entry
		if d <= 0.003 {
			fs = append(fs, Factor{"entry at fib 0.618", +1.5, fmt.Sprintf("fib 0.618 @ %.4f", r.NearestFib.Price)})
		}
	}

	if r.BollLower != 0 {
		if r.Side == signal.Long && math.Abs(r.BollLower-r.Entry)/r.Entry <= 0.005 {
			fs = append(fs, Factor{"entry at lower Bollinger", +0.5, fmt.Sprintf("BOLL lower @ %.4f", r.BollLower)})
		}
		if r.Side == signal.Short && math.Abs(r.BollUpper-r.Entry)/r.Entry <= 0.005 {
			fs = append(fs, Factor{"entry at upper Bollinger", +0.5, fmt.Sprintf("BOLL upper @ %.4f", r.BollUpper)})
		}
	}

	if r.NearestHVN != 0 {
		d := r.NearestHVNDist / r.Entry
		if d <= 0.003 {
			if r.IsHVNPOC {
				fs = append(fs, Factor{"entry at POC", -1.0, fmt.Sprintf("POC @ %.4f — chop/equilibrium, weak mean-rev edge", r.NearestHVN)})
			} else {
				if r.Side == signal.Long && r.Entry <= r.NearestHVN {
					fs = append(fs, Factor{"entry below non-POC HVN (chip support)", +1.5, fmt.Sprintf("HVN @ %.4f acts as support", r.NearestHVN)})
				} else if r.Side == signal.Short && r.Entry >= r.NearestHVN {
					fs = append(fs, Factor{"entry above non-POC HVN (chip resistance)", +1.5, fmt.Sprintf("HVN @ %.4f acts as resistance", r.NearestHVN)})
				} else {
					fs = append(fs, Factor{"entry near HVN but wrong side for direction", -1.0, fmt.Sprintf("HVN @ %.4f fights the %s", r.NearestHVN, r.Side)})
				}
			}
		}
	}

	if r.VP.POC != 0 {
		pocDist := (r.VP.POC - r.Entry) / r.Entry
		dirToPOC := 0.0
		if r.Side == signal.Long {
			dirToPOC = pocDist
		} else {
			dirToPOC = -pocDist
		}
		absDist := math.Abs(pocDist) * 100
		switch {
		case dirToPOC >= 0.005 && dirToPOC <= 0.05:
			fs = append(fs, Factor{"POC reachable as target",
				+1.0,
				fmt.Sprintf("POC %.4f is %.2f%% %s entry — natural TP for the %s",
					r.VP.POC, absDist, sideOf(r.VP.POC, r.Entry), r.Side)})
		case dirToPOC > 0.05:
			fs = append(fs, Factor{"POC far in trade direction",
				-0.5,
				fmt.Sprintf("POC %.4f is %.1f%% away — long road to the target",
					r.VP.POC, absDist)})
		case dirToPOC < 0 && dirToPOC > -0.02:
			// POC on wrong side of entry, but close — neutral
		case dirToPOC <= -0.02:
			fs = append(fs, Factor{"fading away from chip zone",
				-0.5,
				fmt.Sprintf("POC %.4f is %.2f%% %s entry — %s is pushing away from gravity",
					r.VP.POC, absDist, sideOf(r.VP.POC, r.Entry), r.Side)})
		}
	}

	// Value-area position. Stage A weights are intentionally modest
	// (±0.5 range) — display-leaning, not yet a strong directional vote.
	// Engine-level vote on these is Stage B and requires a backtest A/B.
	if r.VAH > 0 && r.VAL > 0 {
		switch {
		case r.AtVAL && r.Side == signal.Long:
			fs = append(fs, Factor{"entry at VAL — mean-rev long",
				+0.5,
				fmt.Sprintf("VAL %.4f acts as value-area floor; POC %.4f is the natural target", r.VAL, r.VP.POC)})
		case r.AtVAH && r.Side == signal.Short:
			fs = append(fs, Factor{"entry at VAH — mean-rev short",
				+0.5,
				fmt.Sprintf("VAH %.4f acts as value-area ceiling; POC %.4f is the natural target", r.VAH, r.VP.POC)})
		case r.AtVAL && r.Side == signal.Short:
			fs = append(fs, Factor{"entry at VAL fighting value-area floor",
				-0.5,
				fmt.Sprintf("VAL %.4f is buyers' value edge — shorting into it is low-edge", r.VAL)})
		case r.AtVAH && r.Side == signal.Long:
			fs = append(fs, Factor{"entry at VAH fighting value-area ceiling",
				-0.5,
				fmt.Sprintf("VAH %.4f is sellers' value edge — longing into it is low-edge", r.VAH)})
		case r.OutsideVAUp:
			// Above value: trend mode (acceptance) candidate or fade-the-extension.
			if r.Side == signal.Short {
				fs = append(fs, Factor{"entry above VAH — extension short",
					+0.3,
					fmt.Sprintf("entry %.4f > VAH %.4f — price has rejected value; reversion candidate", r.Entry, r.VAH)})
			} else {
				fs = append(fs, Factor{"entry above VAH — chasing trend",
					-0.3,
					fmt.Sprintf("entry %.4f > VAH %.4f — late long into extension", r.Entry, r.VAH)})
			}
		case r.OutsideVADn:
			if r.Side == signal.Long {
				fs = append(fs, Factor{"entry below VAL — extension long",
					+0.3,
					fmt.Sprintf("entry %.4f < VAL %.4f — price has rejected value; reversion candidate", r.Entry, r.VAL)})
			} else {
				fs = append(fs, Factor{"entry below VAL — chasing trend",
					-0.3,
					fmt.Sprintf("entry %.4f < VAL %.4f — late short into extension", r.Entry, r.VAL)})
			}
		case r.InsideVA:
			// Inside VA without being at an edge = chop / equilibrium.
			fs = append(fs, Factor{"entry inside VA — chop zone",
				-0.3,
				fmt.Sprintf("entry %.4f within [VAL %.4f, VAH %.4f] — limited edge, mean-rev to POC", r.Entry, r.VAL, r.VAH)})
		}
	}

	// POC migration regime factor. Four-state matrix based on (price vs
	// POC_short) × (drift direction). Weights bumped slightly for the
	// "best mean-rev" setup (pullback to POC in a confirmed regime).
	if r.POCMig.Trend != indicator.POCFlat && r.POCMig.POCShort > 0 {
		driftPct := r.POCMig.DriftPct * 100
		priceAbovePOC := r.Entry > r.POCMig.POCShort
		switch r.POCMig.Trend {
		case indicator.POCRising:
			switch {
			case r.Side == signal.Long && !priceAbovePOC:
				fs = append(fs, Factor{"pullback long in rising POC regime",
					+0.7,
					fmt.Sprintf("POC drift %+.2f%% (rising), entry below POC %.4f — classic mean-rev long", driftPct, r.POCMig.POCShort)})
			case r.Side == signal.Long && priceAbovePOC:
				fs = append(fs, Factor{"trend-follow long with rising POC",
					+0.5,
					fmt.Sprintf("POC drift %+.2f%%, entry above POC %.4f — trend-aligned", driftPct, r.POCMig.POCShort)})
			case r.Side == signal.Short && priceAbovePOC:
				fs = append(fs, Factor{"short fighting rising POC regime",
					-0.5,
					fmt.Sprintf("POC drift %+.2f%% (rising), entry above POC — counter-trend short", driftPct)})
			case r.Side == signal.Short && !priceAbovePOC:
				fs = append(fs, Factor{"short below POC in rising regime",
					-0.7,
					fmt.Sprintf("POC drift %+.2f%% (rising), entry below POC %.4f — catching a falling knife in an uptrend", driftPct, r.POCMig.POCShort)})
			}
		case indicator.POCFalling:
			switch {
			case r.Side == signal.Short && priceAbovePOC:
				fs = append(fs, Factor{"bounce short in falling POC regime",
					+0.7,
					fmt.Sprintf("POC drift %+.2f%% (falling), entry above POC %.4f — classic mean-rev short", driftPct, r.POCMig.POCShort)})
			case r.Side == signal.Short && !priceAbovePOC:
				fs = append(fs, Factor{"trend-follow short with falling POC",
					+0.5,
					fmt.Sprintf("POC drift %+.2f%%, entry below POC %.4f — trend-aligned", driftPct, r.POCMig.POCShort)})
			case r.Side == signal.Long && !priceAbovePOC:
				fs = append(fs, Factor{"long fighting falling POC regime",
					-0.5,
					fmt.Sprintf("POC drift %+.2f%% (falling), entry below POC — counter-trend long", driftPct)})
			case r.Side == signal.Long && priceAbovePOC:
				fs = append(fs, Factor{"long above POC in falling regime",
					-0.7,
					fmt.Sprintf("POC drift %+.2f%% (falling), entry above POC %.4f — chasing a bounce in a downtrend", driftPct, r.POCMig.POCShort)})
			}
		}
	}

	// Entry-chase penalty. Filling LONG above the live market — or SHORT
	// below it — means you're paying a worse price than you could right now,
	// usually because the structural reason already played out. Mild chase
	// (~0.2-0.5%) is fine for fast tape; beyond that, the edge erodes fast.
	chase := (r.Entry - r.Price) / r.Price
	chasePct := chase * 100 // raw % (positive = entry above market) for display
	if r.Side == signal.Short {
		chase = -chase // flip so positive = chasing in either direction
	}
	switch {
	case chase > 0.015:
		fs = append(fs, Factor{
			"chasing market significantly",
			-2.5,
			fmt.Sprintf("entry %+.2f%% vs market — paying up for a move that already happened", chasePct),
		})
	case chase > 0.005:
		fs = append(fs, Factor{
			"chasing market",
			-1.5,
			fmt.Sprintf("entry %+.2f%% vs market — late on the move", chasePct),
		})
	case chase > 0.002:
		fs = append(fs, Factor{
			"mild market chase",
			-0.5,
			fmt.Sprintf("entry %+.2f%% vs market", chasePct),
		})
	}

	// Falling-knife / catching-the-knife penalty. If a recent range-expansion
	// bar moved hard against the proposed trade direction, the structural
	// state has likely changed and confluence votes are lagging it.
	if r.RecentFlashBarBearish && r.Side == signal.Long {
		fs = append(fs, Factor{
			"fighting recent bearish range expansion",
			-1.5,
			fmt.Sprintf("range-expansion bearish bar %d bar(s) ago (range %.4f) — falling knife risk", r.RecentFlashBarAge, r.RecentFlashBarRange),
		})
	}
	if r.RecentFlashBarBullish && r.Side == signal.Short {
		fs = append(fs, Factor{
			"fighting recent bullish range expansion",
			-1.5,
			fmt.Sprintf("range-expansion bullish bar %d bar(s) ago (range %.4f) — short-squeeze risk", r.RecentFlashBarAge, r.RecentFlashBarRange),
		})
	}

	switch {
	case r.FeeR < 0.15:
		fs = append(fs, Factor{"fee math healthy", +1.0, fmt.Sprintf("fee_R = %.2fR", r.FeeR)})
	case r.FeeR < 0.30:
		fs = append(fs, Factor{"fee math acceptable", +0.5, fmt.Sprintf("fee_R = %.2fR", r.FeeR)})
	case r.FeeR < 0.50:
		fs = append(fs, Factor{"fee math thin", 0, fmt.Sprintf("fee_R = %.2fR — needs sharp setup", r.FeeR)})
	default:
		fs = append(fs, Factor{"fee math fatal", -1.5, fmt.Sprintf("fee_R = %.2fR — fees will eat the edge", r.FeeR)})
	}

	return fs
}

func sideOf(level, ref float64) string {
	if level > ref {
		return "above"
	}
	return "below"
}

func sweepStr(s analyzer.SweepSide) string {
	if s == analyzer.SweepLow {
		return "low"
	}
	return "high"
}
