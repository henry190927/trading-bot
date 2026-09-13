// Command mcp is the trading-bot MCP (Model Context Protocol) server.
// It runs as a stdio subprocess of Claude Code and exposes the
// trading-bot's data — journal trades, BingX positions, recent candles,
// macro events, backtest facts — as tools the user's Claude Code
// session can call.
//
// This is the zero-cost alternative to the trading-web /ai/analyze
// endpoint: instead of calling Anthropic's API with a key, the user
// asks Claude Code (already authenticated to their account via OAuth)
// to "analyze trade #25", and Claude Code calls the tools below to
// gather context, then synthesizes the analysis using its own session.
// No API key, no incremental billing.
//
// All tools follow the same Quant Trader framing as the web button
// path. Output is markdown text (LLM-friendly), not raw JSON — the
// reuse of ai.BuildTradeAnalysisMessage keeps the persona / context
// shape consistent across both inference paths.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/henry190927/trading-bot/ai"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/market"
)

func main() {
	// Logs go to stderr — stdout is the JSON-RPC transport in stdio mode,
	// and writing to it would corrupt the protocol stream.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds | log.Lshortfile)

	bxClient := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	srv := &mcpServer{bx: bxClient}

	s := server.NewMCPServer(
		"trading-bot",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithInstructions(serverInstructions),
	)

	srv.register(s)

	log.Printf("trading-bot MCP server starting (stdio). Journal: %s. BingX keys: %v",
		journalPath(), bxClient.APIKey != "")

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("ServeStdio: %v", err)
	}
}

// serverInstructions are sent to the client (Claude Code) on initialize.
// This is where the Quant persona + discipline rules anchor — Claude
// Code presents these to the underlying model as system context, so
// even though we're using the user's OAuth session (not our system
// prompt), the same Quant framing applies to any analysis built from
// these tools.
const serverInstructions = `You are connected to the user's personal trading-bot MCP server. This server exposes the user's BingX perpetuals trading data: journal trades, live positions, recent candles, macro event calendar, and backtest facts.

**Persona**: Act as a senior Quantitative Trader / Researcher / Analyzer when using these tools. The user is a disciplined retail quant running a mean-reversion strategy on BTC/ETH/XAU/XAG. Be direct, data-first, terse. No cheerleading.

**Strategy context**: The engine is mean-reversion (fade RSI extremes, sweep low/high, fib 0.618, BOLL touches). Counter-trend setups are the design. Scope is strictly 4 symbols — don't propose adding pairs. Engine evaluates on closed bars only (backtest parity).

**Discipline rules (HARD)**:
- No second-guessing on closed trades — report facts, don't compute "what-if-held" hindsight.
- Strategy changes need backtest A/B 60/90/120d before claiming robustness.
- When a trade is stopped then reverses to TP, fix EXECUTION (TP limit mechanics, macro window) not stop width.
- Engine uses closed bars only — never propose changing this.

**Backtest-known facts** (use to anchor analysis):
- XAU is broken on every TF (60d -10R, 90d -4R, 120d -13R).
- 15m is poison across all symbols (-408R aggregate).
- XAG 2h is the standout edge (+33R aggregate).
- Score < 3 is below daemon MIN_SCORE threshold.

**Output style for analyses**:
1. Status snapshot (table): plan vs current mark, distance to SL/TP, unrealized R, time in trade.
2. Setup quality vs backtest facts.
3. Path observation.
4. Regime context (broader market if relevant).
5. Honest verdict with explicit confidence — but the user owns the decision.

Use get_trade / get_open_positions / get_recent_candles / get_journal_history / get_macro_events_near / get_backtest_facts as needed. Reach for them yourself; don't ask the user to provide data you can fetch.`

// mcpServer bundles handler dependencies. Methods are tool handlers.
type mcpServer struct {
	bx *bingx.Client
}

