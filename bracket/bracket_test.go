package bracket

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
)

// openTrade is a minimally-valid open journal trade: BTC long, stop recorded,
// entry seen filled. Tests mutate one field at a time from here so a failure
// names the field that broke rather than the whole fixture.
func openTrade() journal.Trade {
	return journal.Trade{
		ID: 67, Symbol: "BTC", Side: "long",
		Entry: 79006, Stop: 78760, TP1: 79407.2,
		OpenedAt: time.Date(2026, 9, 7, 23, 40, 0, 0, time.UTC),
		FilledAt: time.Date(2026, 9, 7, 23, 41, 0, 0, time.UTC),
	}
}

// livePos is a HEDGE-mode position (positionSide LONG/SHORT).
func livePos(side string) *bingx.Position {
	ps := "LONG"
	if side == "short" {
		ps = "SHORT"
	}
	return &bingx.Position{
		Symbol: market.BTCUSDT, PositionSide: ps, Side: side,
		Quantity: 0.131, EntryPrice: 79006, Leverage: 125, MarginMode: "cross",
	}
}

// oneWayPos is the same position in ONE-WAY mode, where positionSide is BOTH
// and reduceOnly is the only marker of a closing order.
func oneWayPos(side string) *bingx.Position {
	p := livePos(side)
	p.PositionSide = "BOTH"
	return p
}

func stopOrder(id string, side string, trigger float64) bingx.OpenOrder {
	return bingx.OpenOrder{
		Symbol: string(market.BTCUSDT), OrderID: id, Type: "STOP_MARKET",
		Side: side, PositionSide: "LONG", StopPrice: trigger,
		Quantity: 0.131, ReduceOnly: true,
	}
}

func TestDecideSkips(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*journal.Trade)
	}{
		{"closed", func(tr *journal.Trade) { tr.ClosedAt = time.Now() }},
		{"no-fill", func(tr *journal.Trade) { tr.Outcome = "no-fill" }},
		{"no stop recorded", func(tr *journal.Trade) { tr.Stop = 0 }},
		{"negative stop", func(tr *journal.Trade) { tr.Stop = -1 }},
		{"unknown symbol", func(tr *journal.Trade) { tr.Symbol = "DOGE" }},
		{"bad side", func(tr *journal.Trade) { tr.Side = "flat" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := openTrade()
			c.mut(&tr)
			// A live position and no stop at all — the most alarming possible
			// input. Skip must still win, because these trades carry no
			// actionable intent.
			d := Decide(tr, livePos("long"), nil, ModePlace)
			if d.State != StateSkip {
				t.Fatalf("state = %q, want skip (reason %q)", d.State, d.Reason)
			}
			if d.PlaceStop != 0 {
				t.Errorf("PlaceStop = %g, want 0 — a skipped trade must never propose an order", d.PlaceStop)
			}
		})
	}
}

func TestDecideWaitingVsStale(t *testing.T) {
	// Pending: never filled, no position. Normal, not an alarm.
	tr := openTrade()
	tr.FilledAt = time.Time{}
	if d := Decide(tr, nil, nil, ModePlace); d.State != StateWaiting {
		t.Errorf("unfilled + no position: state = %q, want waiting", d.State)
	}

	// Was filled, position now gone: the CSV is behind. Must not place.
	tr = openTrade()
	d := Decide(tr, nil, nil, ModePlace)
	if d.State != StateStale {
		t.Errorf("filled + no position: state = %q, want stale", d.State)
	}
	if d.PlaceStop != 0 {
		t.Errorf("PlaceStop = %g on a stale trade, want 0 — there is no position to reduce", d.PlaceStop)
	}
}

