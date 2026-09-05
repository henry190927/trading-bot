package protect

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
)

func longPos() *bingx.Position {
	return &bingx.Position{
		Symbol: market.BTCUSDT, PositionSide: "BOTH", Side: "long",
		Quantity: 0.1104, EntryPrice: 79000, Leverage: 125, MarginMode: "cross",
	}
}

func shortPos() *bingx.Position {
	return &bingx.Position{
		Symbol: market.ETHUSDT, PositionSide: "SHORT", Side: "short",
		Quantity: 3.55, EntryPrice: 2463, Leverage: 125, MarginMode: "cross",
	}
}

// ---------------------------------------------------------------------
// The invariant this package exists to protect: MARK, not entry.
//
// An entry-based check forbade every trailing stop, which is why placing one
// meant SSH. A stop beyond entry in the favourable direction is a profit-lock
// and must be ALLOWED; a stop on the wrong side of the MARK fires on arrival
// and must be REFUSED. Those are different mistakes and only one is a mistake.
// ---------------------------------------------------------------------

func TestStopBeyondEntryIsAllowedAsProfitLock(t *testing.T) {
	// Long from 79,000, mark 80,000, stop trailed up to 79,500 — under entry
	// no longer, but still 500 below the mark, so it rests.
	p := BuildPlan(longPos(), 80000, 79500, 0)
	if !p.OK() {
		t.Fatalf("a trailing profit-lock stop must be allowed, got faults: %v", p.Faults)
	}
	if math.Abs(p.StopLocksUSDT-55.2) > 1e-6 {
		t.Errorf("StopLocksUSDT = %.6f, want 55.2 ((79500-79000)*0.1104)", p.StopLocksUSDT)
	}

	// Mirror on a short: from 2,463, mark 2,350, stop pulled down to 2,400.
	q := BuildPlan(shortPos(), 2350, 2400, 0)
	if !q.OK() {
		t.Fatalf("short-side profit-lock must be allowed, got: %v", q.Faults)
	}
	if math.Abs(q.StopLocksUSDT-223.65) > 1e-6 {
		t.Errorf("StopLocksUSDT = %.6f, want 223.65", q.StopLocksUSDT)
	}
}

func TestStopOnTheWrongSideOfMarkIsRefused(t *testing.T) {
	// Long, mark 79,400, stop 79,500 — above the mark, fires on arrival.
	p := BuildPlan(longPos(), 79400, 79500, 0)
	if p.OK() {
		t.Fatal("a stop above the mark on a long must be refused")
	}
	if !strings.Contains(p.Faults[0], "trigger immediately") {
		t.Errorf("fault should name the failure mode, got %q", p.Faults[0])
	}

	// Short, mark 2,470, stop 2,400 — below the mark, fires on arrival.
	q := BuildPlan(shortPos(), 2470, 2400, 0)
	if q.OK() {
		t.Fatal("a stop below the mark on a short must be refused")
	}
}

// Exactly at the mark counts as wrong-side: a trigger sitting on the mark is
// a coin-flip on the next tick, not a resting order.
func TestStopExactlyAtMarkIsRefused(t *testing.T) {
	if BuildPlan(longPos(), 79500, 79500, 0).OK() {
		t.Error("long stop == mark must be refused")
	}
	if BuildPlan(shortPos(), 2400, 2400, 0).OK() {
		t.Error("short stop == mark must be refused")
	}
}

func TestTPMustRestNotFillOnArrival(t *testing.T) {
	// Long, mark 79,400, tp 80,000 → rests. Gain (80000-79000)*0.1104.
	p := BuildPlan(longPos(), 79400, 0, 80000)
	if !p.OK() {
		t.Fatalf("tp above the mark on a long must be allowed: %v", p.Faults)
	}
	if math.Abs(p.TPGainUSDT-110.4) > 1e-6 {
		t.Errorf("TPGainUSDT = %.6f, want 110.4", p.TPGainUSDT)
	}
	// Same tp with the mark already through it → refused.
	if BuildPlan(longPos(), 80100, 0, 80000).OK() {
		t.Error("tp below the mark on a long must be refused")
	}

	// Short, mark 2,470, tp 2,400 → rests. Gain -(2400-2463)*3.55.
	q := BuildPlan(shortPos(), 2470, 0, 2400)
	if !q.OK() {
		t.Fatalf("tp below the mark on a short must be allowed: %v", q.Faults)
	}
	if math.Abs(q.TPGainUSDT-223.65) > 1e-6 {
		t.Errorf("TPGainUSDT = %.6f, want 223.65", q.TPGainUSDT)
	}
	if BuildPlan(shortPos(), 2390, 0, 2400).OK() {
		t.Error("tp above the mark on a short must be refused")
	}
}

// An ordinary loss-capping stop is not a profit-lock and must not be
// annotated as one — a "+USDT locked" line on a losing stop would be a lie.
func TestOrdinaryStopReportsNoLock(t *testing.T) {
	p := BuildPlan(longPos(), 79400, 78500, 0)
	if !p.OK() {
		t.Fatalf("unexpected faults: %v", p.Faults)
	}
	if p.StopLocksUSDT != 0 {
		t.Errorf("StopLocksUSDT = %v, want 0 for a stop below entry on a long", p.StopLocksUSDT)
	}
}

func TestBuildPlanRefusesMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pos           *bingx.Position
		mark          float64
		stop, tp      float64
		wantSubstring string
	}{
		{"no position", nil, 79400, 78500, 0, "no open position"},
		{"zero qty", &bingx.Position{Symbol: market.BTCUSDT, Side: "long"}, 79400, 78500, 0, "quantity is 0"},
		{"no mark", longPos(), 0, 78500, 0, "mark price unavailable"},
		{"neither stop nor tp", longPos(), 79400, 0, 0, "give a stop, a tp, or both"},
		{"weird side", &bingx.Position{Symbol: market.BTCUSDT, Side: "sideways", Quantity: 1, EntryPrice: 1}, 79400, 78500, 0, "unexpected position side"},
	} {
		p := BuildPlan(tc.pos, tc.mark, tc.stop, tc.tp)
		if p.OK() {
			t.Errorf("%s: expected a fault", tc.name)
			continue
		}
		if !strings.Contains(strings.Join(p.Faults, " "), tc.wantSubstring) {
			t.Errorf("%s: faults %v, want one containing %q", tc.name, p.Faults, tc.wantSubstring)
		}
	}
}

// Hedge mode comes from what the exchange reports, never from config: the two
// modes need different order payloads and guessing wrong gets the order
// rejected (or worse, accepted against the wrong leg).
func TestHedgeModeInferredFromPositionSide(t *testing.T) {
	for _, tc := range []struct {
		posSide string
		want    bool
	}{{"LONG", true}, {"SHORT", true}, {"BOTH", false}, {"", false}} {
		pos := longPos()
		pos.PositionSide = tc.posSide
		if got := BuildPlan(pos, 79400, 78500, 0).Hedge; got != tc.want {
			t.Errorf("positionSide %q → hedge %v, want %v", tc.posSide, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------

type fakePlacer struct {
	stopCalls, tpCalls int
	gotQty             []float64
	stopErr, tpErr     error
	order              int
}

func (f *fakePlacer) PlaceStopMarket(_ context.Context, _ market.Symbol, _ string, qty, _ float64, _ bool) (*bingx.OrderResult, error) {
	f.stopCalls++
	f.order++
	f.gotQty = append(f.gotQty, qty)
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &bingx.OrderResult{OrderID: "STOP-1"}, nil
}

func (f *fakePlacer) PlaceReduceOnlyLimit(_ context.Context, _ market.Symbol, _ string, qty, _ float64, _ bool) (*bingx.OrderResult, error) {
	f.tpCalls++
	f.order += 10
	f.gotQty = append(f.gotQty, qty)
	if f.tpErr != nil {
		return nil, f.tpErr
	}
	return &bingx.OrderResult{OrderID: "TP-1"}, nil
}

// Size must come from the position, so both orders carry the position's qty
// and nothing else can influence it.
func TestApplySizesFromThePosition(t *testing.T) {
	f := &fakePlacer{}
	p := BuildPlan(shortPos(), 2470, 2495, 2400)
	if !p.OK() {
		t.Fatalf("plan faults: %v", p.Faults)
	}
	r := Apply(context.Background(), f, p)
	if len(r.Errors) != 0 {
		t.Fatalf("errors: %v", r.Errors)
	}
	if r.StopOrderID != "STOP-1" || r.TPOrderID != "TP-1" {
		t.Errorf("ids = %q / %q", r.StopOrderID, r.TPOrderID)
	}
	for _, q := range f.gotQty {
		if q != 3.55 {
			t.Errorf("order sized %v, want the position's 3.55", q)
		}
	}
}

// Stop first. If the second call fails the position must end up
// protected-without-a-target, never targeted-and-naked.
func TestApplySendsStopBeforeTP(t *testing.T) {
	f := &fakePlacer{tpErr: errors.New("exchange said no")}
	p := BuildPlan(shortPos(), 2470, 2495, 2400)
	r := Apply(context.Background(), f, p)
	if r.StopOrderID == "" {
		t.Error("the stop must have landed even though the tp failed")
	}
	if r.TPOrderID != "" {
		t.Error("tp should not report an id after an error")
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "place tp") {
		t.Errorf("errors = %v", r.Errors)
	}
	if !r.Sent() {
		t.Error("Sent() must be true — something reached the exchange and a re-verify is warranted")
	}
	if f.order != 11 { // stop(+1) then tp(+10)
		t.Errorf("call order marker = %d, want 11 (stop before tp)", f.order)
	}
}

// Apply is the last gate before a live order, so it re-checks rather than
// trusting the caller to have looked at Faults.
func TestApplyRefusesAFaultyPlan(t *testing.T) {
	f := &fakePlacer{}
	bad := BuildPlan(longPos(), 79400, 79500, 0) // stop above mark
	if bad.OK() {
		t.Fatal("precondition: this plan should be faulty")
	}
	r := Apply(context.Background(), f, bad)
	if f.stopCalls != 0 || f.tpCalls != 0 {
		t.Errorf("nothing may be sent for a faulty plan, got %d stop / %d tp calls", f.stopCalls, f.tpCalls)
	}
	if r.Sent() {
		t.Error("Sent() must be false")
	}
	if len(r.Errors) == 0 {
		t.Error("want an error explaining the refusal")
	}
}

// Only what was asked for gets sent.
func TestApplySendsOnlyRequestedLegs(t *testing.T) {
	f := &fakePlacer{}
	Apply(context.Background(), f, BuildPlan(shortPos(), 2470, 2495, 0))
	if f.stopCalls != 1 || f.tpCalls != 0 {
		t.Errorf("stop-only: %d stop / %d tp", f.stopCalls, f.tpCalls)
	}
	g := &fakePlacer{}
	Apply(context.Background(), g, BuildPlan(shortPos(), 2470, 0, 2400))
	if g.stopCalls != 0 || g.tpCalls != 1 {
		t.Errorf("tp-only: %d stop / %d tp", g.stopCalls, g.tpCalls)
	}
}
