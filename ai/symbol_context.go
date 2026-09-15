package ai

import (
	"fmt"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// SymbolAnalysisInputs is the dashboard-level (non-trade) equivalent of
// TradeAnalysisInputs. Used for the per-symbol "AI analyze" button on
// each dashboard card — no Trade entity exists yet, only the live
// signal + validator diagnose + recent context.
type SymbolAnalysisInputs struct {
	Symbol    market.Symbol
	Short     string // BTC / ETH / XAU / XAG header label
	Timeframe market.Timeframe
	Signal    signal.Signal   // current engine output
	Diagnose  *SymbolDiagnose // current validator output, nil if absent
	Summary   string          // the rule-based one-liner (provides anchor for LLM)

	MarkPrice   float64
	FundingRate float64 // fractional (e.g. 0.00005 = 0.005%/interval)

	RecentBars []market.Candle // last N closed bars on the card's TF
	RecentSame []journal.Trade // last 5 closed trades on this symbol
	MacroNear  []macro.Event   // macro events ±24h of now

	// StructureNote is a human-readable read of the LH-LL / HH-HL
	// fractal classification on this TF (from signal.ClassifyTrendStructure).
	// Empty when not computable.
	StructureNote string

	// Structure is the full N-字 snapshot (pivot 樞紐區, trend, BOS/CHoCH,
	// invalidation, measured target) — the GROUND TRUTH the pivot-zone-fade
	// analysis reasons over. nil when not computable.
	Structure *signal.StructureState

	// HigherTFs carries digested Signal + POC read from each parent TF
	// the handler decided to fetch (typically 30m→[1h,4h] or 1h→[4h]).
	// Empty when the handler skipped higher-TF fetch (e.g. failed API).
	HigherTFs []HigherTFSummary

	// Fundamental layer (STOCKS ONLY) — the slow spot quality/valuation
	// context from the fundamental board + the F1/F2 × technical sizing
	// overlay. Flat fields (not the fundamental types) to keep ai decoupled.
	// FundLabel empty = not a stock / no rating available.
	FundLabel       string  // buy / hold / rich / avoid / unknown
	FundQuality     float64 // 0-100
	FundValuation   float64 // 0-100 (higher = cheaper)
	FundNote        string
	FundSizeLabel   string // full / half / small / skip / neutral
	FundSizeAligned string // aligned / conflict / neutral
	FundSizeWhy     string
}

// HigherTFSummary is one parent-TF snapshot for multi-TF context.
type HigherTFSummary struct {
	Timeframe     market.Timeframe
	Side          signal.Side
	Score         int
	MRScore       int
	MomentumScore int
	POCDriftPct   float64 // POC50→POC200 drift as fraction (0.01 = 1%)
	POCTrend      string  // "rising" / "falling" / "flat"
	StructureNote string  // LH-LL / HH-HL classifier on this TF
}

// SymbolDiagnose is the projection of validator.Result needed for symbol-
// level analysis. Defined here to avoid an import cycle (validator imports
// ai-related context indirectly via the signal package's Signal type).
type SymbolDiagnose struct {
	Side         signal.Side
	Entry        float64
	Total        float64
	TotalMR      float64
	TotalMOM     float64
	Verdict      string
	Factors      []SymbolDiagnoseFactor
	AtVAH        bool
	AtVAL        bool
	InsideVA     bool
	OutsideVAUp  bool
	OutsideVADn  bool
	POCTrend     indicator.POCTrend
	POCDriftPct  float64
	POCStacked   bool
	FallingKnife bool
	BlowOff      bool
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
	fmt.Fprintf(&sb, "Quick analysis of %s on %s — what's the current setup look like, should I take it?\n\n",
		in.Short, in.Timeframe)
	sb.WriteString("Follow your system prompt's output structure but adapt: there's no committed Trade yet. LEAD WITH STRUCTURE, not the engine score. Give (1) structure & regime read — trend from the SWING SEQUENCE (the label lags), the 樞紐區 fade zone, POC-drift regime, higher-TF alignment; decide trend-vs-range FIRST; (2) engine axes as CONFIRMATION or COUNTER-INDICATOR — in a clean trend the engine MR is a counter-indicator (it fades the trend), so a low MR score does NOT veto a structure-aligned setup and a high MR score does NOT endorse a counter-trend fade; in a range, MR is the primary edge; (3) decision: GO / WAIT / SKIP with sizing. Do NOT open with the MR score or the rule-based one-liner — those are engine inputs, not the anchor.\n\n")

	sig := in.Signal

	// --- Section: N-字 structure + pivot zone (GROUND TRUTH) — LEAD WITH THIS ---
	if st := in.Structure; st != nil {
		sb.WriteString("## Structure — GROUND TRUTH (reason over these FIRST; do NOT recompute or invent numbers)\n\n")
		sb.WriteString("```\n")
		fmt.Fprintf(&sb, "trend      : %s (read the swing sequence, not just this label — the label lags)\n", st.Trend.String())
		if st.Event != signal.EvNone {
			fmt.Fprintf(&sb, "event      : %s @ %.4f\n", st.Event.String(), st.EventPrice)
		}
		if st.BOSLevel > 0 {
			fmt.Fprintf(&sb, "BOS level  : %.4f (close beyond = trend continuation)\n", st.BOSLevel)
		}
		if st.Protected > 0 {
			fmt.Fprintf(&sb, "protected  : %.4f (close beyond = CHoCH / reversal)\n", st.Protected)
		}
		if z := st.Zone; z != nil {
			dir := "up — buy-the-dip"
			if z.Dir == signal.StructDowntrend {
				dir = "down — sell-the-bounce"
			}
			fmt.Fprintf(&sb, "樞紐區 zone : %s | band 0.5=%.4f .. 0.705=%.4f | invalidate %.4f | target %.4f\n",
				dir, z.Hi, z.Lo, z.Invalidate, z.Target)
			sb.WriteString("  → pivot-zone-fade entry: LIMIT inside the band (upper-middle ~0.6 default), stop past invalidate, never market-chase outside it.\n")
		} else {
			sb.WriteString("樞紐區 zone : none (leg invalidated by CHoCH / no active zone) — no fade entry here\n")
		}
		sb.WriteString("```\n\n")
	}

	// --- Section: fundamental layer (STOCKS ONLY) — slow spot context + sizing overlay ---
	if in.FundLabel != "" {
		sb.WriteString("## Fundamental layer (STOCK — slow spot quality/valuation, NOT a trade trigger)\n\n")
		sb.WriteString("```\n")
		fmt.Fprintf(&sb, "spot rating : %s (quality %.0f/100, valuation %.0f/100 — higher=cheaper)\n", in.FundLabel, in.FundQuality, in.FundValuation)
		if in.FundNote != "" {
			fmt.Fprintf(&sb, "read        : %s\n", in.FundNote)
		}
		if in.FundSizeLabel != "" && in.FundSizeLabel != "neutral" {
			fmt.Fprintf(&sb, "sizing over.: %s (%s) — %s\n", in.FundSizeLabel, in.FundSizeAligned, in.FundSizeWhy)
		}
		sb.WriteString("```\n")
		sb.WriteString("  → Use this as PERMISSION + SIZE, not timing: F1 quality gates direction (don't full-size a LONG into a deteriorating company; green-light a SHORT on one), F2 valuation scales conviction (expensive fades a long / tailwinds a short). The technical structure above still owns the ENTRY. Fundamentals move on a quarterly clock — never let them override the structural read on timing.\n\n")
	}

	// --- Section: engine signal state (confirmation / counter-indicator input) ---
	sb.WriteString("## Engine signal (confirmation / counter-indicator — NOT the lead)\n\n")
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "symbol/tf  : %s / %s\n", in.Short, in.Timeframe)
	fmt.Fprintf(&sb, "side       : %s\n", sig.Side)
	fmt.Fprintf(&sb, "score      : %d total (MR %d + MOM %d)\n", sig.Score, sig.MRScore, sig.MomentumScore)
	if in.MarkPrice > 0 {
		fmt.Fprintf(&sb, "mark       : %.4f\n", in.MarkPrice)
	}
	if sig.Plan.Entry > 0 {
		fmt.Fprintf(&sb, "plan entry : %.4f\n", sig.Plan.Entry)
		fmt.Fprintf(&sb, "plan stop  : %.4f (risk %.4f)\n", sig.Plan.StopLoss, riskDistance(sig.Plan.Entry, sig.Plan.StopLoss))
		if len(sig.Plan.TakeProfit) >= 1 {
			fmt.Fprintf(&sb, "plan tp1   : %.4f\n", sig.Plan.TakeProfit[0])
		}
		if len(sig.Plan.TakeProfit) >= 2 {
			fmt.Fprintf(&sb, "plan tp2   : %.4f\n", sig.Plan.TakeProfit[1])
		}
		fmt.Fprintf(&sb, "anchor     : %s\n", sig.Plan.Anchor)
	}
	if sig.POCMig.POCShort > 0 && sig.POCMig.Trend != indicator.POCFlat {
		stacked := ""
		if sig.POCMig.Stacked {
			stacked = " stacked"
		}
		fmt.Fprintf(&sb, "regime     : POC %s drift %+.2f%%%s\n",
			sig.POCMig.Trend.String(), sig.POCMig.DriftPct*100, stacked)
	}
	if sig.VP.POC > 0 {
		fmt.Fprintf(&sb, "POC / VA   : %.4f / [%.4f, %.4f]\n", sig.VP.POC, sig.VP.VAL, sig.VP.VAH)
	}
	if in.FundingRate != 0 {
		annualized := in.FundingRate * 3 * 365 * 100 // 3 intervals/day × 365 × pct
		crowded := ""
		switch {
		case in.FundingRate >= 0.0010:
			crowded = " ⚠ EXTREME long crowding — squeeze fuel"
		case in.FundingRate >= 0.0005:
			crowded = " ⚠ longs crowded"
		case in.FundingRate <= -0.0010:
			crowded = " ⚠ EXTREME short crowding — flush fuel"
		case in.FundingRate <= -0.0005:
			crowded = " ⚠ shorts crowded"
		}
		fmt.Fprintf(&sb, "funding    : %+.5f%%/interval (~%+.2f%% annualized)%s\n",
			in.FundingRate*100, annualized, crowded)
	}
	if in.StructureNote != "" {
		fmt.Fprintf(&sb, "structure  : %s (this TF)\n", in.StructureNote)
	}
	sb.WriteString("```\n\n")

	// --- Section: rule-based one-liner (engine's deterministic take — one input, not the anchor) ---
	if in.Summary != "" {
		sb.WriteString("## Rule-based one-line read (engine's deterministic take — one input, NOT the anchor)\n\n")
		sb.WriteString("> ")
		sb.WriteString(in.Summary)
		sb.WriteString("\n\n")
		sb.WriteString("(This is what the deterministic Go summary emitted — confirm, refine, or contradict it with the structure read above.)\n\n")
	}

	// --- Section: top HVNs (chip zones) ---
	if len(sig.VP.HVN) > 0 {
		sb.WriteString("**High-Volume Nodes** (chip zones — likely support/resistance):\n\n")
		mark := in.MarkPrice
		if mark == 0 {
			mark = sig.Price
		}
		for i, h := range sig.VP.HVN {
			if i >= 5 {
				break
			}
			rel := "at"
			if mark > 0 {
				pct := (h - mark) / mark * 100
				switch {
				case pct > 0.05:
					rel = fmt.Sprintf("%.2f%% above mark", pct)
				case pct < -0.05:
					rel = fmt.Sprintf("%.2f%% below mark", -pct)
				default:
					rel = "at mark"
				}
			}
			mark2 := ""
			if h == sig.VP.POC {
				mark2 = " ← POC"
			}
			fmt.Fprintf(&sb, "- %.4f (%s)%s\n", h, rel, mark2)
		}
		sb.WriteString("\n")
	}

	// --- Section: higher-TF context ---
	if len(in.HigherTFs) > 0 {
		sb.WriteString("**Higher-TF context** (parent-TF regime for alignment / disagreement check):\n\n")
		sb.WriteString("| TF | side | score | MR | MOM | POC drift | structure |\n")
		sb.WriteString("|---|---|---:|---:|---:|---|---|\n")
		for _, h := range in.HigherTFs {
			fmt.Fprintf(&sb, "| %s | %s | %d | %d | %d | %s %+.2f%% | %s |\n",
				h.Timeframe, h.Side, h.Score, h.MRScore, h.MomentumScore,
				h.POCTrend, h.POCDriftPct*100, h.StructureNote)
		}
		sb.WriteString("\n")
	}

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
		fmt.Fprintf(&sb, "hypothesis : %s @ %.4f\n", d.Side, d.Entry)
		fmt.Fprintf(&sb, "total /10  : %.1f → %s\n", d.Total, d.Verdict)
		fmt.Fprintf(&sb, "MR /10     : %.1f (mr + shared factors)\n", d.TotalMR)
		fmt.Fprintf(&sb, "MOM /10    : %.1f (mom + shared factors)\n", d.TotalMOM)
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
		switch d.POCTrend {
		case indicator.POCRising:
			flags = append(flags, "POC rising regime")
		case indicator.POCFalling:
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
			fmt.Fprintf(&sb, "flags      : %s\n", strings.Join(flags, " · "))
		}
		sb.WriteString("```\n\n")

		if len(d.Factors) > 0 {
			sb.WriteString("**Top contributing factors** (signed weight, axis):\n\n")
			for _, f := range d.Factors {
				if f.Points == 0 {
					continue
				}
				fmt.Fprintf(&sb, "- %+.1f [%s] %s — %s\n", f.Points, f.Axis, f.Name, f.Detail)
			}
			sb.WriteString("\n")
		}
	}

	// --- Section: recent path ---
	if len(in.RecentBars) > 0 {
		fmt.Fprintf(&sb, "## Recent price path (last %d closed %s bars)\n\n", len(in.RecentBars), in.Timeframe)
		summarizeBarsAtMark(&sb, in.RecentBars, in.MarkPrice)
		sb.WriteString("\n")
	}

	// --- Section: macro ---
	if len(in.MacroNear) > 0 {
		sb.WriteString("## Macro events within ±24h\n\n")
		for _, e := range in.MacroNear {
			fmt.Fprintf(&sb, "- %s @ %s UTC (blackout %dmin before / %dmin after)\n",
				e.Name, e.DatetimeUTC.Format("2006-01-02 15:04"), e.BeforeMinutes, e.AfterMinutes)
		}
		sb.WriteString("\n")
	}

	// --- Section: recent same-symbol journal ---
	if len(in.RecentSame) > 0 {
		fmt.Fprintf(&sb, "## Recent %s journal context (last %d closed, newest first)\n\n", in.Short, len(in.RecentSame))
		sb.WriteString("| # | side/tf | score | outcome | R | open→close |\n")
		sb.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range in.RecentSame {
			dur := ""
			if !r.ClosedAt.IsZero() && !r.OpenedAt.IsZero() {
				dur = r.ClosedAt.Sub(r.OpenedAt).Round(time.Minute).String()
			}
			fmt.Fprintf(&sb, "| %d | %s/%s | %s | %s | %+.2f | %s |\n",
				r.ID, r.Side, r.TF, r.Score, r.Outcome, r.RRealized, dur)
		}
		sb.WriteString("\n**Notes excerpts** (patterns worth spotting across recent trades):\n\n")
		for _, r := range in.RecentSame {
			if r.OpenNotes == "" && r.CloseNotes == "" {
				continue
			}
			fmt.Fprintf(&sb, "*#%d %s %s (%s, R=%+.2f)*\n", r.ID, r.Symbol, strings.ToUpper(r.Side), r.Outcome, r.RRealized)
			if r.OpenNotes != "" {
				fmt.Fprintf(&sb, "- open: %s\n", truncateOneLine(r.OpenNotes, 220))
			}
			if r.CloseNotes != "" {
				fmt.Fprintf(&sb, "- close: %s\n", truncateOneLine(r.CloseNotes, 220))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("End with a one-line takeaway suitable for the dashboard summary (replace or refine the rule-based one-liner above).\n")
	return sb.String()
}

// truncateOneLine collapses newlines to " · " and truncates to n chars
// with an ellipsis. Used for journal-note excerpts so the LLM sees the
// gist without the full multi-line notes bloating context.
func truncateOneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " · ")
	s = strings.ReplaceAll(s, "  ", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// summarizeBarsAtMark is the no-trade variant of summarizeBars. Same
// window stats, but R-anchored sections drop out since there's no plan.
func summarizeBarsAtMark(sb *strings.Builder, bars []market.Candle, mark float64) {
	if len(bars) == 0 {
		return
	}
	first := bars[0]
	last := bars[len(bars)-1]
	hi, lo := -1e18, 1e18
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
	fmt.Fprintf(sb, "- window     : %s → %s\n",
		first.OpenTime.Local().Format("2006-01-02 15:04"),
		last.CloseTime.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(sb, "- first/last : O=%.4f C=%.4f (%.2f%% net move)\n", first.Open, last.Close, pctMove(last.Close, first.Open))
	fmt.Fprintf(sb, "- peak       : %.4f at %s (%.2f%% from mark)\n", hi, hiBar.OpenTime.Local().Format("01-02 15:04"), pctMove(hi, mark))
	fmt.Fprintf(sb, "- trough     : %.4f at %s (%.2f%% from mark)\n", lo, loBar.OpenTime.Local().Format("01-02 15:04"), pctMove(lo, mark))

	tail := bars
	if len(tail) > 5 {
		tail = tail[len(tail)-5:]
	}
	sb.WriteString("- last 5 bars:\n")
	for _, b := range tail {
		fmt.Fprintf(sb, "    %s  O=%.4f H=%.4f L=%.4f C=%.4f V=%.0f\n",
			b.OpenTime.Local().Format("01-02 15:04"), b.Open, b.High, b.Low, b.Close, b.Volume)
	}
}