func TestDecideProtected(t *testing.T) {
	tr := openTrade()
	orders := []bingx.OpenOrder{stopOrder("2094445961317412864", "SELL", 78760)}
	d := Decide(tr, livePos("long"), orders, ModePlace)
	if d.State != StateProtected {
		t.Fatalf("state = %q, want protected (reason %q)", d.State, d.Reason)
	}
	if d.LiveStopID != "2094445961317412864" {
		t.Errorf("LiveStopID = %q, want the resting order's id", d.LiveStopID)
	}
	if d.LiveStopPrice != 78760 {
		t.Errorf("LiveStopPrice = %g, want 78760", d.LiveStopPrice)
	}
	if d.PlaceStop != 0 {
		t.Errorf("PlaceStop = %g on a protected position, want 0", d.PlaceStop)
	}
	if d.GhostStopID != "" {
		t.Errorf("GhostStopID = %q, want empty — journal named no order", d.GhostStopID)
	}
}

// A stop at a price nowhere near the journal's stop still protects. This is
// the trailing / profit-lock case that a price-matching check would reject.
func TestDecideProtectedByATrailingStop(t *testing.T) {
	tr := openTrade() // journal stop 78760
	orders := []bingx.OpenOrder{stopOrder("999", "SELL", 79900)}
	d := Decide(tr, livePos("long"), orders, ModePlace)
	if d.State != StateProtected {
		t.Fatalf("state = %q, want protected — a profit-lock is still a stop", d.State)
	}
	if d.LiveStopPrice != 79900 {
		t.Errorf("LiveStopPrice = %g, want 79900", d.LiveStopPrice)
	}
}

func TestDecideNakedModes(t *testing.T) {
	cases := []struct {
		name      string
		stopAuto  bool
		mode      Mode
		wantPlace float64
	}{
		{"alert mode never places", false, ModeAlert, 0},
		{"place mode places the journal stop", false, ModePlace, 78760},
		{"StopAuto places even in alert mode", true, ModeAlert, 78760},
		{"StopAuto and place mode agree", true, ModePlace, 78760},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := openTrade()
			tr.StopAuto = c.stopAuto
			d := Decide(tr, livePos("long"), nil, c.mode)
			if d.State != StateNaked {
				t.Fatalf("state = %q, want naked", d.State)
			}
			if d.PlaceStop != c.wantPlace {
				t.Errorf("PlaceStop = %g, want %g", d.PlaceStop, c.wantPlace)
			}
			if d.PlannedStop != 78760 {
				t.Errorf("PlannedStop = %g, want 78760 — the alert needs it even when nothing is placed", d.PlannedStop)
			}
		})
	}
}

// The case cmd/web structurally cannot reach: it skips placement once
// StopOrderID is set, so a cancelled stop leaves the journal reading
// protected forever. A live trade has failed exactly this way.
func TestDecideGhostStop(t *testing.T) {
	tr := openTrade()
	tr.StopOrderID = "2094351178603393024"
	d := Decide(tr, livePos("long"), nil, ModePlace)
	if d.State != StateNaked {
		t.Fatalf("state = %q, want naked — the named order is not resting", d.State)
	}
	if d.GhostStopID != "2094351178603393024" {
		t.Errorf("GhostStopID = %q, want the journal's stale id", d.GhostStopID)
	}
	if d.PlaceStop != 78760 {
		t.Errorf("PlaceStop = %g, want 78760 — a ghost must be re-placed", d.PlaceStop)
	}
}

// Benign twin: protected, but by a different order than the CSV names. Worth
// reporting, not worth acting on.
func TestDecideProtectedByADifferentOrder(t *testing.T) {
	tr := openTrade()
	tr.StopOrderID = "old-id"
	orders := []bingx.OpenOrder{stopOrder("new-id", "SELL", 78900)}
	d := Decide(tr, livePos("long"), orders, ModePlace)
	if d.State != StateProtected {
		t.Fatalf("state = %q, want protected", d.State)
	}
	if d.GhostStopID != "old-id" {
		t.Errorf("GhostStopID = %q, want \"old-id\"", d.GhostStopID)
	}
	if d.PlaceStop != 0 {
		t.Errorf("PlaceStop = %g, want 0 — the position is covered", d.PlaceStop)
	}
}

