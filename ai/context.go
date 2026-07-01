package ai

import (
	"fmt"
	"strings"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/journal"
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/market"
)

// BuildTradeAnalysisMessage assembles the user-message payload sent
// alongside SystemPromptQuantAdvisor for a per-trade "Analyze" request.
// All large data sources (candles, journal history) are PRE-DIGESTED in
// Go to summary stats so the primary LLM sees compact input — the
// equivalent of running a sub-agent's mechanical extraction step but
// done in code for Phase 1 (no extra API call). Phase 2 will swap parts
// of this out for actual sub-agent LLM calls.
//
// Inputs are optional except trade itself — pass zero values for any
// data the caller couldn't fetch (e.g., live position when no API key,
// recent candles when offline). The packager skips empty sections so
// the LLM doesn't see "no data available" filler.
type TradeAnalysisInputs struct {
	Trade        journal.Trade   // the trade being analyzed (required)
	LivePosition *bingx.Position // current BingX position, nil if none / offline
	MarkPrice    float64         // 0 if not fetched
	FundingRate  float64         // fractional (e.g. 0.00005); 0 = not fetched
	RecentSame   []journal.Trade // last 5 closed trades on same symbol (for context)
	RecentBars   []market.Candle // last 50 closed candles for the trade's TF (for path summary)
	MacroNear    []macro.Event   // macro events within ±24h of trade.OpenedAt or now

	// StructureNote is the LH-LL / HH-HL classifier read on the trade's
	// TF at analysis time. Empty when unavailable.
	StructureNote string

	// TopHVNs are the top 5 high-volume nodes on the symbol's 200-bar
	// profile, pre-formatted (e.g. "60245.34 (POC)", "59012.11").
	TopHVNs []string

	// HigherTFs — parent-TF summaries for multi-TF alignment view.
	HigherTFs []HigherTFSummary
}

