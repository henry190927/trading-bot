package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/ai"
	"github.com/henry190927/trading-bot/fundamental"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
	"github.com/henry190927/trading-bot/tools"
	"github.com/henry190927/trading-bot/validator"
)

// The context the three page-level "AI analysis" buttons used to send to the
// model, now served as copilot tools (docs/ai_copilot_design.md P2). The
// builders are the buttons' code, unchanged apart from returning the text
// instead of calling the model: the copilot answers from the same facts.

// tradeAnalysisContext is the context for reviewing journal trade id: plan,
// live position, mark and funding, 50 bars plus structure, top volume nodes,
// higher timeframes, the last five closed trades on the symbol, and macro
// events within ±24h of the open.
func (s *server) tradeAnalysisContext(ctx context.Context, id int) (string, error) {
	trades, err := journal.ReadAll("")
	if err != nil {
		return "", fmt.Errorf("read journal: %w", err)
	}
	idx := journal.FindByID(trades, id)
	if idx < 0 {
		return "", fmt.Errorf("no trade with id %d", id)
	}
	t := trades[idx]
	inputs := ai.TradeAnalysisInputs{Trade: t}

	if sym, err := resolveWebSymbol(t.Symbol); err == nil && s.client != nil {
		if pos, perr := s.client.FindOpenPosition(ctx, sym, t.Side); perr == nil && pos != nil {
			inputs.LivePosition = pos
		}
		if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil {
			inputs.MarkPrice = fr.MarkPrice
			inputs.FundingRate = fr.Rate
		}
		tfStr := t.TF
		if j := strings.Index(tfStr, ","); j >= 0 {
			tfStr = strings.TrimSpace(tfStr[:j])
		}
		if tfStr != "" {
			tf := market.Timeframe(tfStr)
			if candles, cerr := s.client.Klines(ctx, sym, tf, 200); cerr == nil && len(candles) >= 60 {
				inputs.RecentBars = candles[len(candles)-50:]
				inputs.StructureNote = structureNoteFor(candles)
				sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: candles})
				inputs.TopHVNs = topHVNList(sig.VP.HVN, sig.VP.POC, inputs.MarkPrice)
				inputs.HigherTFs = s.buildHigherTFSummaries(ctx, sym, tf)
			}
		}
	}
	inputs.RecentSame = recentClosedOn(trades, t.Symbol, t.ID)
	anchor := t.OpenedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	inputs.MacroNear = macroNear(anchor)
	return ai.BuildTradeAnalysisMessage(inputs), nil
}

// symbolAnalysisContext is the context for analysing short on tf: the
// engine's signal and diagnosis (the same scan the dashboard renders),
// structure, higher timeframes, funding, the fundamental overlay for
// stocks, recent same-symbol trades and macro events near now.
func (s *server) symbolAnalysisContext(ctx context.Context, short, tfStr string) (string, error) {
	short = strings.ToUpper(strings.TrimSpace(short))
	sym, err := resolveWebSymbol(short)
	if err != nil {
		return "", err
	}
	tf := market.Timeframe(strings.TrimSpace(tfStr))
	view := s.scanOne(ctx, sym, tf)
	if view.Err != "" {
		return "", fmt.Errorf("scan failed: %s", view.Err)
	}
	var fundingRate float64
	if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil {
		fundingRate = fr.Rate
	}
	in := ai.SymbolAnalysisInputs{
		Symbol:        sym,
		Short:         short,
		Timeframe:     tf,
		Signal:        view.Signal,
		Summary:       view.Summary,
		MarkPrice:     view.MarkPrice,
		FundingRate:   fundingRate,
		RecentBars:    view.Candles,
		StructureNote: structureNoteFor(view.Candles),
		Structure:     analyzeStructForAI(view.Candles),
		HigherTFs:     s.buildHigherTFSummaries(ctx, sym, tf),
	}
	// Fundamental overlay (stocks only; a no-op for symbols not on the board).
	if board, errB := fundamental.LoadBoard(fundamentalsPath()); errB == nil {
		if e, ok := board.Find(short); ok {
			in.FundLabel = e.Label
			if e.Rich {
				in.FundLabel = "rich"
			}
			in.FundQuality = e.Quality
			in.FundValuation = e.Valuation
			in.FundNote = firstNote(e.Notes)
			hint := fundamental.SuggestSizing(techSideStr(view.Signal.Side), e.Rating)
			in.FundSizeLabel = hint.Label
			in.FundSizeAligned = hint.Aligned
			in.FundSizeWhy = hint.Rationale
		}
	}
	if view.Diagnose != nil {
		in.Diagnose = symbolDiagnoseForAI(view.Diagnose)
	}
	all, _ := journal.ReadAll("")
	in.RecentSame = recentClosedOn(all, short, 0)
	in.MacroNear = macroNear(time.Now())
	return ai.BuildSymbolAnalysisMessage(in), nil
}

