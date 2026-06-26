package ai

import (
	"fmt"
	"strings"
	"time"

	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/journal"
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// SymbolAnalysisInputs is the dashboard-level (non-trade) equivalent of
// TradeAnalysisInputs. Used for the per-symbol "AI analyze" button on
// each dashboard card — no Trade entity exists yet, only the live
// signal + validator diagnose + recent context.
type SymbolAnalysisInputs struct {
	Symbol    market.Symbol
	Short     string             // BTC / ETH / XAU / XAG header label
	Timeframe market.Timeframe
	Signal    signal.Signal      // current engine output
	Diagnose  *SymbolDiagnose    // current validator output, nil if absent
	Summary   string             // the rule-based one-liner (provides anchor for LLM)

	MarkPrice  float64
	RecentBars []market.Candle  // last N closed bars on the card's TF
	RecentSame []journal.Trade  // last 5 closed trades on this symbol
	MacroNear  []macro.Event    // macro events ±24h of now
}

// SymbolDiagnose is the projection of validator.Result needed for symbol-
// level analysis. Defined here to avoid an import cycle (validator imports
// ai-related context indirectly via the signal package's Signal type).
type SymbolDiagnose struct {
	Side          signal.Side
	Entry         float64
	Total         float64
	TotalMR       float64
	TotalMOM      float64
	Verdict       string
	Factors       []SymbolDiagnoseFactor
	AtVAH         bool
	AtVAL         bool
	InsideVA      bool
	OutsideVAUp   bool
	OutsideVADn   bool
	POCTrend      indicator.POCTrend
	POCDriftPct   float64
	POCStacked    bool
	FallingKnife  bool
	BlowOff       bool
}

// SymbolDiagnoseFactor mirrors validator.Factor for the LLM context.
type SymbolDiagnoseFactor struct {
	Name   string
	Points float64
	Detail string
	Axis   string
}

// BuildSymbolAnalysisMessage formats the inputs into a user message for
// the per-symbol Analyze button. Mirrors BuildTradeAnalysisMessage's
// structure but pivots around "current setup" instead of "this trade".
func BuildSymbolAnalysisMessage(in SymbolAnalysisInputs) string {
	var sb strings.Builder
	sb.WriteString("# Symbol analysis request\n\n")
	sb.WriteString(fmt.Sprintf("Quick analysis of %s on %s — what's the current setup look like, should I take it?\n\n",
		in.Short, in.Timeframe))
	sb.WriteString("Follow your system prompt's output structure but adapt: there's no committed Trade yet — instead give (1) current setup quality, (2) regime context, (3) decision: GO / WAIT / SKIP with sizing suggestion.\n\n")

	// --- Section: rule-based one-liner as the anchor ---
	if in.Summary != "" {
		sb.WriteString("## Rule-based one-line read\n\n")
		sb.WriteString("> ")
		sb.WriteString(in.Summary)
		sb.WriteString("\n\n")
		sb.WriteString("(This is what the deterministic Go summary emitted — confirm, refine, or contradict it with deeper context.)\n\n")
	}

	// --- Section: engine signal state ---
	sig := in.Signal
	sb.WriteString("## Engine signal\n\n")
	sb.WriteString("```\n")
	sb.WriteString(fmt.Sprintf("symbol/tf  : %s / %s\n", in.Short, in.Timeframe))
	sb.WriteString(fmt.Sprintf("side       : %s\n", sig.Side))
	sb.WriteString(fmt.Sprintf("score      : %d total (MR %d + MOM %d)\n", sig.Score, sig.MRScore, sig.MomentumScore))
	if in.MarkPrice > 0 {
		sb.WriteString(fmt.Sprintf("mark       : %.4f\n", in.MarkPrice))
	}
	if sig.Plan.Entry > 0 {
		sb.WriteString(fmt.Sprintf("plan entry : %.4f\n", sig.Plan.Entry))
		sb.WriteString(fmt.Sprintf("plan stop  : %.4f (risk %.4f)\n", sig.Plan.StopLoss, riskDistance(sig.Plan.Entry, sig.Plan.StopLoss)))
		if len(sig.Plan.TakeProfit) >= 1 {
			sb.WriteString(fmt.Sprintf("plan tp1   : %.4f\n", sig.Plan.TakeProfit[0]))
		}
		if len(sig.Plan.TakeProfit) >= 2 {
			sb.WriteString(fmt.Sprintf("plan tp2   : %.4f\n", sig.Plan.TakeProfit[1]))
		}
		sb.WriteString(fmt.Sprintf("anchor     : %s\n", sig.Plan.Anchor))
	}
	if sig.POCMig.POCShort > 0 && sig.POCMig.Trend != indicator.POCFlat {
		stacked := ""
		if sig.POCMig.Stacked {
			stacked = " stacked"
		}
		sb.WriteString(fmt.Sprintf("regime     : POC %s drift %+.2f%%%s\n",
			sig.POCMig.Trend.String(), sig.POCMig.DriftPct*100, stacked))
	}
	if sig.VP.POC > 0 {
		sb.WriteString(fmt.Sprintf("POC / VA   : %.4f / [%.4f, %.4f]\n", sig.VP.POC, sig.VP.VAL, sig.VP.VAH))
	}
	sb.WriteString("```\n\n")

	if len(sig.Reasons) > 0 {
		sb.WriteString("**Votes that fired** (engine confluence, [MR]/[MOM] tagged):\n\n")
		for _, r := range sig.Reasons {
			sb.WriteString("- ")
			sb.WriteString(r)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if len(sig.Notes) > 0 {
		sb.WriteString("**Notes** (observations that didn't vote):\n\n")
		for _, n := range sig.Notes {
			sb.WriteString("- ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if len(sig.Warnings) > 0 {
		sb.WriteString("**Warnings** (crowd / funding / axis-disagreement flags):\n\n")
		for _, w := range sig.Warnings {
			sb.WriteString("- ")
			sb.WriteString(w)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// --- Section: validator diagnose ---
	if d := in.Diagnose; d != nil {
		sb.WriteString("## Validator diagnose\n\n")
		sb.WriteString("```\n")
		sb.WriteString(fmt.Sprintf("hypothesis : %s @ %.4f\n", d.Side, d.Entry))
		sb.WriteString(fmt.Sprintf("total /10  : %.1f → %s\n", d.Total, d.Verdict))
		sb.WriteString(fmt.Sprintf("MR /10     : %.1f (mr + shared factors)\n", d.TotalMR))
		sb.WriteString(fmt.Sprintf("MOM /10    : %.1f (mom + shared factors)\n", d.TotalMOM))
		flags := []string{}
		switch {
		case d.AtVAH:
			flags = append(flags, "at VAH")
		case d.AtVAL:
			flags = append(flags, "at VAL")
		case d.OutsideVAUp:
			flags = append(flags, "above VA (extension)")
		case d.OutsideVADn:
			flags = append(flags, "below VA (extension)")
		case d.InsideVA:
			flags = append(flags, "inside VA (chop)")
		}
		if d.POCTrend == indicator.POCRising {
			flags = append(flags, "POC rising regime")
		} else if d.POCTrend == indicator.POCFalling {
			flags = append(flags, "POC falling regime")
		}
		if d.POCStacked {
			flags = append(flags, "stacked POC")
		}
		if d.FallingKnife {
			flags = append(flags, "⚠ recent falling knife")
		}
		if d.BlowOff {
			flags = append(flags, "⚠ recent blow-off")
		}
		if len(flags) > 0 {
			sb.WriteString(fmt.Sprintf("flags      : %s\n", strings.Join(flags, " · ")))
		}
		sb.WriteString("```\n\n")

		if len(d.Factors) > 0 {
			sb.WriteString("**Top contributing factors** (signed weight, axis):\n\n")
			for _, f := range d.Factors {
				if f.Points == 0 {
					continue
				}
				sb.WriteString(fmt.Sprintf("- %+.1f [%s] %s — %s\n", f.Points, f.Axis, f.Name, f.Detail))
			}
			sb.WriteString("\n")
		}
	}

	// --- Section: recent path ---
	if len(in.RecentBars) > 0 {
		sb.WriteString(fmt.Sprintf("## Recent price path (last %d closed %s bars)\n\n", len(in.RecentBars), in.Timeframe))
		summarizeBarsAtMark(&sb, in.RecentBars, in.MarkPrice)
		sb.WriteString("\n")
	}

	// --- Section: macro ---
	if len(in.MacroNear) > 0 {
		sb.WriteString("## Macro events within ±24h\n\n")
		for _, e := range in.MacroNear {
			sb.WriteString(fmt.Sprintf("- %s @ %s UTC (blackout %dmin before / %dmin after)\n",
				e.Name, e.DatetimeUTC.Format("2006-01-02 15:04"), e.BeforeMinutes, e.AfterMinutes))
		}
		sb.WriteString("\n")
	}

	// --- Section: recent same-symbol journal ---
	if len(in.RecentSame) > 0 {
		sb.WriteString(fmt.Sprintf("## Recent %s journal context (last %d closed, newest first)\n\n", in.Short, len(in.RecentSame)))
		sb.WriteString("| # | side/tf | score | outcome | R | open→close |\n")
		sb.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range in.RecentSame {
			dur := ""
			if !r.ClosedAt.IsZero() && !r.OpenedAt.IsZero() {
				dur = r.ClosedAt.Sub(r.OpenedAt).Round(time.Minute).String()
			}
			sb.WriteString(fmt.Sprintf("| %d | %s/%s | %s | %s | %+.2f | %s |\n",
				r.ID, r.Side, r.TF, r.Score, r.Outcome, r.RRealized, dur))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("End with a one-line takeaway suitable for the dashboard summary (replace or refine the rule-based one-liner above).\n")
	return sb.String()
}

// summarizeBarsAtMark is the no-trade variant of summarizeBars. Same
// window stats, but R-anchored sections drop out since there's no plan.
func summarizeBarsAtMark(sb *strings.Builder, bars []market.Candle, mark float64) {
	if len(bars) == 0 {
		return
	}
	first := bars[0]
	last := bars[len(bars)-1]
	var hi, lo float64 = -1e18, 1e18
	var hiBar, loBar market.Candle
	for _, b := range bars {
		if b.High > hi {
			hi = b.High
			hiBar = b
		}
		if b.Low < lo {
			lo = b.Low
			loBar = b
		}
	}
	sb.WriteString(fmt.Sprintf("- window     : %s → %s\n",
		first.OpenTime.Local().Format("2006-01-02 15:04"),
		last.CloseTime.Local().Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("- first/last : O=%.4f C=%.4f (%.2f%% net move)\n", first.Open, last.Close, pctMove(last.Close, first.Open)))
	sb.WriteString(fmt.Sprintf("- peak       : %.4f at %s (%.2f%% from mark)\n", hi, hiBar.OpenTime.Local().Format("01-02 15:04"), pctMove(hi, mark)))
	sb.WriteString(fmt.Sprintf("- trough     : %.4f at %s (%.2f%% from mark)\n", lo, loBar.OpenTime.Local().Format("01-02 15:04"), pctMove(lo, mark)))

	tail := bars
	if len(tail) > 5 {
		tail = tail[len(tail)-5:]
	}
	sb.WriteString("- last 5 bars:\n")
	for _, b := range tail {
		sb.WriteString(fmt.Sprintf("    %s  O=%.4f H=%.4f L=%.4f C=%.4f V=%.0f\n",
			b.OpenTime.Local().Format("01-02 15:04"), b.Open, b.High, b.Low, b.Close, b.Volume))
	}
}
