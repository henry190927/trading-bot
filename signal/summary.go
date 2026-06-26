package signal

import (
	"fmt"
	"strings"

	"myFirstGo/trading-bot/indicator"
)

// Summary is a pure-function rule-based one-line description of a symbol's
// current state. Designed for the dashboard card to surface a human-readable
// take without an LLM call.
//
// Output format (English, ~80–120 chars):
//
//	"<regime context> · <score breakdown> · <validator verdict> · <suggested action>"
//
// Examples:
//
//	"falling POC -4.98% + stacked regime · 2 MR + 0 MOM (below MIN_SCORE=3) · 4.9/10 NEUTRAL · LOW-CONVICTION SHORT — discretionary or wait"
//	"sweep-high anchor · 1 MR + 0 MOM · 6.4/10 TAKE · MEAN-REV SHORT candidate — vote count thin"
//	"flat — no setup · 0 MR + 0 MOM · — · NO TRADE"
//
// diag may be nil (no validator result available); the summary degrades
// gracefully to engine-only signal.
func BuildSummary(sig Signal, diag DiagnoseView) string {
	parts := []string{}

	if reg := regimeFragment(sig); reg != "" {
		parts = append(parts, reg)
	}
	parts = append(parts, scoreFragment(sig))
	if diag.Has {
		parts = append(parts, fmt.Sprintf("%.1f/10 %s", diag.Total, diag.VerdictShort))
	}
	parts = append(parts, suggestionFragment(sig, diag))

	return strings.Join(parts, " · ")
}

// DiagnoseView is a minimal projection of validator.Result fields that
// BuildSummary needs. Defined here to avoid an import cycle
// (validator imports signal; signal can't import validator). Callers
// translate from validator.Result.
type DiagnoseView struct {
	Has          bool   // false when no validator result is available
	Side         Side   // validator's recommended side
	Total        float64
	TotalMR      float64
	TotalMOM     float64
	VerdictShort string // "STRONG TAKE", "TAKE", "NEUTRAL", "WEAK", "AVOID"
}

// regimeFragment surfaces what the macro / regime picture is doing.
// Emits empty string when nothing notable.
func regimeFragment(sig Signal) string {
	bits := []string{}

	// POC migration drift (always informative when non-flat).
	if sig.POCMig.POCShort > 0 && sig.POCMig.Trend != indicator.POCFlat {
		arrow := "→"
		switch sig.POCMig.Trend {
		case indicator.POCRising:
			arrow = "↗"
		case indicator.POCFalling:
			arrow = "↘"
		}
		stacked := ""
		if sig.POCMig.Stacked {
			stacked = " stacked"
		}
		bits = append(bits, fmt.Sprintf("POC %s %+.2f%%%s", arrow, sig.POCMig.DriftPct*100, stacked))
	}

	// Sweep / anchor surface — read the first Reason that signals an anchor.
	for _, r := range sig.Reasons {
		low := strings.ToLower(r)
		switch {
		case strings.Contains(low, "sweep low"):
			bits = append(bits, "sweep-low anchor")
		case strings.Contains(low, "sweep high"):
			bits = append(bits, "sweep-high anchor")
		case strings.Contains(low, "fib 0.618"):
			bits = append(bits, "fib 0.618 anchor")
		case strings.Contains(low, "double top"):
			bits = append(bits, "double-top retest")
		case strings.Contains(low, "double bottom"):
			bits = append(bits, "double-bottom retest")
		case strings.Contains(low, "hh-hl"):
			bits = append(bits, "HH-HL uptrend")
		case strings.Contains(low, "lh-ll"):
			bits = append(bits, "LH-LL downtrend")
		default:
			continue
		}
		break // first match wins
	}

	return strings.Join(bits, " + ")
}

// scoreFragment renders the tri-axis vote count, flagged when below the
// engine's MIN_SCORE threshold of 3.
func scoreFragment(sig Signal) string {
	tag := ""
	if sig.Score < 3 {
		tag = " (below MIN_SCORE=3)"
	}
	return fmt.Sprintf("%d MR + %d MOM%s", sig.MRScore, sig.MomentumScore, tag)
}

// suggestionFragment is the actionable read — what to DO given the
// state. Designed to be terse and decision-shaped (not narrative).
func suggestionFragment(sig Signal, diag DiagnoseView) string {
	// Side resolution: trust engine unless validator strongly disagrees.
	side := sig.Side
	if side == Flat && diag.Has {
		side = diag.Side
	}

	if side == Flat {
		return "NO TRADE — flat"
	}

	sideTxt := "LONG"
	if side == Short {
		sideTxt = "SHORT"
	}

	// Classify by the leading axis so the suggestion communicates style.
	style := "MEAN-REV"
	if sig.MomentumScore > sig.MRScore {
		style = "MOMENTUM"
	} else if sig.MomentumScore > 0 && sig.MomentumScore == sig.MRScore {
		style = "MIXED MR/MOM"
	}

	// Axis-disagreement warning supersedes the normal suggestion: a
	// Warnings entry with "Axis disagreement" implies MR and MOM voted
	// opposite. Caller already surfaces the Warnings; here we just flag.
	for _, w := range sig.Warnings {
		if strings.Contains(strings.ToLower(w), "axis disagreement") {
			return fmt.Sprintf("CAUTION — axis disagreement, %s only as fast scalp", sideTxt)
		}
	}

	// Confidence band: combines vote count + validator verdict.
	switch {
	case sig.Score >= 4 && diag.Has && diag.Total >= 7.5:
		return fmt.Sprintf("FULL-SIZE %s %s — strong confluence", style, sideTxt)
	case sig.Score >= 3 && diag.Has && diag.Total >= 5.5:
		return fmt.Sprintf("DEFAULT %s %s — TAKE band", style, sideTxt)
	case sig.Score >= 3:
		return fmt.Sprintf("%s %s — meets MIN_SCORE, validator soft", style, sideTxt)
	case diag.Has && diag.Total >= 5.5:
		return fmt.Sprintf("LOW-CONVICTION %s %s — discretionary, validator carrying it", style, sideTxt)
	default:
		return fmt.Sprintf("WAIT — %s lean too thin to act", sideTxt)
	}
}