// validateSetupContext is the context for judging a proposed entry: the
// validator's diagnosis of (side, entry) on 200 bars, the engine's own read
// of the same bars, structure, higher timeframes, funding, recent trades
// and macro events.
func (s *server) validateSetupContext(ctx context.Context, short, tfStr, sideStr string, entry float64) (string, error) {
	short = strings.ToUpper(strings.TrimSpace(short))
	sym, err := resolveWebSymbol(short)
	if err != nil {
		return "", err
	}
	tf := market.Timeframe(strings.TrimSpace(tfStr))
	sideStr = strings.ToLower(strings.TrimSpace(sideStr))
	var side signal.Side
	switch sideStr {
	case "long":
		side = signal.Long
	case "short":
		side = signal.Short
	default:
		return "", errors.New("side must be long or short")
	}
	if entry <= 0 {
		return "", errors.New("entry must be a positive number")
	}
	candles, err := s.client.Klines(ctx, sym, tf, 200)
	if err != nil {
		return "", fmt.Errorf("klines: %w", err)
	}
	if len(candles) < 60 {
		return "", fmt.Errorf("only %d bars of %s %s; need 60", len(candles), short, tf)
	}
	var markPrice, fundingRate float64
	if fr, ferr := s.client.FundingRate(ctx, sym); ferr == nil {
		markPrice, fundingRate = fr.MarkPrice, fr.Rate
	}
	const dashboardFeeBps = 6.0
	d := validator.Validate(sym, tf, side, entry, dashboardFeeBps, candles, markPrice)
	sig := signal.Evaluate(signal.Inputs{Symbol: sym, Timeframe: tf, Candles: candles, LiveMarkPrice: markPrice})
	ruleSummary := signal.BuildSummary(sig, diagnoseView(&d))
	in := ai.SymbolAnalysisInputs{
		Symbol:        sym,
		Short:         short,
		Timeframe:     tf,
		Signal:        sig,
		Summary:       fmt.Sprintf("USER PROPOSAL — %s %s @ %.4f. (Engine's own read: %s)", sideStr, short, entry, ruleSummary),
		MarkPrice:     markPrice,
		FundingRate:   fundingRate,
		RecentBars:    candles,
		StructureNote: structureNoteFor(candles),
		HigherTFs:     s.buildHigherTFSummaries(ctx, sym, tf),
		Diagnose:      symbolDiagnoseForAI(&d),
	}
	all, _ := journal.ReadAll("")
	in.RecentSame = recentClosedOn(all, short, 0)
	in.MacroNear = macroNear(time.Now())
	return ai.BuildSymbolAnalysisMessage(in), nil
}

// symbolDiagnoseForAI flattens a validator diagnosis for the prompt.
func symbolDiagnoseForAI(d *validator.Result) *ai.SymbolDiagnose {
	sd := &ai.SymbolDiagnose{
		Side: d.Side, Entry: d.Entry, Total: d.Total, TotalMR: d.TotalMR, TotalMOM: d.TotalMOM,
		Verdict: d.Verdict,
		AtVAH:   d.AtVAH, AtVAL: d.AtVAL, InsideVA: d.InsideVA,
		OutsideVAUp: d.OutsideVAUp, OutsideVADn: d.OutsideVADn,
		POCTrend: d.POCMig.Trend, POCDriftPct: d.POCMig.DriftPct, POCStacked: d.POCMig.Stacked,
		FallingKnife: d.RecentFlashBarBearish, BlowOff: d.RecentFlashBarBullish,
	}
	for _, f := range d.Factors {
		sd.Factors = append(sd.Factors, ai.SymbolDiagnoseFactor{Name: f.Name, Points: f.Points, Detail: f.Detail, Axis: f.Axis})
	}
	return sd
}