// Everything ambiguous must resolve toward naked. A duplicate reduce-only
// stop is harmless; a missed naked position is not.
func TestFindProtectiveStopRejections(t *testing.T) {
	cases := []struct {
		name  string
		order bingx.OpenOrder
		side  string
	}{
		{
			name:  "reduce-only LIMIT is a take-profit, not a stop",
			order: bingx.OpenOrder{OrderID: "1", Type: "LIMIT", Side: "SELL", Price: 79407, ReduceOnly: true},
			side:  "long",
		},
		{
			name:  "STOP that is not reduce-only could OPEN a position",
			order: bingx.OpenOrder{OrderID: "2", Type: "STOP_MARKET", Side: "SELL", StopPrice: 78760, ReduceOnly: false},
			side:  "long",
		},
		{
			name:  "wrong closing side for a long",
			order: bingx.OpenOrder{OrderID: "3", Type: "STOP_MARKET", Side: "BUY", StopPrice: 78760, ReduceOnly: true},
			side:  "long",
		},
		{
			name:  "wrong closing side for a short",
			order: bingx.OpenOrder{OrderID: "4", Type: "STOP_MARKET", Side: "SELL", StopPrice: 2495, ReduceOnly: true},
			side:  "short",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := FindProtectiveStop([]bingx.OpenOrder{c.order}, livePos(c.side)); ok {
				t.Errorf("order was accepted as protection; want rejected")
			}
		})
	}
}

func TestFindProtectiveStopAccepts(t *testing.T) {
	cases := []struct {
		name  string
		order bingx.OpenOrder
		side  string
	}{
		{
			name:  "short protected by a reduce-only BUY stop",
			order: bingx.OpenOrder{OrderID: "1", Type: "STOP_MARKET", Side: "BUY", StopPrice: 2495, ReduceOnly: true},
			side:  "short",
		},
		{
			// Degrade over-inclusive rather than declaring every position
			// naked and double-stopping all of them.
			name:  "missing side field is accepted",
			order: bingx.OpenOrder{OrderID: "2", Type: "STOP_MARKET", Side: "", StopPrice: 78760, ReduceOnly: true},
			side:  "long",
		},
		{
			name:  "plain STOP type, not just STOP_MARKET",
			order: bingx.OpenOrder{OrderID: "3", Type: "STOP", Side: "SELL", StopPrice: 78760, ReduceOnly: true},
			side:  "long",
		},
		{
			name:  "lowercase type and side from a future response change",
			order: bingx.OpenOrder{OrderID: "4", Type: "stop_market", Side: "sell", StopPrice: 78760, ReduceOnly: true},
			side:  "long",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := FindProtectiveStop([]bingx.OpenOrder{c.order}, livePos(c.side)); !ok {
				t.Errorf("order was rejected; want accepted as protection")
			}
		})
	}
}

// A TP resting alongside the stop must not shadow it — the scan has to keep
// looking past the first reduce-only order it sees.
func TestFindProtectiveStopPastATakeProfit(t *testing.T) {
	orders := []bingx.OpenOrder{
		{OrderID: "tp", Type: "LIMIT", Side: "SELL", Price: 79407, ReduceOnly: true},
		stopOrder("sl", "SELL", 78760),
	}
	got, ok := FindProtectiveStop(orders, livePos("long"))
	if !ok {
		t.Fatal("stop not found behind a resting take-profit")
	}
	if got.OrderID != "sl" {
		t.Errorf("found order %q, want \"sl\"", got.OrderID)
	}
}

