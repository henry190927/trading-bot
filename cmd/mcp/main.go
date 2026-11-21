// Command mcp is the trading-bot MCP (Model Context Protocol) server.
// It runs as a stdio subprocess of Claude Code and exposes the
// trading-bot's data — journal trades, BingX positions, recent candles,
// macro events, backtest facts — as tools the user's Claude Code
// session can call.
//
// This is the zero-cost alternative to the trading-web /ai/analyze
// endpoint: instead of calling Anthropic's API with a key, the user
// asks Claude Code (already authenticated to their account via OAuth)
// to "analyze trade 25", and Claude Code calls the tools below to
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
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/tools"
)

func main() {
	// Logs go to stderr — stdout is the JSON-RPC transport in stdio mode,
	// and writing to it would corrupt the protocol stream.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds | log.Lshortfile)

	bxClient := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	reg, err := tools.NewRegistry(tools.ReadTools(mcpDeps(bxClient))...)
	if err != nil {
		log.Fatalf("tools: %v", err)
	}

	s := server.NewMCPServer(
		"trading-bot",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithInstructions(serverInstructions),
	)

	register(s, reg)

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

// mcpDeps wires the shared tools (package tools) to this process: the
// journal at JOURNAL_PATH and the four MCP symbols.
func mcpDeps(bx *bingx.Client) tools.Deps {
	return tools.Deps{
		Exchange:       bx,
		HasExchangeKey: bx.APIKey != "",
		ReadJournal:    func() ([]journal.Trade, error) { return journal.ReadAll(journalPath()) },
		ResolveSymbol:  resolveShortSymbol,
		ShortName:      shortName,
		Symbols:        market.All(),
		SymbolHint:     "BTC, ETH, XAU, or XAG",
	}
}

// register exposes every tool in reg over MCP. The definitions live in
// package tools, shared with the web copilot, so the two cannot drift.
func register(srv *server.MCPServer, reg *tools.Registry) {
	for _, t := range reg.All() {
		srv.AddTool(mcpTool(t), mcpHandler(t))
	}
}

// mcpTool converts a tools.Tool into an mcp-go tool definition.
func mcpTool(t tools.Tool) mcp.Tool {
	opts := []mcp.ToolOption{mcp.WithDescription(t.Description)}
	for _, p := range t.Params {
		var po []mcp.PropertyOption
		if p.Required {
			po = append(po, mcp.Required())
		}
		if p.Description != "" {
			po = append(po, mcp.Description(p.Description))
		}
		switch p.Type {
		case tools.Number, tools.Integer:
			opts = append(opts, mcp.WithNumber(p.Name, po...))
		case tools.Boolean:
			opts = append(opts, mcp.WithBoolean(p.Name, po...))
		default:
			if len(p.Enum) > 0 {
				po = append(po, mcp.Enum(p.Enum...))
			}
			opts = append(opts, mcp.WithString(p.Name, po...))
		}
	}
	return mcp.NewTool(t.Name, opts...)
}

// mcpHandler runs t and wraps its text, or its error, as an MCP result. A
// tool error is a result the model reads, not a protocol error.
func mcpHandler(t tools.Tool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := t.Run(ctx, tools.Args(req.GetArguments()))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return textResult(out), nil
	}
}

// ---- Helpers ----

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: s}},
	}
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
