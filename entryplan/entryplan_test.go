package entryplan

import (
	"math"
	"strings"
	"testing"
)

// The live order placed 2026-09-15: ETH long, limit 2441, stop 2424.88,
// tp 2480.50, 56.44u margin at 125x, mark 2474.25, equity 289.82. ETH's
// quantity precision is 2 decimals.
func ethLong() Inputs {
	return Inputs{
		Side: "long", Entry: 2441, Stop: 2424.88, TP: 2480.50,
		MarginUSDT: 56.44, Leverage: 125, Mark: 2474.25, Equity: 289.82, QtyStep: 2,
	}
}

func has(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestBuildSizesTheLiveOrder(t *testing.T) {
	p := Build(ethLong())
	if !p.OK() {
		t.Fatalf("faults on a valid plan: %v", p.Faults)
	}
	// 56.44 * 125 / 2441 = 2.8902...  → floors to 2.89 at 2 dp.
	if math.Abs(p.Qty-2.89) > 1e-9 {
		t.Errorf("qty = %v, want 2.89", p.Qty)
	}
	// 2.89 * 2441 = 7054.49
	if math.Abs(p.NotionalUSDT-7054.49) > 1e-6 {
		t.Errorf("notional = %.4f, want 7054.49", p.NotionalUSDT)
	}
	// stop distance 2441 - 2424.88 = 16.12; 2.89 * 16.12 = 46.5868
	if math.Abs(p.RiskUSDT-46.5868) > 1e-6 {
		t.Errorf("risk = %.4f, want 46.5868", p.RiskUSDT)
	}
	// 46.5868 / 289.82 = 16.074...%
	if math.Abs(p.RiskPctEquity-16.0744) > 1e-3 {
		t.Errorf("risk%%equity = %.4f, want ~16.0744", p.RiskPctEquity)
	}
	// target distance 39.50 / stop distance 16.12 = 2.4504...
	if math.Abs(p.RR-2.4504) > 1e-3 {
		t.Errorf("RR = %.4f, want ~2.4504", p.RR)
	}
	// 16.07% is over the 15% shout line but must NOT block.
	if !has(p.Warnings, "% of") {
		t.Errorf("expected a risk-vs-equity warning, got %v", p.Warnings)
	}
	if p.Marketable {
		t.Error("2441 is 1.3% below a 2474.25 mark — not marketable")
	}
}

// The rule with no override.
func TestStopIsRequired(t *testing.T) {
	in := ethLong()
	in.Stop = 0
	p := Build(in)
	if p.OK() {
		t.Fatal("a stopless entry was accepted")
	}
	if !has(p.Faults, "does not place naked entries") {
		t.Errorf("faults = %v, want the naked-entry refusal", p.Faults)
	}
	// AllowMarketable is the only escape hatch in this package and it must not
	// reach this rule.
	in.AllowMarketable = true
	if Build(in).OK() {
		t.Error("allow_marketable let a stopless entry through")
	}
}

func TestStopAndTPMustStraddleTheEntry(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*Inputs)
		want string
	}{
		{"long stop above entry", func(i *Inputs) { i.Stop = 2450 }, "must be BELOW entry"},
		{"long tp below entry", func(i *Inputs) { i.TP = 2400 }, "must be ABOVE entry"},
		{"short stop below entry", func(i *Inputs) {
			i.Side, i.Mark, i.Entry, i.Stop, i.TP = "short", 2400, 2441, 2430, 2400
		}, "must be ABOVE entry"},
		{"short tp above entry", func(i *Inputs) {
			i.Side, i.Mark, i.Entry, i.Stop, i.TP = "short", 2400, 2441, 2460, 2500
		}, "must be BELOW entry"},
	} {
		in := ethLong()
		c.mut(&in)
		p := Build(in)
		if p.OK() {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !has(p.Faults, c.want) {
			t.Errorf("%s: faults = %v, want %q", c.name, p.Faults, c.want)
		}
	}
}

