package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
)

type fakeExchange struct {
	positions map[market.Symbol][]bingx.Position
	posErr    error
	candles   []market.Candle
	mark      float64
}

func (f *fakeExchange) FindOpenPosition(_ context.Context, sym market.Symbol, side string) (*bingx.Position, error) {
	for _, p := range f.positions[sym] {
		if p.Side == side {
			return &p, nil
		}
	}
	return nil, nil
}

func (f *fakeExchange) FundingRate(context.Context, market.Symbol) (bingx.FundingInfo, error) {
	return bingx.FundingInfo{MarkPrice: f.mark}, nil
}

func (f *fakeExchange) Klines(_ context.Context, _ market.Symbol, _ market.Timeframe, n int) ([]market.Candle, error) {
	if n < len(f.candles) {
		return f.candles[len(f.candles)-n:], nil
	}
	return f.candles, nil
}

func (f *fakeExchange) OpenPositions(_ context.Context, sym market.Symbol) ([]bingx.Position, error) {
	if f.posErr != nil {
		return nil, f.posErr
	}
	return f.positions[sym], nil
}

func resolve(s string) (market.Symbol, error) {
	switch strings.ToUpper(s) {
	case "BTC":
		return market.BTCUSDT, nil
	case "ETH":
		return market.ETHUSDT, nil
	}
	return "", errors.New("unknown symbol " + s)
}

func short(s market.Symbol) string {
	if s == market.BTCUSDT {
		return "BTC"
	}
	return "ETH"
}

func testDeps(trades []journal.Trade, ex *fakeExchange, key bool) Deps {
	return Deps{
		Exchange:       ex,
		HasExchangeKey: key,
		ReadJournal:    func() ([]journal.Trade, error) { return trades, nil },
		ResolveSymbol:  resolve,
		ShortName:      short,
		Symbols:        []market.Symbol{market.BTCUSDT, market.ETHUSDT},
		Now:            func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) },
	}
}

func run(t *testing.T, d Deps, name string, args Args) (string, error) {
	t.Helper()
	reg, err := NewRegistry(ReadTools(d)...)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	return tool.Run(context.Background(), args)
}

// cmd/mcp has always exposed exactly these, in this order. Renaming one
// breaks every saved Claude Desktop conversation that refers to it.
func TestReadToolsKeepTheMCPNames(t *testing.T) {
	var got []string
	for _, tl := range ReadTools(testDeps(nil, &fakeExchange{}, false)) {
		got = append(got, tl.Name)
		if tl.Kind != Read {
			t.Errorf("%s: kind %v, want read", tl.Name, tl.Kind)
		}
	}
	want := []string{"get_trade", "get_journal_history", "get_open_positions", "get_recent_candles", "get_macro_events_near", "get_backtest_facts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestRegistryRejectsDuplicatesAndBlanks(t *testing.T) {
	noop := func(context.Context, Args) (string, error) { return "", nil }
	if _, err := NewRegistry(Tool{Name: "a", Run: noop}, Tool{Name: "a", Run: noop}); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := NewRegistry(Tool{Name: " ", Run: noop}); err == nil {
		t.Error("blank name accepted")
	}
	if _, err := NewRegistry(Tool{Name: "b"}); err == nil {
		t.Error("tool without Run accepted")
	}
}

func TestSchemaShape(t *testing.T) {
	var candles Tool
	for _, tl := range ReadTools(testDeps(nil, &fakeExchange{}, false)) {
		if tl.Name == "get_recent_candles" {
			candles = tl
		}
	}
	s := candles.Schema()
	if s["type"] != "object" {
		t.Fatalf("type = %v", s["type"])
	}
	if req := s["required"].([]string); !reflect.DeepEqual(req, []string{"symbol", "tf"}) {
		t.Errorf("required = %v", req)
	}
	props := s["properties"].(map[string]any)
	if props["n"].(map[string]any)["type"] != "number" {
		t.Errorf("n type = %v", props["n"])
	}
	// A tool with no params still yields a valid empty object schema and no
	// "required" key (Gemini rejects an empty required list).
	var noParams Tool
	for _, tl := range ReadTools(testDeps(nil, &fakeExchange{}, false)) {
		if tl.Name == "get_backtest_facts" {
			noParams = tl
		}
	}
	if _, has := noParams.Schema()["required"]; has {
		t.Error("required present on a tool with no params")
	}
}

func TestGetTrade(t *testing.T) {
	trades := []journal.Trade{
		{ID: 1, Symbol: "BTC", Side: "long", Entry: 100, Stop: 90, OpenNotes: "first"},
		{ID: 2, Symbol: "ETH", Side: "short", Entry: 2000, Stop: 2100, OpenNotes: "the one"},
	}
	out, err := run(t, testDeps(trades, &fakeExchange{}, false), "get_trade", Args{"id": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the one") {
		t.Errorf("trade #2 text missing its notes:\n%s", out)
	}
	if _, err := run(t, testDeps(trades, &fakeExchange{}, false), "get_trade", Args{"id": 99.0}); err == nil || !strings.Contains(err.Error(), "no trade with id 99") {
		t.Errorf("missing id: err = %v", err)
	}
	if _, err := run(t, testDeps(trades, &fakeExchange{}, false), "get_trade", Args{}); err == nil {
		t.Error("no id accepted")
	}
}

func TestGetJournalHistoryFilters(t *testing.T) {
	closed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	trades := []journal.Trade{
		{ID: 1, Symbol: "BTC", Side: "long", ClosedAt: closed, Outcome: "win"},
		{ID: 2, Symbol: "ETH", Side: "long"},
		{ID: 3, Symbol: "BTC", Side: "short"},
		{ID: 4, Symbol: "BTC", Side: "long", ClosedAt: closed, Outcome: "loss"},
	}
	d := testDeps(trades, &fakeExchange{}, false)
	rows := func(out string) []string {
		var r []string
		for l := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| #") {
				r = append(r, strings.Fields(l)[1])
			}
		}
		return r
	}
	out, _ := run(t, d, "get_journal_history", Args{"symbol": "btc"})
	if got := rows(out); !reflect.DeepEqual(got, []string{"4", "3", "1"}) {
		t.Errorf("BTC rows = %v, want newest first [4 3 1]", got)
	}
	out, _ = run(t, d, "get_journal_history", Args{"status": "open"})
	if got := rows(out); !reflect.DeepEqual(got, []string{"3", "2"}) {
		t.Errorf("open rows = %v", got)
	}
	out, _ = run(t, d, "get_journal_history", Args{"limit": 1.0})
	if got := rows(out); !reflect.DeepEqual(got, []string{"4"}) {
		t.Errorf("limit 1 rows = %v", got)
	}
	out, _ = run(t, d, "get_journal_history", Args{"symbol": "XAG"})
	if !strings.Contains(out, "(no matching trades)") {
		t.Errorf("no-match text missing:\n%s", out)
	}
}