// BuildTradeAnalysisMessage formats the inputs into the user message
// body. The structure mirrors what a Quant peer would scribble before
// looking at a position: plan facts, what's happened since fill, the
// backtest-known context for this symbol/TF, regime hints.
func BuildTradeAnalysisMessage(in TradeAnalysisInputs) string {
	var sb strings.Builder
	sb.WriteString("# Trade analysis request\n\n")
	sb.WriteString("Please analyze the following trade per your system prompt's output structure (status snapshot → setup quality vs backtest → path → regime → verdict).\n\n")

	// --- Section: trade plan + journal metadata ---
	t := in.Trade
	sb.WriteString("## Trade plan (#")
	sb.WriteString(fmt.Sprintf("%d", t.ID))
	sb.WriteString(")\n\n")
	sb.WriteString("```\n")
	sb.WriteString(fmt.Sprintf("symbol/side/tf : %s %s / %s\n", t.Symbol, strings.ToUpper(t.Side), t.TF))
	sb.WriteString(fmt.Sprintf("entry / stop   : %.4f / %.4f (risk = %.4f pts, %.2f%% from entry)\n",
		t.Entry, t.Stop, riskDistance(t.Entry, t.Stop), pctMove(t.Stop, t.Entry)))
	sb.WriteString(fmt.Sprintf("tp1 / tp2      : %.4f (+1R, %.2f%%) / %.4f (+2R, %.2f%%)\n",
		t.TP1, pctMove(t.TP1, t.Entry), t.TP2, pctMove(t.TP2, t.Entry)))
	sb.WriteString(fmt.Sprintf("score / anchor : %s — %s\n", t.Score, t.Anchor))
	if t.SignalCtx != "" {
		sb.WriteString(fmt.Sprintf("signal_ctx     : %s\n", t.SignalCtx))
	}
	if t.Leverage > 0 {
		sb.WriteString(fmt.Sprintf("leverage       : %dx\n", t.Leverage))
	}
	if t.MarginUSDT > 0 {
		sb.WriteString(fmt.Sprintf("margin         : %.2f USDT (notional %.2f)\n", t.MarginUSDT, t.MarginUSDT*float64(t.Leverage)))
	}
	sb.WriteString(fmt.Sprintf("opened_at      : %s\n", t.OpenedAt.Local().Format("2006-01-02 15:04 -0700")))
	if !t.FilledAt.IsZero() {
		sb.WriteString(fmt.Sprintf("filled_at      : %s\n", t.FilledAt.Local().Format("2006-01-02 15:04 -0700")))
	}
	if !t.ClosedAt.IsZero() {
		sb.WriteString(fmt.Sprintf("closed_at      : %s (outcome=%s, R=%+.3f)\n", t.ClosedAt.Local().Format("2006-01-02 15:04 -0700"), t.Outcome, t.RRealized))
		if t.ExitPrice != 0 {
			sb.WriteString(fmt.Sprintf("exit_price     : %.4f\n", t.ExitPrice))
		}
	} else {
		sb.WriteString("status         : OPEN\n")
	}
	sb.WriteString("```\n\n")

	if t.OpenNotes != "" {
		sb.WriteString("**User's open notes** (verbatim — analyze against these, esp. any 'Concern' the user already flagged):\n\n")
		sb.WriteString("> ")
		sb.WriteString(strings.ReplaceAll(t.OpenNotes, "\n", "\n> "))
		sb.WriteString("\n\n")
	}
	if t.CloseNotes != "" {
		sb.WriteString("**User's close notes** (verbatim):\n\n")
		sb.WriteString("> ")
		sb.WriteString(strings.ReplaceAll(t.CloseNotes, "\n", "\n> "))
		sb.WriteString("\n\n")
	}

	// --- Section: live BingX position ---
	if in.LivePosition != nil {
		p := in.LivePosition
		sb.WriteString("## Live BingX position\n\n")
		sb.WriteString("```\n")
		sb.WriteString(fmt.Sprintf("qty       : %g %s (side=%s)\n", p.Quantity, p.Symbol, p.Side))
		sb.WriteString(fmt.Sprintf("avg fill  : %.4f\n", p.EntryPrice))
		if in.MarkPrice > 0 {
			sb.WriteString(fmt.Sprintf("mark      : %.4f (%.2f%% from avg)\n", in.MarkPrice, pctMove(in.MarkPrice, p.EntryPrice)))
			// Compute unrealized R if we have plan risk
			if r := riskDistance(t.Entry, t.Stop); r > 0 {
				var unrealR float64
				if t.Side == "long" {
					unrealR = (in.MarkPrice - p.EntryPrice) / r
				} else {
					unrealR = (p.EntryPrice - in.MarkPrice) / r
				}
				sb.WriteString(fmt.Sprintf("unreal R  : %+.2fR (plan risk = %.4f pts)\n", unrealR, r))
			}
		}
		sb.WriteString(fmt.Sprintf("leverage  : %dx\n", p.Leverage))
		sb.WriteString("```\n\n")
	}

	// --- Section: bar path summary (digest, not raw bars) ---
	if len(in.RecentBars) > 0 {
		sb.WriteString("## Recent price path (")
		sb.WriteString(fmt.Sprintf("last %d closed %s bars)\n\n", len(in.RecentBars), t.TF))
		summarizeBars(&sb, in.RecentBars, t)
		sb.WriteString("\n")
	}

	// --- Section: macro events nearby ---
	if len(in.MacroNear) > 0 {
		sb.WriteString("## Macro events within ±24h\n\n")
		for _, e := range in.MacroNear {
			sb.WriteString(fmt.Sprintf("- %s @ %s UTC (blackout %dmin before / %dmin after)\n",
				e.Name, e.DatetimeUTC.Format("2006-01-02 15:04"), e.BeforeMinutes, e.AfterMinutes))
		}
		sb.WriteString("\n")
	}

	// --- Section: funding + structure + HVN + higher-TF context ---
	if in.FundingRate != 0 || in.StructureNote != "" || len(in.TopHVNs) > 0 || len(in.HigherTFs) > 0 {
		sb.WriteString("## Live market context\n\n")
		if in.FundingRate != 0 {
			annualized := in.FundingRate * 3 * 365 * 100
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
			sb.WriteString(fmt.Sprintf("- funding: %+.5f%%/interval (~%+.2f%% annualized)%s\n",
				in.FundingRate*100, annualized, crowded))
		}
		if in.StructureNote != "" {
			sb.WriteString(fmt.Sprintf("- structure on %s: %s\n", t.TF, in.StructureNote))
		}
		if len(in.TopHVNs) > 0 {
			sb.WriteString("- top HVNs (chip zones):\n")
			for _, h := range in.TopHVNs {
				sb.WriteString(fmt.Sprintf("    · %s\n", h))
			}
		}
		if len(in.HigherTFs) > 0 {
			sb.WriteString("- higher-TF context:\n\n")
			sb.WriteString("  | TF | side | score | MR | MOM | POC drift | structure |\n")
			sb.WriteString("  |---|---|---:|---:|---:|---|---|\n")
			for _, h := range in.HigherTFs {
				sb.WriteString(fmt.Sprintf("  | %s | %s | %d | %d | %d | %s %+.2f%% | %s |\n",
					h.Timeframe, h.Side, h.Score, h.MRScore, h.MomentumScore,
					h.POCTrend, h.POCDriftPct*100, h.StructureNote))
			}
		}
		sb.WriteString("\n")
	}

	// --- Section: recent same-symbol journal entries ---
	if len(in.RecentSame) > 0 {
		sb.WriteString("## Recent ")
		sb.WriteString(string(t.Symbol))
		sb.WriteString(" journal context (last ")
		sb.WriteString(fmt.Sprintf("%d closed trades, newest first)\n\n", len(in.RecentSame)))
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
		sb.WriteString("\n**Notes excerpts** (patterns worth spotting across recent trades):\n\n")
		for _, r := range in.RecentSame {
			if r.OpenNotes == "" && r.CloseNotes == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("*#%d %s (%s, R=%+.2f)*\n", r.ID, strings.ToUpper(r.Side), r.Outcome, r.RRealized))
			if r.OpenNotes != "" {
				sb.WriteString(fmt.Sprintf("- open: %s\n", truncateOneLine(r.OpenNotes, 220)))
			}
			if r.CloseNotes != "" {
				sb.WriteString(fmt.Sprintf("- close: %s\n", truncateOneLine(r.CloseNotes, 220)))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("Analyze per the system prompt's output structure (flavor A — Trade analysis). End with one-line journal takeaway.\n")
	return sb.String()
}

// summarizeBars digests a candle slice into a compact bar-path summary
// suitable for LLM consumption. Includes peak/trough, distance to plan
// levels, and a thumbnail of the last few bars. Skips raw OHLCV dumps —
// the LLM doesn't need to re-derive what we can compute deterministically.
func summarizeBars(sb *strings.Builder, bars []market.Candle, t journal.Trade) {
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
	sb.WriteString(fmt.Sprintf("- peak       : %.4f at %s\n", hi, hiBar.OpenTime.Local().Format("01-02 15:04")))
	sb.WriteString(fmt.Sprintf("- trough     : %.4f at %s\n", lo, loBar.OpenTime.Local().Format("01-02 15:04")))

	// Relate path to plan levels.
	if r := riskDistance(t.Entry, t.Stop); r > 0 {
		var maxR, minR float64
		if t.Side == "long" {
			maxR = (hi - t.Entry) / r
			minR = (lo - t.Entry) / r
		} else {
			maxR = (t.Entry - lo) / r
			minR = (t.Entry - hi) / r
		}
		sb.WriteString(fmt.Sprintf("- peak R     : %+.2fR (max unrealized in window)\n", maxR))
		sb.WriteString(fmt.Sprintf("- trough R   : %+.2fR (max drawdown in window)\n", minR))
	}

	// Thumbnail of the last 5 bars so the LLM can spot direction/momentum.
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

func riskDistance(entry, stop float64) float64 {
	d := entry - stop
	if d < 0 {
		d = -d
	}
	return d
}

func pctMove(to, from float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}