// A long limit at or above the mark is a market buy. Blocked by default
// because chasing is this account's most expensive habit, and allowed on an
// explicit flag because a stop-entry breakout is a real thing.
func TestMarketableNeedsTheFlag(t *testing.T) {
	in := ethLong()
	in.Entry = 2480 // above the 2474.25 mark
	in.Stop = 2460
	in.TP = 2520
	p := Build(in)
	if p.OK() || !has(p.Faults, "market order") {
		t.Fatalf("marketable long accepted or mis-worded: ok=%v faults=%v", p.OK(), p.Faults)
	}
	in.AllowMarketable = true
	p = Build(in)
	if !p.OK() {
		t.Fatalf("allow_marketable did not let it through: %v", p.Faults)
	}
	if !p.Marketable {
		t.Error("Marketable should still be reported true — the flag permits it, not hides it")
	}

	// A short at or below the mark is the mirror case.
	in2 := ethLong()
	in2.Side, in2.Entry, in2.Stop, in2.TP, in2.Mark = "short", 2470, 2490, 2440, 2474.25
	if Build(in2).OK() {
		t.Error("marketable short accepted")
	}
}

// Exactly at the mark counts as marketable: a resting order at the mark is
// filled by the next tick in either direction, and calling that "resting"
// would be the same lie as an entry above it.
func TestEntryAtTheMarkIsMarketable(t *testing.T) {
	in := ethLong()
	in.Entry, in.Mark = 2474.25, 2474.25
	in.Stop = 2460
	if p := Build(in); !p.Marketable {
		t.Error("entry == mark should be marketable")
	}
}

func TestQtyRoundingToZeroIsAFault(t *testing.T) {
	in := ethLong()
	in.MarginUSDT, in.Leverage, in.QtyStep = 0.01, 1, 2 // 0.01/2441 → 0.000004
	p := Build(in)
	if p.OK() || !has(p.Faults, "rounds to 0") {
		t.Errorf("ok=%v faults=%v, want a qty-rounds-to-zero fault", p.OK(), p.Faults)
	}
}

// Warnings must never block — the account's risk gate is warn-only by the
// owner's explicit decision and this package must not overrule it.
func TestWarningsDoNotBlock(t *testing.T) {
	in := ethLong()
	in.MarginUSDT = 200 // huge: ~35% of equity at risk
	p := Build(in)
	if !p.OK() {
		t.Fatalf("a risky-but-valid plan was blocked: %v", p.Faults)
	}
	if p.RiskPctEquity <= RiskWarnPctEquity {
		t.Fatalf("fixture no longer exceeds the warn line: %.2f%%", p.RiskPctEquity)
	}
	if len(p.Warnings) == 0 {
		t.Error("no warning on a risk this far above the line")
	}
}

func TestNoTPWarnsAndSubOneRWarns(t *testing.T) {
	in := ethLong()
	in.TP = 0
	p := Build(in)
	if !p.OK() {
		t.Fatalf("no-TP must be allowed: %v", p.Faults)
	}
	if p.RR != 0 || p.RewardUSDT != 0 {
		t.Errorf("RR/reward should be zero with no TP, got %v/%v", p.RR, p.RewardUSDT)
	}
	if !has(p.Warnings, "no take-profit") {
		t.Errorf("warnings = %v, want the missing-TP note", p.Warnings)
	}

	// stop 16.12 away, target 8 away → 0.496R
	in2 := ethLong()
	in2.TP = 2449
	p2 := Build(in2)
	if !p2.OK() || !has(p2.Warnings, "below 1R") {
		t.Errorf("sub-1R: ok=%v warnings=%v", p2.OK(), p2.Warnings)
	}
}

func TestBadSideStopsEarly(t *testing.T) {
	in := ethLong()
	in.Side = "buy"
	p := Build(in)
	if p.OK() || len(p.Faults) != 1 {
		t.Errorf("want exactly one fault for a bad side, got %v", p.Faults)
	}
}
