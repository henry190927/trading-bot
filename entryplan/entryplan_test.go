package entryplan

import (
	"math"
	"strings"
	"testing"
)

// A synthetic long, chosen so every expectation below is exact arithmetic
// rather than a number read back off a run:
//
//	qty      60 * 125 / 2400 = 3.125       -> 3.12 at 2 dp
//	notional 3.12 * 2400                   = 7488.00
//	risk     3.12 * (2400 - 2384)          = 49.92
//	risk%    49.92 / 300                   = 16.64%  (over the 15% warn line)
//	RR       (2440 - 2400) / (2400 - 2384) = 2.50
func ethLong() Inputs {
	return Inputs{
		Side: "long", Entry: 2400, Stop: 2384, TP: 2440,
		MarginUSDT: 60, Leverage: 125, Mark: 2430, Equity: 300, QtyStep: 2,
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

func TestBuildSizesAndScoresThePlan(t *testing.T) {
	p := Build(ethLong())
	if !p.OK() {
		t.Fatalf("faults on a valid plan: %v", p.Faults)
	}
	if math.Abs(p.Qty-3.12) > 1e-9 {
		t.Errorf("qty = %v, want 3.12 (3.125 floored at 2 dp)", p.Qty)
	}
	if math.Abs(p.NotionalUSDT-7488) > 1e-6 {
		t.Errorf("notional = %.4f, want 7488", p.NotionalUSDT)
	}
	if math.Abs(p.RiskUSDT-49.92) > 1e-6 {
		t.Errorf("risk = %.4f, want 49.92", p.RiskUSDT)
	}
	if math.Abs(p.RiskPctEquity-16.64) > 1e-6 {
		t.Errorf("risk%%equity = %.4f, want 16.64", p.RiskPctEquity)
	}
	if math.Abs(p.RewardUSDT-124.80) > 1e-6 {
		t.Errorf("reward = %.4f, want 124.80", p.RewardUSDT)
	}
	if math.Abs(p.RR-2.5) > 1e-9 {
		t.Errorf("RR = %.4f, want 2.50", p.RR)
	}
	// 16.64% is over the warn line and must NOT block.
	if !has(p.Warnings, "% of") {
		t.Errorf("expected a risk-vs-equity warning, got %v", p.Warnings)
	}
	if p.Marketable {
		t.Error("a 2400 long against a 2430 mark is not marketable")
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
		{"long stop above entry", func(i *Inputs) { i.Stop = 2410 }, "must be BELOW entry"},
		{"long tp below entry", func(i *Inputs) { i.TP = 2380 }, "must be ABOVE entry"},
		{"short stop below entry", func(i *Inputs) {
			i.Side, i.Mark, i.Entry, i.Stop, i.TP = "short", 2370, 2400, 2390, 2360
		}, "must be ABOVE entry"},
		{"short tp above entry", func(i *Inputs) {
			i.Side, i.Mark, i.Entry, i.Stop, i.TP = "short", 2370, 2400, 2420, 2460
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
// because a chased entry is the failure this exists to stop, and allowed on an
// explicit flag because a stop-entry breakout is a real thing.
func TestMarketableNeedsTheFlag(t *testing.T) {
	in := ethLong()
	in.Entry = 2450 // above the 2430 mark
	in.Stop = 2430
	in.TP = 2500
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
	in2.Side, in2.Entry, in2.Stop, in2.TP, in2.Mark = "short", 2420, 2450, 2380, 2430
	if Build(in2).OK() {
		t.Error("marketable short accepted")
	}
}

// Exactly at the mark counts as marketable: a resting order at the mark is
// filled by the next tick in either direction, and calling that "resting"
// would be the same lie as an entry above it.
func TestEntryAtTheMarkIsMarketable(t *testing.T) {
	in := ethLong()
	in.Entry, in.Mark = 2430, 2430
	in.Stop = 2410
	if p := Build(in); !p.Marketable {
		t.Error("entry == mark should be marketable")
	}
}

func TestQtyRoundingToZeroIsAFault(t *testing.T) {
	in := ethLong()
	in.MarginUSDT, in.Leverage, in.QtyStep = 0.01, 1, 2 // 0.01/2400 → 0.0000042
	p := Build(in)
	if p.OK() || !has(p.Faults, "rounds to 0") {
		t.Errorf("ok=%v faults=%v, want a qty-rounds-to-zero fault", p.OK(), p.Faults)
	}
}

// Warnings must never block — the account's risk gate is warn-only by the
// owner's explicit decision and this package must not overrule it.
func TestWarningsDoNotBlock(t *testing.T) {
	in := ethLong()
	in.MarginUSDT = 200 // 10.41 qty x 16 = 166.56u risk on 300u equity
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

	// stop 16 away, target 8 away → 0.5R
	in2 := ethLong()
	in2.TP = 2408
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
