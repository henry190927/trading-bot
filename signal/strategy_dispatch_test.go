package signal

import (
	"testing"

	"myFirstGo/trading-bot/market"
)

// The allowlist table itself is asserted by TestStrategyForAltAssignments in
// strategy_momentum_test.go — not duplicated here. What this file covers is the
// dispatch OVERRIDE, which is what makes an SM-vs-MR A/B possible at all.

// This is the whole reason StructMomentumOff exists: on an already-assigned
// symbol, the baseline arm and the variant arm would both run SM, and the A/B
// would compare a strategy against itself.
func TestStructMomentumOffMakesTheBaselineReal(t *testing.T) {
	sym, tf := market.SOLUSDT, market.Timeframe("1h")

	// Baseline arm as it WOULD have been: no flags at all.
	if StrategyFor(sym, tf) != StrategyStructMomentum {
		t.Fatal("precondition: SOL/1h must be allowlisted to SM for this test to mean anything")
	}

	dispatchesSM := func() bool {
		return !StructMomentumOff && (StructMomentumEnabled || StrategyFor(sym, tf) == StrategyStructMomentum)
	}

	// Arm A (old baseline) and Arm B (variant) — identical, which is the bug.
	StructMomentumEnabled, StructMomentumOff = false, false
	armAOld := dispatchesSM()
	StructMomentumEnabled, StructMomentumOff = true, false
	armB := dispatchesSM()
	if armAOld != armB {
		t.Error("expected the OLD arms to be identical — if they differ, this test no longer describes the bug")
	}
	if !armAOld {
		t.Error("both old arms should have dispatched SM")
	}

	// Arm A with the override: now genuinely MR.
	StructMomentumEnabled, StructMomentumOff = false, true
	if dispatchesSM() {
		t.Error("StructMomentumOff must force MR even though the allowlist assigns SM")
	}

	// And it beats the force-on flag, so a mis-set pair cannot silently
	// re-enable SM in the baseline arm.
	StructMomentumEnabled, StructMomentumOff = true, true
	if dispatchesSM() {
		t.Error("StructMomentumOff must take precedence over StructMomentumEnabled")
	}

	t.Cleanup(func() { StructMomentumEnabled, StructMomentumOff = false, false })
}

// A symbol NOT on the allowlist must be unaffected by the override (it was
// already MR), so the flag cannot quietly change the control group.
func TestStructMomentumOffNoOpForMRSymbols(t *testing.T) {
	t.Cleanup(func() { StructMomentumEnabled, StructMomentumOff = false, false })
	sym, tf := market.BTCUSDT, market.Timeframe("1h")
	for _, off := range []bool{false, true} {
		StructMomentumOff = off
		if StrategyFor(sym, tf) != StrategyMR {
			t.Errorf("BTC/1h with off=%v: want MR", off)
		}
	}
}

// The two removals of 2026-09-03, asserted separately so a revert cannot pass
// quietly: HYPE 1h failed the gate 0/3 and LINK 1h managed 1/3, both on
// adequate sample sizes.
func TestPhase2RemovalsStayRemoved(t *testing.T) {
	for _, tc := range []struct {
		sym market.Symbol
		tf  market.Timeframe
		why string
	}{
		{market.HYPEUSDT, "1h", "FAIL 0/3 (MR +1.35/+4.22/-1.28 beat SM -0.61/-1.90/-1.94, n=11/18/23)"},
		{market.LINKUSDT, "1h", "1/3 (MR -0.08/+5.69/-4.05 vs SM -3.66/+2.22/+0.71, n=11/14/21)"},
	} {
		if got := StrategyFor(tc.sym, tc.tf); got != StrategyMR {
			t.Errorf("%s/%s = %v, want MR — it was removed because %s", tc.sym, tc.tf, got, tc.why)
		}
	}
}

// HYPE has no StructMomentum assignment on ANY timeframe now.
func TestHYPEIsFullyMR(t *testing.T) {
	for _, tf := range []market.Timeframe{"5m", "15m", "1h", "2h", "4h", "1d"} {
		if got := StrategyFor(market.HYPEUSDT, tf); got != StrategyMR {
			t.Errorf("HYPE/%s = %v, want MR on every timeframe", tf, got)
		}
	}
}

// SOL 1h is the one strongly-validated pair; it must not be dropped by a
// careless edit to the switch.
func TestSOL1hKeepsStructMomentum(t *testing.T) {
	if StrategyFor(market.SOLUSDT, "1h") != StrategyStructMomentum {
		t.Error("SOL/1h must stay StructMomentum — PASS 3/3, and MR loses -19 to -21R in every window there")
	}
}
