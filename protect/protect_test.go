package protect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
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

type fakeMover struct {
	calls     []string
	placeErr  error
	cancelErr map[string]error
}

func (f *fakeMover) PlaceStopMarket(_ context.Context, _ market.Symbol, _ string, qty, stop float64, _ bool) (*bingx.OrderResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("place %g@%g", qty, stop))
	if f.placeErr != nil {
		return nil, f.placeErr
	}
	return &bingx.OrderResult{OrderID: "NEW"}, nil
}

func (f *fakeMover) PlaceReduceOnlyLimit(context.Context, market.Symbol, string, float64, float64, bool) (*bingx.OrderResult, error) {
	f.calls = append(f.calls, "limit")
	return &bingx.OrderResult{OrderID: "L"}, nil
}

func (f *fakeMover) CancelOrder(_ context.Context, _ market.Symbol, id string) error {
	f.calls = append(f.calls, "cancel "+id)
	return f.cancelErr[id]
}

func movePlan() Plan {
	pos := &bingx.Position{Symbol: "HYPE-USDT", Side: "long", Quantity: 9.3256, EntryPrice: 80.424, PositionSide: "LONG"}
	return BuildPlan(pos, 81.2, 80.09, 0)
}

func TestMoveStopPlacesBeforeCancelling(t *testing.T) {
	f := &fakeMover{}
	r := MoveStop(context.Background(), f, movePlan(), []string{"OLD1", "OLD2"})
	want := []string{"place 9.3256@80.09", "cancel OLD1", "cancel OLD2"}
	if fmt.Sprint(f.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if r.NewStopID != "NEW" || len(r.Cancelled) != 2 || len(r.Errors) != 0 {
		t.Errorf("result = %+v", r)
	}
}

func TestMoveStopKeepsOldStopWhenPlaceFails(t *testing.T) {
	f := &fakeMover{placeErr: errors.New("rejected")}
	r := MoveStop(context.Background(), f, movePlan(), []string{"OLD1"})
	if len(f.calls) != 1 || r.NewStopID != "" || len(r.Cancelled) != 0 {
		t.Errorf("a failed place must not cancel anything: calls %v result %+v", f.calls, r)
	}
}

func TestMoveStopReportsFailedCancel(t *testing.T) {
	f := &fakeMover{cancelErr: map[string]error{"OLD1": errors.New("80014")}}
	r := MoveStop(context.Background(), f, movePlan(), []string{"OLD1", "OLD2"})
	if r.NewStopID != "NEW" || fmt.Sprint(r.Cancelled) != "[OLD2]" || len(r.Errors) != 1 {
		t.Errorf("result = %+v", r)
	}
}

func TestMoveStopRefusesBadPlans(t *testing.T) {
	pos := &bingx.Position{Symbol: "HYPE-USDT", Side: "long", Quantity: 1, EntryPrice: 80}
	for name, p := range map[string]Plan{
		"stop above mark": BuildPlan(pos, 81, 81.5, 0),
		"with a tp":       BuildPlan(pos, 81, 79, 83),
	} {
		f := &fakeMover{}
		if r := MoveStop(context.Background(), f, p, []string{"OLD"}); len(f.calls) != 0 || len(r.Errors) == 0 {
			t.Errorf("%s: sent %v", name, f.calls)
		}
	}
}

// fakeTPMover records every call as "cancel ID" or "limit QTY@PRICE".
// Limit calls are numbered from 1 and return "L<n>", failing or not, so a
// test can name the order a put-back created.
type fakeTPMover struct {
	calls     []string
	n         int
	limitErr  map[float64]error
	cancelErr map[string]error
}

func (f *fakeTPMover) PlaceStopMarket(_ context.Context, _ market.Symbol, _ string, qty, stop float64, _ bool) (*bingx.OrderResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("stop %g@%g", qty, stop))
	return &bingx.OrderResult{OrderID: "S"}, nil
}

func (f *fakeTPMover) PlaceReduceOnlyLimit(_ context.Context, _ market.Symbol, _ string, qty, price float64, _ bool) (*bingx.OrderResult, error) {
	f.n++
	f.calls = append(f.calls, fmt.Sprintf("limit %g@%g", qty, price))
	if err := f.limitErr[price]; err != nil {
		return nil, err
	}
	return &bingx.OrderResult{OrderID: fmt.Sprintf("L%d", f.n)}, nil
}

