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
func Validate(sym market.Symbol, tf market.Timeframe, side signal.Side, entry, feeBps float64, candles []market.Candle) Result {
	closes := market.Closes(candles)
	price := closes[len(closes)-1]

	sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: candles})

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