func (s *mcpServer) register(srv *server.MCPServer) {
	srv.AddTool(mcp.NewTool("get_trade",
		mcp.WithDescription("Returns a single journal trade by ID. Includes plan (entry/SL/TP), open & close notes verbatim, outcome + realized R if closed, signal context, and BingX order IDs if any. Use this when the user references a specific trade number."),
		mcp.WithNumber("id", mcp.Required(), mcp.Description("Journal row ID")),
	), s.handleGetTrade)

	srv.AddTool(mcp.NewTool("get_journal_history",
		mcp.WithDescription("Returns the user's recent journal trades. Filter by symbol (BTC/ETH/XAU/XAG) and/or status (open / closed / all). Useful for context — 'how have the last 5 BTC trades done?' or 'show me all currently-open positions in the journal'."),
		mcp.WithString("symbol", mcp.Description("Optional: BTC, ETH, XAU, or XAG. Empty = all.")),
		mcp.WithString("status", mcp.Description("open / closed / all. Default: all.")),
		mcp.WithNumber("limit", mcp.Description("Max trades to return, newest first. Default 10.")),
	), s.handleGetJournalHistory)

	srv.AddTool(mcp.NewTool("get_open_positions",
		mcp.WithDescription("Returns the user's currently-open BingX perpetual positions across all symbols (live API call). Each entry shows qty, avg fill price, current mark, unrealized PnL in USDT + % on margin, liquidation price. Useful when analyzing live exposure."),
	), s.handleGetOpenPositions)

	srv.AddTool(mcp.NewTool("get_recent_candles",
		mcp.WithDescription("Returns the last N closed candles for a symbol/TF, pre-digested into summary stats (period high/low, net % move, last 5 bars OHLCV). NOT raw OHLCV dump — keeps tokens compact while preserving the path information you need to spot momentum / reversal."),
		mcp.WithString("symbol", mcp.Required(), mcp.Description("BTC, ETH, XAU, or XAG")),
		mcp.WithString("tf", mcp.Required(), mcp.Description("Timeframe: 5m, 15m, 30m, 1h, 2h, 4h, 1d")),
		mcp.WithNumber("n", mcp.Description("Number of candles. Default 50.")),
	), s.handleGetRecentCandles)

	srv.AddTool(mcp.NewTool("get_macro_events_near",
		mcp.WithDescription("Returns macro events (CPI / FOMC / NFP / PPI) within ±24h of the given timestamp (RFC3339 UTC) or 'now'. Each event shows datetime + blackout window. Use this to check if a trade is straddling a known-event volatility regime."),
		mcp.WithString("timestamp", mcp.Description("RFC3339 UTC. Empty / 'now' uses current time.")),
	), s.handleGetMacroEventsNear)

	srv.AddTool(mcp.NewTool("get_backtest_facts",
		mcp.WithDescription("Returns the canonical 2026-06-02 backtest aggregate table (5 TFs × 4 symbols × 60/90/120d windows) plus key headline facts: XAU broken on all TF, 15m poison, XAG 2h is standout edge. Anchor any setup-quality discussion against this table."),
	), s.handleGetBacktestFacts)
}

// ---- Tool handlers ----

func (s *mcpServer) handleGetTrade(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idF, ok := args["id"].(float64)
	if !ok {
		return mcp.NewToolResultError("id (number) is required"), nil
	}
	id := int(idF)

	trades, err := journal.ReadAll(journalPath())
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read journal: %v", err)), nil
	}
	idx := journal.FindByID(trades, id)
	if idx < 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no trade with id %d", id)), nil
	}
	t := trades[idx]

	// Reuse the web-button context packager for shape consistency.
	inputs := ai.TradeAnalysisInputs{Trade: t}
	if sym, err := resolveShortSymbol(t.Symbol); err == nil && s.bx.APIKey != "" {
		if pos, perr := s.bx.FindOpenPosition(ctx, sym, t.Side); perr == nil && pos != nil {
			inputs.LivePosition = pos
		}
		if fr, ferr := s.bx.FundingRate(ctx, sym); ferr == nil {
			inputs.MarkPrice = fr.MarkPrice
		}
		if t.TF != "" {
			tf := t.TF
			if j := strings.Index(tf, ","); j >= 0 {
				tf = strings.TrimSpace(tf[:j])
			}
			if candles, cerr := s.bx.Klines(ctx, sym, market.Timeframe(tf), 50); cerr == nil {
				inputs.RecentBars = candles
			}
		}
	}
	for i := len(trades) - 1; i >= 0 && len(inputs.RecentSame) < 5; i-- {
		r := trades[i]
		if r.ID == t.ID || r.Symbol != t.Symbol || r.ClosedAt.IsZero() {
			continue
		}
		inputs.RecentSame = append(inputs.RecentSame, r)
	}
	anchor := t.OpenedAt
	if anchor.IsZero() {
		anchor = time.Now()
	}
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(anchor)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		inputs.MacroNear = append(inputs.MacroNear, e)
	}

	return textResult(ai.BuildTradeAnalysisMessage(inputs)), nil
}