func TestGetOpenPositions(t *testing.T) {
	if _, err := run(t, testDeps(nil, &fakeExchange{}, false), "get_open_positions", nil); err == nil || !strings.Contains(err.Error(), "BINGX_API_KEY") {
		t.Errorf("no key: err = %v", err)
	}
	ex := &fakeExchange{mark: 110, positions: map[market.Symbol][]bingx.Position{
		market.BTCUSDT: {{Side: "long", Quantity: 0.5, EntryPrice: 100, Leverage: 10, MarginMode: "cross"}},
	}}
	out, err := run(t, testDeps(nil, ex, true), "get_open_positions", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## BTC LONG", "qty: 0.5", "10.00% from avg"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out, _ = run(t, testDeps(nil, &fakeExchange{}, true), "get_open_positions", nil)
	if !strings.Contains(out, "(no open positions)") {
		t.Errorf("empty book text missing:\n%s", out)
	}
	// A per-symbol read error is reported in the text, not as a tool error.
	out, err = run(t, testDeps(nil, &fakeExchange{posErr: errors.New("boom")}, true), "get_open_positions", nil)
	if err != nil || !strings.Contains(out, "BTC: error: boom") {
		t.Errorf("per-symbol error: out=%q err=%v", out, err)
	}
}

func TestGetRecentCandles(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i, c := range []float64{100, 104, 98, 110} {
		cs = append(cs, market.Candle{OpenTime: t0.Add(time.Duration(i) * time.Hour), CloseTime: t0.Add(time.Duration(i+1) * time.Hour),
			Open: c - 1, High: c + 2, Low: c - 3, Close: c, Volume: 10})
	}
	d := testDeps(nil, &fakeExchange{candles: cs}, true)
	out, err := run(t, d, "get_recent_candles", Args{"symbol": "btc", "tf": "1h"})
	if err != nil {
		t.Fatal(err)
	}
	// first open 99, last close 110 → +11.11%; peak 112, trough 95.
	for _, want := range []string{"BTC 1h — last 4 closed bars", "(11.11% net)", "peak: 112.0000", "trough: 95.0000"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := run(t, d, "get_recent_candles", Args{"symbol": "btc"}); err == nil {
		t.Error("missing tf accepted")
	}
	if _, err := run(t, d, "get_recent_candles", Args{"symbol": "doge", "tf": "1h"}); err == nil {
		t.Error("unknown symbol accepted")
	}
}

func TestGetMacroEventsNearRejectsBadTimestamp(t *testing.T) {
	if _, err := run(t, testDeps(nil, &fakeExchange{}, false), "get_macro_events_near", Args{"timestamp": "yesterday"}); err == nil {
		t.Error("bad timestamp accepted")
	}
	out, err := run(t, testDeps(nil, &fakeExchange{}, false), "get_macro_events_near", Args{"timestamp": "now"})
	if err != nil || !strings.Contains(out, "2026-11-21T12:00:00Z") {
		t.Errorf("now uses Deps.Now: out=%q err=%v", out, err)
	}
}

func TestArgs(t *testing.T) {
	a := Args{"s": "x", "f": 2.0, "i": 3, "neg": -1.0}
	if a.String("s") != "x" || a.String("f") != "" {
		t.Error("String")
	}
	if v, ok := a.Number("i"); !ok || v != 3 {
		t.Error("Number from int")
	}
	if a.Int("f", 9) != 2 || a.Int("missing", 9) != 9 || a.Int("neg", 9) != 9 {
		t.Error("Int defaults")
	}
}
