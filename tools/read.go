package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/ai"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/macro"
	"github.com/henry190927/trading-bot/market"
)

// Exchange is the slice of *bingx.Client the read tools use. An interface so
// tests run without the network.
type Exchange interface {
	FindOpenPosition(ctx context.Context, sym market.Symbol, side string) (*bingx.Position, error)
	FundingRate(ctx context.Context, sym market.Symbol) (bingx.FundingInfo, error)
	Klines(ctx context.Context, sym market.Symbol, tf market.Timeframe, limit int) ([]market.Candle, error)
	OpenPositions(ctx context.Context, sym market.Symbol) ([]bingx.Position, error)
}

// Deps are what the read tools need from their host. cmd/mcp and cmd/web
// each fill it in with their own symbol handling and journal path.
type Deps struct {
	Exchange Exchange
	// HasExchangeKey is false when no exchange API key is configured: the
	// tools that need private endpoints then say so instead of failing.
	HasExchangeKey bool
	// ReadJournal returns every journal row.
	ReadJournal func() ([]journal.Trade, error)
	// ResolveSymbol maps a short name ("BTC") to an exchange symbol.
	ResolveSymbol func(short string) (market.Symbol, error)
	// ShortName maps an exchange symbol back to its short name.
	ShortName func(market.Symbol) string
	// Symbols are the symbols get_open_positions walks.
	Symbols []market.Symbol
	// SymbolHint describes the accepted symbols in parameter descriptions,
	// e.g. "BTC, ETH, XAU, or XAG".
	SymbolHint string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// ReadTools returns the read-only tools, in the order MCP has always listed
// them.
func ReadTools(d Deps) []Tool {
	hint := d.SymbolHint
	if hint == "" {
		hint = "BTC, ETH, XAU, or XAG"
	}
	return []Tool{
		{
			Name:        "get_trade",
			Description: "Returns a single journal trade by ID. Includes plan (entry/SL/TP), open & close notes verbatim, outcome + realized R if closed, signal context, and BingX order IDs if any. Use this when the user references a specific trade number.",
			Params:      []Param{{Name: "id", Type: Number, Description: "Journal row ID", Required: true}},
			Kind:        Read,
			Run:         d.getTrade,
		},
		{
			Name:        "get_journal_history",
			Description: "Returns the user's recent journal trades. Filter by symbol (" + hint + ") and/or status (open / closed / all). Useful for context — 'how have the last 5 BTC trades done?' or 'show me all currently-open positions in the journal'.",
			Params: []Param{
				{Name: "symbol", Type: String, Description: "Optional: " + hint + ". Empty = all."},
				{Name: "status", Type: String, Description: "open / closed / all. Default: all."},
				{Name: "limit", Type: Number, Description: "Max trades to return, newest first. Default 10."},
			},
			Kind: Read,
			Run:  d.getJournalHistory,
		},
		{
			Name:        "get_open_positions",
			Description: "Returns the user's currently-open BingX perpetual positions across all symbols (live API call). Each entry shows qty, avg fill price, current mark, unrealized PnL in USDT + % on margin, liquidation price. Useful when analyzing live exposure.",
			Kind:        Read,
			Run:         d.getOpenPositions,
		},
		{
			Name:        "get_recent_candles",
			Description: "Returns the last N closed candles for a symbol/TF, pre-digested into summary stats (period high/low, net % move, last 5 bars OHLCV). NOT raw OHLCV dump — keeps tokens compact while preserving the path information you need to spot momentum / reversal.",
			Params: []Param{
				{Name: "symbol", Type: String, Description: hint, Required: true},
				{Name: "tf", Type: String, Description: "Timeframe: 5m, 15m, 30m, 1h, 2h, 4h, 1d", Required: true},
				{Name: "n", Type: Number, Description: "Number of candles. Default 50."},
			},
			Kind: Read,
			Run:  d.getRecentCandles,
		},
		{
			Name:        "get_macro_events_near",
			Description: "Returns macro events (CPI / FOMC / NFP / PPI) within ±24h of the given timestamp (RFC3339 UTC) or 'now'. Each event shows datetime + blackout window. Use this to check if a trade is straddling a known-event volatility regime.",
			Params:      []Param{{Name: "timestamp", Type: String, Description: "RFC3339 UTC. Empty / 'now' uses current time."}},
			Kind:        Read,
			Run:         d.getMacroEventsNear,
		},
		{
			Name:        "get_backtest_facts",
			Description: "Returns the canonical 2026-06-02 backtest aggregate table (5 TFs × 4 symbols × 60/90/120d windows) plus key headline facts: XAU broken on all TF, 15m poison, XAG 2h is standout edge. Anchor any setup-quality discussion against this table.",
			Kind:        Read,
			Run:         func(context.Context, Args) (string, error) { return BacktestFactsTable, nil },
		},
	}
}

func (d Deps) getTrade(ctx context.Context, args Args) (string, error) {
	idF, ok := args.Number("id")
	if !ok {
		return "", errors.New("id (number) is required")
	}
	id := int(idF)

	trades, err := d.ReadJournal()
	if err != nil {
		return "", fmt.Errorf("read journal: %w", err)
	}
	idx := journal.FindByID(trades, id)
	if idx < 0 {
		return "", fmt.Errorf("no trade with id %d", id)
	}
	t := trades[idx]

	// Reuse the web-button context packager for shape consistency.
	inputs := ai.TradeAnalysisInputs{Trade: t}
	if sym, err := d.ResolveSymbol(t.Symbol); err == nil && d.HasExchangeKey {
		if pos, perr := d.Exchange.FindOpenPosition(ctx, sym, t.Side); perr == nil && pos != nil {
			inputs.LivePosition = pos
		}
		if fr, ferr := d.Exchange.FundingRate(ctx, sym); ferr == nil {
			inputs.MarkPrice = fr.MarkPrice
		}
		if t.TF != "" {
			tf := t.TF
			if j := strings.Index(tf, ","); j >= 0 {
				tf = strings.TrimSpace(tf[:j])
			}
			if candles, cerr := d.Exchange.Klines(ctx, sym, market.Timeframe(tf), 50); cerr == nil {
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
		anchor = d.now()
	}
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(anchor)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		inputs.MacroNear = append(inputs.MacroNear, e)
	}

	return ai.BuildTradeAnalysisMessage(inputs), nil
}

func (d Deps) getJournalHistory(_ context.Context, args Args) (string, error) {
	sym := strings.ToUpper(args.String("symbol"))
	status := strings.ToLower(args.String("status"))
	if status == "" {
		status = "all"
	}
	limit := args.Int("limit", 10)

	trades, err := d.ReadJournal()
	if err != nil {
		return "", fmt.Errorf("read journal: %w", err)
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
		return sb.String(), nil
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
	return sb.String(), nil
}

func (d Deps) getOpenPositions(ctx context.Context, _ Args) (string, error) {
	if !d.HasExchangeKey {
		return "", errors.New("BINGX_API_KEY not configured in env")
	}
	var sb strings.Builder
	sb.WriteString("# Open BingX positions (live)\n\n")
	found := false
	for _, sym := range d.Symbols {
		ps, err := d.Exchange.OpenPositions(ctx, sym)
		if err != nil {
			fmt.Fprintf(&sb, "- %s: error: %v\n", d.ShortName(sym), err)
			continue
		}
		for _, p := range ps {
			found = true
			fr, _ := d.Exchange.FundingRate(ctx, sym)
			fmt.Fprintf(&sb, "## %s %s\n\n", d.ShortName(sym), strings.ToUpper(p.Side))
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
	if !found {
		sb.WriteString("(no open positions)\n")
	}
	return sb.String(), nil
}

func (d Deps) getRecentCandles(ctx context.Context, args Args) (string, error) {
	symShort := strings.ToUpper(args.String("symbol"))
	tf := args.String("tf")
	if symShort == "" || tf == "" {
		return "", errors.New("symbol and tf required")
	}
	n := args.Int("n", 50)
	sym, err := d.ResolveSymbol(symShort)
	if err != nil {
		return "", err
	}
	candles, err := d.Exchange.Klines(ctx, sym, market.Timeframe(tf), n)
	if err != nil {
		return "", fmt.Errorf("klines: %w", err)
	}
	if len(candles) == 0 {
		return "(no candles returned)", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s %s — last %d closed bars\n\n", symShort, tf, len(candles))

	first := candles[0]
	last := candles[len(candles)-1]
	hi, lo := -1e18, 1e18
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
	return sb.String(), nil
}

func (d Deps) getMacroEventsNear(_ context.Context, args Args) (string, error) {
	tsStr := args.String("timestamp")
	t := d.now().UTC()
	if tsStr != "" && tsStr != "now" {
		parsed, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			return "", fmt.Errorf("timestamp must be RFC3339 UTC or 'now': %w", err)
		}
		t = parsed
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Macro events within ±24h of %s\n\n", t.Format(time.RFC3339))
	found := false
	for _, e := range macro.All() {
		delta := e.DatetimeUTC.Sub(t)
		if delta < -24*time.Hour || delta > 24*time.Hour {
			continue
		}
		found = true
		fmt.Fprintf(&sb, "- **%s** @ %s UTC (Δ %v from reference; blackout %dmin before / %dmin after)\n",
			e.Name, e.DatetimeUTC.Format("2006-01-02 15:04"), delta.Round(time.Minute), e.BeforeMinutes, e.AfterMinutes)
	}
	if !found {
		sb.WriteString("(no macro events within ±24h — clear window)\n")
	}
	if act := macro.ActiveAt(t); act != nil {
		fmt.Fprintf(&sb, "\n**Currently INSIDE blackout window of %s** (signals are suppressed by engine).\n", act.Name)
	}
	return sb.String(), nil
}

// BacktestFactsTable is the canonical 2026-06-02 backtest aggregate.
const BacktestFactsTable = "# Canonical backtest aggregate (2026-06-02)\n\n" +
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

func coalesce(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