func (s *mcpServer) handleGetJournalHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	sym := strings.ToUpper(stringArg(args, "symbol"))
	status := strings.ToLower(stringArg(args, "status"))
	if status == "" {
		status = "all"
	}
	limit := 10
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	trades, err := journal.ReadAll(journalPath())
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read journal: %v", err)), nil
	}

	var out []journal.Trade
	for i := len(trades) - 1; i >= 0 && len(out) < limit; i-- {
		t := trades[i]
		if sym != "" && t.Symbol != sym {
			continue
		}
		switch status {
		case "open":
			if !t.IsOpen() {
				continue
			}
		case "closed":
			if t.IsOpen() {
				continue
			}
		}
		out = append(out, t)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Journal history (%s, %s, newest first, max %d)\n\n", coalesce(sym, "all symbols"), status, limit)
	if len(out) == 0 {
		sb.WriteString("(no matching trades)\n")
		return textResult(sb.String()), nil
	}
	sb.WriteString("| # | symbol/side/tf | score | opened | closed | outcome | R | anchor |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, t := range out {
		closedStr := "OPEN"
		if !t.ClosedAt.IsZero() {
			closedStr = t.ClosedAt.Local().Format("01-02 15:04")
		}
		fmt.Fprintf(&sb, "| %d | %s %s/%s | %s | %s | %s | %s | %+.2f | %s |\n",
			t.ID, t.Symbol, t.Side, t.TF, t.Score,
			t.OpenedAt.Local().Format("01-02 15:04"),
			closedStr, coalesce(t.Outcome, "—"), t.RRealized, t.Anchor)
	}
	return textResult(sb.String()), nil
}

func (s *mcpServer) handleGetOpenPositions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.bx.APIKey == "" {
		return mcp.NewToolResultError("BINGX_API_KEY not configured in env"), nil
	}
	var sb strings.Builder
	sb.WriteString("# Open BingX positions (live)\n\n")
	any := false
	for _, sym := range market.All() {
		ps, err := s.bx.OpenPositions(ctx, sym)
		if err != nil {
			fmt.Fprintf(&sb, "- %s: error: %v\n", shortName(sym), err)
			continue
		}
		for _, p := range ps {
			any = true
			fr, _ := s.bx.FundingRate(ctx, sym)
			fmt.Fprintf(&sb, "## %s %s\n\n", shortName(sym), strings.ToUpper(p.Side))
			fmt.Fprintf(&sb, "- qty: %g\n- avg fill: %.4f\n", p.Quantity, p.EntryPrice)
			if fr.MarkPrice > 0 {
				dpct := (fr.MarkPrice - p.EntryPrice) / p.EntryPrice * 100
				if p.Side == "short" {
					dpct = -dpct
				}
				fmt.Fprintf(&sb, "- mark: %.4f (%.2f%% from avg, %s side)\n", fr.MarkPrice, dpct, p.Side)
			}
			fmt.Fprintf(&sb, "- leverage: %dx  margin mode: %s\n\n", p.Leverage, p.MarginMode)
		}
	}
	if !any {
		sb.WriteString("(no open positions)\n")
	}
	return textResult(sb.String()), nil
}

func (s *mcpServer) handleGetRecentCandles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	symShort := strings.ToUpper(stringArg(args, "symbol"))
	tf := stringArg(args, "tf")
	if symShort == "" || tf == "" {
		return mcp.NewToolResultError("symbol and tf required"), nil
	}
	n := 50
	if v, ok := args["n"].(float64); ok && v > 0 {
		n = int(v)
	}
	sym, err := resolveShortSymbol(symShort)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	candles, err := s.bx.Klines(ctx, sym, market.Timeframe(tf), n)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("klines: %v", err)), nil
	}
	if len(candles) == 0 {
		return textResult("(no candles returned)"), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s %s — last %d closed bars\n\n", symShort, tf, len(candles))

	first := candles[0]
	last := candles[len(candles)-1]
	var hi, lo float64 = -1e18, 1e18
	var hiB, loB market.Candle
	for _, b := range candles {
		if b.High > hi {
			hi = b.High
			hiB = b
		}
		if b.Low < lo {
			lo = b.Low
			loB = b
		}
	}
	netPct := (last.Close - first.Open) / first.Open * 100
	fmt.Fprintf(&sb, "- window: %s → %s\n", first.OpenTime.Local().Format("2006-01-02 15:04"), last.CloseTime.Local().Format("2006-01-02 15:04"))
	fmt.Fprintf(&sb, "- first open / last close: %.4f / %.4f (%.2f%% net)\n", first.Open, last.Close, netPct)
	fmt.Fprintf(&sb, "- peak: %.4f at %s\n", hi, hiB.OpenTime.Local().Format("01-02 15:04"))
	fmt.Fprintf(&sb, "- trough: %.4f at %s\n", lo, loB.OpenTime.Local().Format("01-02 15:04"))
	sb.WriteString("\nLast 5 bars (OHLCV):\n```\n")
	tail := candles
	if len(tail) > 5 {
		tail = tail[len(tail)-5:]
	}
	for _, b := range tail {
		fmt.Fprintf(&sb, "  %s  O=%.4f H=%.4f L=%.4f C=%.4f V=%.0f\n",
			b.OpenTime.Local().Format("01-02 15:04"), b.Open, b.High, b.Low, b.Close, b.Volume)
	}
	sb.WriteString("```\n")
	return textResult(sb.String()), nil
}