func (f *fakeTPMover) CancelOrder(_ context.Context, _ market.Symbol, id string) error {
	f.calls = append(f.calls, "cancel "+id)
	return f.cancelErr[id]
}

// Long HYPE, 9.3256 @ 80.424, mark 81.2, new take-profit 83 (above the mark).
func tpPlan() Plan {
	pos := &bingx.Position{Symbol: "HYPE-USDT", Side: "long", Quantity: 9.3256, EntryPrice: 80.424, PositionSide: "LONG"}
	return BuildPlan(pos, 81.2, 0, 83)
}

func TestMoveTPCancelsThenPlaces(t *testing.T) {
	f := &fakeTPMover{}
	r := MoveTP(context.Background(), f, tpPlan(), []RestingTP{{"OLD1", 82.5}})
	if want := "[cancel OLD1 limit 9.3256@83]"; fmt.Sprint(f.calls) != want {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if r.NewTPID != "L1" || fmt.Sprint(r.Cancelled) != "[OLD1]" || len(r.Restored) != 0 || len(r.Errors) != 0 {
		t.Errorf("result = %+v", r)
	}
}

func TestMoveTPWithNothingToReplace(t *testing.T) {
	f := &fakeTPMover{}
	r := MoveTP(context.Background(), f, tpPlan(), nil)
	if fmt.Sprint(f.calls) != "[limit 9.3256@83]" || r.NewTPID != "L1" || len(r.Errors) != 0 {
		t.Errorf("calls %v result %+v", f.calls, r)
	}
}

// The new order is refused: the old take-profit is put back at 82.5. It is
// the second limit call, so its id is L2.
func TestMoveTPPutsOldBackWhenPlaceFails(t *testing.T) {
	f := &fakeTPMover{limitErr: map[float64]error{83: errors.New("rejected")}}
	r := MoveTP(context.Background(), f, tpPlan(), []RestingTP{{"OLD1", 82.5}})
	if want := "[cancel OLD1 limit 9.3256@83 limit 9.3256@82.5]"; fmt.Sprint(f.calls) != want {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if r.NewTPID != "" || fmt.Sprint(r.Restored) != "[L2]" || len(r.Errors) != 1 {
		t.Errorf("result = %+v", r)
	}
}

func TestMoveTPSaysSoWhenNothingIsLeft(t *testing.T) {
	f := &fakeTPMover{limitErr: map[float64]error{83: errors.New("rejected"), 82.5: errors.New("rejected")}}
	r := MoveTP(context.Background(), f, tpPlan(), []RestingTP{{"OLD1", 82.5}})
	if r.NewTPID != "" || len(r.Restored) != 0 || len(r.Errors) != 2 {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(strings.ToLower(strings.Join(r.Errors, " ")), "no take-profit") {
		t.Errorf("errors %q must tell the trader there is no take-profit left", r.Errors)
	}
}

// OLD1 cancels, OLD2 refuses: stop there, place nothing new, put OLD1 back
// (the first limit call, L1).
func TestMoveTPStopsAtAFailedCancel(t *testing.T) {
	f := &fakeTPMover{cancelErr: map[string]error{"OLD2": errors.New("80014")}}
	r := MoveTP(context.Background(), f, tpPlan(), []RestingTP{{"OLD1", 82.5}, {"OLD2", 84}})
	if want := "[cancel OLD1 cancel OLD2 limit 9.3256@82.5]"; fmt.Sprint(f.calls) != want {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if r.NewTPID != "" || fmt.Sprint(r.Cancelled) != "[OLD1]" || fmt.Sprint(r.Restored) != "[L1]" || len(r.Errors) != 1 {
		t.Errorf("result = %+v", r)
	}
}

func TestMoveTPRefusesBadPlans(t *testing.T) {
	pos := &bingx.Position{Symbol: "HYPE-USDT", Side: "long", Quantity: 9.3256, EntryPrice: 80.424, PositionSide: "LONG"}
	for name, p := range map[string]Plan{
		"tp below the mark": BuildPlan(pos, 81.2, 0, 80),
		"with a stop":       BuildPlan(pos, 81.2, 80.09, 83),
	} {
		f := &fakeTPMover{}
		r := MoveTP(context.Background(), f, p, []RestingTP{{"OLD1", 82.5}})
		if len(f.calls) != 0 || r.NewTPID != "" || len(r.Errors) == 0 {
			t.Errorf("%s: calls %v result %+v, want refused with no calls", name, f.calls, r)
		}
	}
}
