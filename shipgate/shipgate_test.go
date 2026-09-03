package shipgate

import "testing"

// Every case below is a REAL result from 2026-09-03/04. That is the point: the
// gate is only worth having if it reaches the same verdict a careful human
// reached by hand, on the results that actually caused trouble.

func w(days int, netR float64, trades int, wr float64) Window {
	return Window{Days: days, NetR: netR, Trades: trades, WinRate: wr}
}

// SUI 1h: StructMomentum beat the MR baseline in ALL THREE windows and was
// NEGATIVE in all three. The old prose gate passed it. This is the case the
// package exists for.
func TestSUI1hPassesRelativeButFailsAbsolute(t *testing.T) {
	sm := Arm{"SUI 1h struct-momentum", []Window{
		w(60, -2.44, 9, 33.3), w(90, -2.44, 15, 40.0), w(120, -0.69, 20, 40.0),
	}}
	mr := Arm{"SUI 1h mr", []Window{
		w(60, -6.10, 34, 40.6), w(90, -8.13, 49, 39.5), w(120, -18.05, 64, 30.9),
	}}
	v := Evaluate(sm, &mr, Default())
	if v.Result != Fail {
		t.Fatalf("want FAIL, got %s — this is the exact result the old gate let through\n%s", v.Result, v)
	}
	// It must fail on ABSOLUTE grounds, and the verdict must say the relative
	// criterion was satisfied, or the reader will think it lost to MR.
	foundAbs := false
	for _, r := range v.Reasons {
		if contains(r, "LOSES money") {
			foundAbs = true
		}
		if contains(r, "beat the baseline in only") {
			t.Errorf("must NOT claim it lost to the baseline — it beat MR in all 3: %s", r)
		}
	}
	if !foundAbs {
		t.Errorf("no absolute-grounds reason given: %+v", v.Reasons)
	}
	foundNote := false
	for _, n := range v.Notes {
		if contains(n, "better than a disaster") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("the beat-baseline-but-still-losing note is the whole explanation, and it is missing: %+v", v.Notes)
	}
}

// SOL 1h: the one genuinely strong StructMomentum result. Must PASS.
func TestSOL1hPasses(t *testing.T) {
	sm := Arm{"SOL 1h struct-momentum", []Window{
		w(60, +3.65, 14, 50.0), w(90, +2.48, 18, 44.4), w(120, +8.47, 23, 52.2),
	}}
	mr := Arm{"SOL 1h mr", []Window{
		w(60, -19.37, 36, 12.9), w(90, -21.17, 54, 27.1), w(120, -20.10, 75, 27.7),
	}}
	if v := Evaluate(sm, &mr, Default()); v.Result != Pass {
		t.Fatalf("want PASS, got %s\n%s", v.Result, v)
	}
}

// HYPE 1h: lost to MR in every window. Straight FAIL, on relative grounds.
func TestHYPE1hFailsRelative(t *testing.T) {
	sm := Arm{"HYPE 1h struct-momentum", []Window{
		w(60, -0.61, 11, 36.4), w(90, -1.90, 18, 33.3), w(120, -1.94, 23, 34.8),
	}}
	mr := Arm{"HYPE 1h mr", []Window{
		w(60, +1.35, 34, 43.8), w(90, +4.22, 55, 44.2), w(120, -1.28, 73, 40.3),
	}}
	v := Evaluate(sm, &mr, Default())
	if v.Result != Fail {
		t.Fatalf("want FAIL, got %s\n%s", v.Result, v)
	}
	got := false
	for _, r := range v.Reasons {
		if contains(r, "beat the baseline in only") {
			got = true
		}
	}
	if !got {
		t.Errorf("should name the relative failure too: %+v", v.Reasons)
	}
}

// StructMomentum on 2h fired 3-14 times per window. That is UNDECIDED, not
// FAIL — the two lead to opposite actions, and calling it FAIL would retire a
// strategy on noise.
func TestThinSamplesAreUndecidedNotFail(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  Arm
	}{
		{"SOL 2h", Arm{"SOL 2h sm", []Window{w(60, +1.63, 4, 50), w(90, +2.55, 6, 50), w(120, +5.17, 12, 50)}}},
		{"LINK 2h", Arm{"LINK 2h sm", []Window{w(60, -0.18, 3, 33), w(90, +1.15, 6, 50), w(120, +2.01, 11, 45)}}},
	} {
		v := Evaluate(tc.arm, nil, Default())
		if v.Result != Undecided {
			t.Errorf("%s: want UNDECIDED, got %s — n=%d cannot decide\n%s", tc.name, v.Result, v.MinTrades, v)
		}
	}
	// And a thin-but-POSITIVE arm must not sneak a PASS either.
	thin := Arm{"thin winner", []Window{w(60, +9, 4, 75), w(90, +9, 5, 75), w(120, +9, 6, 75)}}
	if v := Evaluate(thin, nil, Default()); v.Result != Undecided {
		t.Errorf("a thin positive arm must be UNDECIDED, got %s", v.Result)
	}
}

// pivot-zone-fade CONFIRM: better than TOUCH in every window and every nearby
// parameter, at -0.119 R/trade. Must FAIL.
func TestConfirmArmFailsDespiteBeatingTouch(t *testing.T) {
	confirm := Arm{"zonefade CONFIRM", []Window{
		w(60, -30.61, 311, 29), w(90, -44.93, 377, 29), w(120, -33.23, 528, 30),
	}}
	touch := Arm{"zonefade TOUCH", []Window{
		w(60, -124.00, 388, 15), w(90, -195.00, 475, 15), w(120, -265.00, 661, 15),
	}}
	v := Evaluate(confirm, &touch, Default())
	if v.Result != Fail {
		t.Fatalf("want FAIL, got %s\n%s", v.Result, v)
	}
	if v.MedRPT >= 0 {
		t.Errorf("median R/trade should be negative, got %+.3f", v.MedRPT)
	}
}