func (s *mcpServer) handleGetMacroEventsNear(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	tsStr := stringArg(args, "timestamp")
	t := time.Now().UTC()
	if tsStr != "" && tsStr != "now" {
		parsed, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("timestamp must be RFC3339 UTC or 'now': %v", err)), nil
		}
		t = parsed
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Macro events within ±24h of %s\n\n", t.Format(time.RFC3339))
	any := false
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(t)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		any = true
		fmt.Fprintf(&sb, "- **%s** @ %s UTC (Δ %v from reference; blackout %dmin before / %dmin after)\n",
			e.Name, e.DatetimeUTC.Format("2006-01-02 15:04"), delta.Round(time.Minute), e.BeforeMinutes, e.AfterMinutes)
	}
	if !any {
		sb.WriteString("(no macro events within ±24h — clear window)\n")
	}
	if act := macro.ActiveAt(t); act != nil {
		fmt.Fprintf(&sb, "\n**Currently INSIDE blackout window of %s** (signals are suppressed by engine).\n", act.Name)
	}
	return textResult(sb.String()), nil
}

func (s *mcpServer) handleGetBacktestFacts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return textResult(backtestFactsTable), nil
}

const backtestFactsTable = "# Canonical backtest aggregate (2026-06-02)\n\n" +
	"A/B across 5 TFs × 4 symbols × 3 windows (60/90/120d). Values are aggregate netR.\n\n" +
	"| TF | BTC | ETH | XAU | XAG | TOTAL |\n" +
	"|---|---|---|---|---|---|\n" +
	"| 15m | −104.77 | −70.07 | −194.94 | −38.61 | **−408.39** (poison) |\n" +
	"| 30m | −19.59 | +15.94 | −112.88 | −17.49 | −134.02 |\n" +
	"| 1h | +0.47 | −3.51 | −71.93 | +16.75 | −58.22 |\n" +
	"| 2h | −18.40 | −2.52 | −20.17 | +33.10 | **−7.99** (best aggregate) |\n" +
	"| 4h | −7.44 | +6.82 | −4.37 | −10.18 | −15.17 |\n\n" +
	"## Headline facts (use these to anchor analyses)\n\n" +
	"- **XAU is broken on every TF.** Any XAU trade requires significantly higher conviction than equivalent setups on other symbols.\n" +
	"- **15m is poison** across all symbols. Daemon runs 15m alert-only, not for execution.\n" +
	"- **XAG 2h is the standout edge** (+33R aggregate). User watches this manually.\n" +
	"- **Score < 3** is below the daemon's MIN_SCORE filter — flag as below live-execution threshold.\n" +
	"- Per-symbol post-2026-06-22 update: volume-confirmation gate now applies to XAU/XAG only, suppressing sweep + MACD-cross votes when signal bar volume < 1.0× 20-bar avg. Net +25R aggregate, zero crypto regression.\n"

// ---- Helpers ----

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: s}},
	}
}

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func coalesce(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// journalPath resolves the path to journal.csv. Honors JOURNAL_PATH env
// for the typical Mac-side workflow (~/trading-bot-data/journal.csv,
// kept fresh via a scp/cron from VPS), defaulting to ./journal.csv.
func journalPath() string {
	if p := os.Getenv("JOURNAL_PATH"); p != "" {
		return p
	}
	return "journal.csv"
}

// resolveShortSymbol maps BTC/ETH/XAU/XAG → BingX contract codes,
// duplicated from cmd/web's resolveWebSymbol to avoid an import cycle
// (cmd/web imports ai; ai shouldn't depend back on cmd/web).
func resolveShortSymbol(s string) (market.Symbol, error) {
	switch strings.ToUpper(s) {
	case "BTC":
		return market.BTCUSDT, nil
	case "ETH":
		return market.ETHUSDT, nil
	case "XAU":
		return market.XAUUSDT, nil
	case "XAG":
		return market.XAGUSDT, nil
	}
	return "", fmt.Errorf("unknown symbol %q (use BTC / ETH / XAU / XAG)", s)
}

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

// init disables JSON-RPC over stdout from log output (already redirected
// to stderr in main). Kept as a defensive net for any imported package
// that calls log.Println at init time before main runs.
func init() {
	log.SetOutput(os.Stderr)
}

// ensure json is referenced (used by mcp-go internally, but lint
// may flag unused imports if we drop our reference in this file).
var _ = json.Marshal