func TestStopTriggerPrefersStopPrice(t *testing.T) {
	if got := StopTrigger(bingx.OpenOrder{StopPrice: 78760, Price: 5}); got != 78760 {
		t.Errorf("stopTrigger = %g, want 78760 (StopPrice wins)", got)
	}
	if got := StopTrigger(bingx.OpenOrder{StopPrice: 0, Price: 78760}); got != 78760 {
		t.Errorf("stopTrigger = %g, want 78760 (Price fallback)", got)
	}
}

func TestNakedHelper(t *testing.T) {
	for _, s := range []State{StateSkip, StateWaiting, StateProtected, StateStale} {
		if (Decision{State: s}).Naked() {
			t.Errorf("state %q reported Naked() = true", s)
		}
	}
	if !(Decision{State: StateNaked}).Naked() {
		t.Error("StateNaked reported Naked() = false")
	}
}

// Every short name the journal accepts must resolve, or a trade on that
// symbol silently skips protection. This is the check that was missing when
// zone.ShortToSym went stale against the five crypto alts.
func TestEverySymbolResolves(t *testing.T) {
	for _, short := range market.Shorts() {
		tr := openTrade()
		tr.Symbol = short
		d := Decide(tr, nil, nil, ModeAlert)
		if d.State == StateSkip {
			t.Errorf("%s: skipped with reason %q — a journal trade on this symbol would go unprotected", short, d.Reason)
		}
		if d.Symbol == "" {
			t.Errorf("%s: resolved to an empty contract symbol", short)
		}
	}
}

// Hedge mode is the trap. BingX rejects reduceOnly there and encodes the
// close-side in positionSide instead, so a real protective stop reports
// ReduceOnly=false. Requiring the flag would declare every hedge-mode
// position naked and place a second stop on all of them.
func TestFindProtectiveStopHedgeMode(t *testing.T) {
	hedgeStop := bingx.OpenOrder{
		OrderID: "h1", Type: "STOP_MARKET", Side: "SELL",
		PositionSide: "LONG", StopPrice: 78760, ReduceOnly: false,
	}
	if _, ok := FindProtectiveStop([]bingx.OpenOrder{hedgeStop}, livePos("long")); !ok {
		t.Error("hedge-mode protective stop rejected — would double-place on every hedge position")
	}

	// Same order shape, but positionSide names the OTHER book: in hedge mode
	// that stop would OPEN a short, not close the long.
	opensShort := hedgeStop
	opensShort.PositionSide = "SHORT"
	if _, ok := FindProtectiveStop([]bingx.OpenOrder{opensShort}, livePos("long")); ok {
		t.Error("a stop that would open the opposite book was accepted as protection")
	}

	// One-way mode: positionSide is BOTH on both the position and the order,
	// so it carries no information. Without reduceOnly this is ambiguous —
	// it could close, or it could stop-and-reverse — and must read naked.
	ambiguous := bingx.OpenOrder{
		OrderID: "o1", Type: "STOP_MARKET", Side: "SELL",
		PositionSide: "BOTH", StopPrice: 78760, ReduceOnly: false,
	}
	if _, ok := FindProtectiveStop([]bingx.OpenOrder{ambiguous}, oneWayPos("long")); ok {
		t.Error("one-way non-reduce-only stop accepted; ambiguity must resolve toward naked")
	}
	ambiguous.ReduceOnly = true
	if _, ok := FindProtectiveStop([]bingx.OpenOrder{ambiguous}, oneWayPos("long")); !ok {
		t.Error("one-way reduce-only stop rejected")
	}
}

// A short in hedge mode closes with a BUY on positionSide SHORT.
func TestFindProtectiveStopHedgeShort(t *testing.T) {
	o := bingx.OpenOrder{
		OrderID: "h2", Type: "STOP_MARKET", Side: "BUY",
		PositionSide: "SHORT", StopPrice: 2495, ReduceOnly: false,
	}
	if _, ok := FindProtectiveStop([]bingx.OpenOrder{o}, livePos("short")); !ok {
		t.Error("hedge-mode short protective stop rejected")
	}
}