// The stock-symbol additions, which had no baseline (absolute gate only).
func TestStockAdditions(t *testing.T) {
	for _, tc := range []struct {
		arm  Arm
		want Result
	}{
		{Arm{"SPCX mr+veto", []Window{w(60, +7.94, 26, 57.1), w(90, +9.59, 37, 51.6), w(120, +5.87, 43, 44.1)}}, Pass},
		{Arm{"MSTR mr+veto", []Window{w(60, +8.34, 15, 53.8), w(90, +7.42, 20, 58.8), w(120, +3.92, 25, 45.5)}}, Pass},
		{Arm{"APP sweep-reject", []Window{w(60, +4.00, 18, 41), w(90, +10.00, 33, 44), w(120, +12.00, 49, 42)}}, Pass},
		{Arm{"APP mr", []Window{w(60, -12.47, 27, 19.2), w(90, -21.23, 45, 19.0), w(120, -22.64, 69, 30.3)}}, Fail},
		{Arm{"WDC mr", []Window{w(60, +2.11, 41, 39.0), w(90, -3.79, 54, 34.6), w(120, -6.76, 74, 35.2)}}, Fail},
		{Arm{"MU mr+veto", []Window{w(60, -0.56, 18, 42.9), w(90, -0.96, 25, 45.0), w(120, -3.24, 35, 38.5)}}, Fail},
	} {
		if v := Evaluate(tc.arm, nil, Default()); v.Result != tc.want {
			t.Errorf("%s: want %s, got %s\n%s", tc.arm.Name, tc.want, v.Result, v)
		}
	}
}

// A single losing window must sink an otherwise-good arm. Aggregate would
// have hidden it, which is why the gate is per-window.
func TestOneLosingWindowFails(t *testing.T) {
	arm := Arm{"aggregate-positive but one bad window", []Window{
		w(60, +30.00, 40, 60), w(90, -0.50, 45, 45), w(120, +25.00, 50, 55),
	}}
	v := Evaluate(arm, nil, Default())
	if v.Result != Fail {
		t.Fatalf("want FAIL — aggregate is +54.5R but one window loses. got %s\n%s", v.Result, v)
	}
	if len(v.Reasons) != 1 || !contains(v.Reasons[0], "90d") {
		t.Errorf("should name the 90d window specifically: %+v", v.Reasons)
	}
}

// Every failed criterion is reported, not just the first: fixing one would not
// save an arm that fails on both counts.
func TestAllReasonsReported(t *testing.T) {
	arm := Arm{"bad on both counts", []Window{
		w(60, -5, 40, 20), w(90, -6, 45, 20), w(120, -7, 50, 20),
	}}
	base := Arm{"baseline", []Window{w(60, +1, 40, 50), w(90, +1, 45, 50), w(120, +1, 50, 50)}}
	v := Evaluate(arm, &base, Default())
	if len(v.Reasons) < 4 {
		t.Errorf("want a reason per losing window plus absolute plus relative, got %d: %+v", len(v.Reasons), v.Reasons)
	}
}

// Zero-trade windows must not divide by zero into a fake 0.000 R/trade.
func TestZeroTradesSafe(t *testing.T) {
	if got := (Window{Days: 60, NetR: 5, Trades: 0}).RPerTrade(); got != 0 {
		t.Errorf("RPerTrade with 0 trades = %v, want 0", got)
	}
	arm := Arm{"empty", []Window{w(60, 0, 0, 0), w(90, 0, 0, 0), w(120, 0, 0, 0)}}
	if v := Evaluate(arm, nil, Default()); v.Result != Undecided {
		t.Errorf("all-empty windows must be UNDECIDED, got %s", v.Result)
	}
}

// A baseline whose windows don't line up must not silently satisfy the
// relative criterion.
func TestMismatchedBaselineWindowsNoted(t *testing.T) {
	arm := Arm{"arm", []Window{w(60, +5, 40, 60), w(90, +5, 45, 60), w(120, +5, 50, 60)}}
	base := Arm{"base", []Window{w(30, +99, 40, 90), w(45, +99, 45, 90), w(200, +99, 50, 90)}}
	v := Evaluate(arm, &base, Default())
	if v.Result != Pass {
		t.Errorf("absolute criteria are met, so it should PASS: %s", v)
	}
	got := false
	for _, n := range v.Notes {
		if contains(n, "no matching windows") {
			got = true
		}
	}
	if !got {
		t.Errorf("must NOTE that the relative criterion was skipped rather than pretend it applied: %+v", v.Notes)
	}
}

// Too few windows is a data problem, not a verdict — ONDS has 62 days.
func TestTooFewWindowsUndecided(t *testing.T) {
	arm := Arm{"ONDS-like", []Window{w(60, +12, 40, 60)}}
	v := Evaluate(arm, nil, Default())
	if v.Result != Undecided {
		t.Fatalf("want UNDECIDED, got %s", v.Result)
	}
	if !contains(v.Reasons[0], "only 1 window") {
		t.Errorf("reason should name the window shortfall: %v", v.Reasons)
	}
}

func TestMedian(t *testing.T) {
	if median(nil) != 0 {
		t.Error("nil")
	}
	if median([]float64{5}) != 5 {
		t.Error("single")
	}
	if median([]float64{3, 1, 2}) != 2 {
		t.Error("odd")
	}
	if median([]float64{4, 1, 3, 2}) != 2.5 {
		t.Error("even")
	}
	in := []float64{3, 1, 2}
	_ = median(in)
	if in[0] != 3 {
		t.Error("median mutated its input")
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