// recentClosedOn returns up to five closed trades on symbol, newest first,
// skipping trade skipID.
func recentClosedOn(trades []journal.Trade, symbol string, skipID int) []journal.Trade {
	var out []journal.Trade
	for i := len(trades) - 1; i >= 0 && len(out) < 5; i-- {
		r := trades[i]
		if r.ID == skipID || r.Symbol != symbol || r.ClosedAt.IsZero() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// macroNear returns macro events within ±24h of t.
func macroNear(t time.Time) []macro.Event {
	var out []macro.Event
	for _, e := range macro.All() {
		if d := e.DatetimeUTC.Sub(t); d >= -24*time.Hour && d <= 24*time.Hour {
			out = append(out, e)
		}
	}
	return out
}

// webAnalysisTools are the copilot tools only the web can serve (they need
// the engine scan and the validator). get_trade replaces the shared, thinner
// MCP version of the same name.
func (s *server) webAnalysisTools() []tools.Tool {
	hint := strings.Join(uiSymbols, ", ")
	return []tools.Tool{
		{
			Name:        "get_trade",
			Description: "Returns one journal trade by ID with everything needed to review it: plan (entry/SL/TP), notes, outcome and realized R, the live position and mark, the last 50 bars with structure, top volume nodes, higher timeframes, the last five closed trades on the symbol, and macro events near the open. Use it whenever the user refers to a trade number or asks to review a trade.",
			Params:      []tools.Param{{Name: "id", Type: tools.Number, Description: "Journal row ID", Required: true}},
			Kind:        tools.Read,
			Run: func(ctx context.Context, a tools.Args) (string, error) {
				id, ok := a.Number("id")
				if !ok {
					return "", errors.New("id (number) is required")
				}
				return s.tradeAnalysisContext(ctx, int(id))
			},
		},
		{
			Name:        "analyze_symbol",
			Description: "Runs the engine on one symbol and timeframe and returns the full analysis context: score and votes, diagnosis factors, structure (trend, BOS/CHoCH, pivot zone), volume profile, higher timeframes, funding, recent trades on the symbol and nearby macro events. Use it for 'analyze BTC 1h', 'is there a setup on ETH', or any question about a symbol's current state.",
			Params: []tools.Param{
				{Name: "symbol", Type: tools.String, Description: hint, Required: true},
				{Name: "tf", Type: tools.String, Description: "Timeframe: 5m, 15m, 30m, 1h, 2h, 4h, 1d", Required: true},
			},
			Kind: tools.Read,
			Run: func(ctx context.Context, a tools.Args) (string, error) {
				return s.symbolAnalysisContext(ctx, a.String("symbol"), a.String("tf"))
			},
		},
		{
			Name:        "validate_setup",
			Description: "Scores a proposed entry (symbol, timeframe, side, entry price) with the validator and returns its verdict and factors next to the engine's own read, structure, higher timeframes and recent trades. Use it when the user asks whether an entry is good, or wants a plan checked before placing it.",
			Params: []tools.Param{
				{Name: "symbol", Type: tools.String, Description: hint, Required: true},
				{Name: "tf", Type: tools.String, Description: "Timeframe: 5m, 15m, 30m, 1h, 2h, 4h, 1d", Required: true},
				{Name: "side", Type: tools.String, Description: "long or short", Required: true, Enum: []string{"long", "short"}},
				{Name: "entry", Type: tools.Number, Description: "Proposed entry price", Required: true},
			},
			Kind: tools.Read,
			Run: func(ctx context.Context, a tools.Args) (string, error) {
				entry, _ := a.Number("entry")
				return s.validateSetupContext(ctx, a.String("symbol"), a.String("tf"), a.String("side"), entry)
			},
		},
	}
}
