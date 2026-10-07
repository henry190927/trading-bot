package shipgate

import (
	"strings"
	"testing"
)

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
	sm := Arm{Name: "SUI 1h struct-momentum", Windows: []Window{
		w(60, -2.44, 9, 33.3), w(90, -2.44, 15, 40.0), w(120, -0.69, 20, 40.0),
	}}
	mr := Arm{Name: "SUI 1h mr", Windows: []Window{
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
	sm := Arm{Name: "SOL 1h struct-momentum", Windows: []Window{
		w(60, +3.65, 14, 50.0), w(90, +2.48, 18, 44.4), w(120, +8.47, 23, 52.2),
	}}
	mr := Arm{Name: "SOL 1h mr", Windows: []Window{
		w(60, -19.37, 36, 12.9), w(90, -21.17, 54, 27.1), w(120, -20.10, 75, 27.7),
	}}
	if v := Evaluate(sm, &mr, Default()); v.Result != Pass {
		t.Fatalf("want PASS, got %s\n%s", v.Result, v)
	}
}

// HYPE 1h: lost to MR in every window. Straight FAIL, on relative grounds.
func TestHYPE1hFailsRelative(t *testing.T) {
	sm := Arm{Name: "HYPE 1h struct-momentum", Windows: []Window{
		w(60, -0.61, 11, 36.4), w(90, -1.90, 18, 33.3), w(120, -1.94, 23, 34.8),
	}}
	mr := Arm{Name: "HYPE 1h mr", Windows: []Window{
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
		{"SOL 2h", Arm{Name: "SOL 2h sm", Windows: []Window{w(60, +1.63, 4, 50), w(90, +2.55, 6, 50), w(120, +5.17, 12, 50)}}},
		{"LINK 2h", Arm{Name: "LINK 2h sm", Windows: []Window{w(60, -0.18, 3, 33), w(90, +1.15, 6, 50), w(120, +2.01, 11, 45)}}},
	} {
		v := Evaluate(tc.arm, nil, Default())
		if v.Result != Undecided {
			t.Errorf("%s: want UNDECIDED, got %s — n=%d cannot decide\n%s", tc.name, v.Result, v.MinTrades, v)
		}
	}
	// And a thin-but-POSITIVE arm must not sneak a PASS either.
	thin := Arm{Name: "thin winner", Windows: []Window{w(60, +9, 4, 75), w(90, +9, 5, 75), w(120, +9, 6, 75)}}
	if v := Evaluate(thin, nil, Default()); v.Result != Undecided {
		t.Errorf("a thin positive arm must be UNDECIDED, got %s", v.Result)
	}
}

// pivot-zone-fade CONFIRM: better than TOUCH in every window and every nearby
// parameter, at -0.119 R/trade. Must FAIL.
func TestConfirmArmFailsDespiteBeatingTouch(t *testing.T) {
	confirm := Arm{Name: "zonefade CONFIRM", Windows: []Window{
		w(60, -30.61, 311, 29), w(90, -44.93, 377, 29), w(120, -33.23, 528, 30),
	}}
	touch := Arm{Name: "zonefade TOUCH", Windows: []Window{
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
		{Arm{Name: "SPCX mr+veto", Windows: []Window{w(60, +7.94, 26, 57.1), w(90, +9.59, 37, 51.6), w(120, +5.87, 43, 44.1)}}, Pass},
		{Arm{Name: "MSTR mr+veto", Windows: []Window{w(60, +8.34, 15, 53.8), w(90, +7.42, 20, 58.8), w(120, +3.92, 25, 45.5)}}, Pass},
		{Arm{Name: "APP sweep-reject", Windows: []Window{w(60, +4.00, 18, 41), w(90, +10.00, 33, 44), w(120, +12.00, 49, 42)}}, Pass},
		{Arm{Name: "APP mr", Windows: []Window{w(60, -12.47, 27, 19.2), w(90, -21.23, 45, 19.0), w(120, -22.64, 69, 30.3)}}, Fail},
		{Arm{Name: "WDC mr", Windows: []Window{w(60, +2.11, 41, 39.0), w(90, -3.79, 54, 34.6), w(120, -6.76, 74, 35.2)}}, Fail},
		{Arm{Name: "MU mr+veto", Windows: []Window{w(60, -0.56, 18, 42.9), w(90, -0.96, 25, 45.0), w(120, -3.24, 35, 38.5)}}, Fail},
	} {
		if v := Evaluate(tc.arm, nil, Default()); v.Result != tc.want {
			t.Errorf("%s: want %s, got %s\n%s", tc.arm.Name, tc.want, v.Result, v)
		}
	}
}

// A single losing window must sink an otherwise-good arm. Aggregate would
// have hidden it, which is why the gate is per-window.
func TestOneLosingWindowFails(t *testing.T) {
	arm := Arm{Name: "aggregate-positive but one bad window", Windows: []Window{
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
	arm := Arm{Name: "bad on both counts", Windows: []Window{
		w(60, -5, 40, 20), w(90, -6, 45, 20), w(120, -7, 50, 20),
	}}
	base := Arm{Name: "baseline", Windows: []Window{w(60, +1, 40, 50), w(90, +1, 45, 50), w(120, +1, 50, 50)}}
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
	arm := Arm{Name: "empty", Windows: []Window{w(60, 0, 0, 0), w(90, 0, 0, 0), w(120, 0, 0, 0)}}
	if v := Evaluate(arm, nil, Default()); v.Result != Undecided {
		t.Errorf("all-empty windows must be UNDECIDED, got %s", v.Result)
	}
}

// A baseline whose windows don't line up must not silently satisfy the
// relative criterion.
func TestMismatchedBaselineWindowsNoted(t *testing.T) {
	arm := Arm{Name: "arm", Windows: []Window{w(60, +5, 40, 60), w(90, +5, 45, 60), w(120, +5, 50, 60)}}
	base := Arm{Name: "base", Windows: []Window{w(30, +99, 40, 90), w(45, +99, 45, 90), w(200, +99, 50, 90)}}
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
	arm := Arm{Name: "ONDS-like", Windows: []Window{w(60, +12, 40, 60)}}
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

// b builds a disjoint bucket: Days carries the END OFFSET, not a length.
func b(endOffset int, netR float64, trades int) Window {
	return Window{Days: endOffset, NetR: netR, Trades: trades}
}

// The case the breadth test was written for: ETH htf-snr on 2026-10-07. Every
// nested window is positive and it PASSES — but the disjoint months are
// -13 / +15 / +2 / +0 / +1, so +5 total becomes -10 without the one good
// month. That is one observation wearing three windows.
func TestConcentratedPassBecomesUndecided(t *testing.T) {
	arm := Arm{
		Name:    "ETH htf-snr",
		Windows: []Window{w(60, 1, 90, 34), w(90, 10, 159, 35), w(120, 37, 219, 39)},
		Buckets: []Window{b(120, -13, 43), b(90, 15, 54), b(60, 2, 50), b(30, 0, 46), b(0, 1, 42)},
	}
	v := Evaluate(arm, nil, Default())
	if v.Result != Undecided {
		t.Fatalf("want UNDECIDED, got %s\n%s", v.Result, v)
	}
	if !v.Concentrated {
		t.Error("want Concentrated")
	}
	if v.BucketTotal != 5 || v.BestBucketR != 15 || v.DropBestR != -10 {
		t.Errorf("got total %+.2f best %+.2f drop %+.2f, want +5 / +15 / -10",
			v.BucketTotal, v.BestBucketR, v.DropBestR)
	}
	// Same windows, no buckets: the old gate says PASS. That contrast is the
	// whole point, so assert it rather than trusting it.
	bare := Arm{Name: arm.Name, Windows: arm.Windows}
	if vb := Evaluate(bare, nil, Default()); vb.Result != Pass {
		t.Fatalf("without buckets the windows alone must still PASS, got %s", vb.Result)
	}
}

// Spread across buckets: +20 total, best +6, still +14 without it.
func TestSpreadPassStaysPass(t *testing.T) {
	arm := Arm{
		Name:    "spread",
		Windows: []Window{w(60, 10, 40, 50), w(90, 15, 60, 50), w(120, 20, 80, 50)},
		Buckets: []Window{b(90, 5, 20), b(60, 6, 20), b(30, 4, 20), b(0, 5, 20)},
	}
	v := Evaluate(arm, nil, Default())
	if v.Result != Pass {
		t.Fatalf("want PASS, got %s\n%s", v.Result, v)
	}
	if v.Concentrated || v.DropBestR != 14 {
		t.Errorf("want spread with drop +14, got concentrated=%v drop %+.2f", v.Concentrated, v.DropBestR)
	}
}

// Concentration must not pile onto a FAIL. A losing rule is already decided,
// and "also concentrated" would read as a second independent reason.
func TestConcentrationDoesNotTouchAFail(t *testing.T) {
	arm := Arm{
		Name:    "loser",
		Windows: []Window{w(60, -7, 70, 30), w(90, -3, 66, 32), w(120, 4, 95, 35)},
		Buckets: []Window{b(90, -9, 20), b(60, 8, 20), b(30, -1, 20), b(0, -4, 20)},
	}
	v := Evaluate(arm, nil, Default())
	if v.Result != Fail {
		t.Fatalf("want FAIL, got %s\n%s", v.Result, v)
	}
	for _, r := range v.Reasons {
		if strings.Contains(r, "one period") {
			t.Errorf("breadth must not add a reason to a FAIL: %q", r)
		}
	}
	// It is still REPORTED, just not weaponised.
	if v.Buckets != 4 {
		t.Errorf("breadth should still be computed and shown, got Buckets=%d", v.Buckets)
	}
}

// Too few buckets is a note, never a downgrade — same posture as MinWindows.
func TestTooFewBucketsNotes(t *testing.T) {
	arm := Arm{
		Name:    "thin buckets",
		Windows: []Window{w(60, 10, 40, 50), w(90, 15, 60, 50), w(120, 20, 80, 50)},
		Buckets: []Window{b(60, -5, 20), b(30, 30, 20), b(0, -5, 20)},
	}
	v := Evaluate(arm, nil, Default())
	if v.Result != Pass {
		t.Fatalf("want PASS (breadth untestable), got %s\n%s", v.Result, v)
	}
	if v.Buckets != 0 {
		t.Errorf("breadth must not be computed from %d buckets", v.Buckets)
	}
	if len(v.Notes) == 0 {
		t.Error("want a note saying breadth was untested")
	}
}

// No buckets at all still PASSes, but the verdict has to admit what it did
// not check.
func TestNoBucketsWarnsOnly(t *testing.T) {
	arm := Arm{Name: "legacy", Windows: []Window{w(60, 10, 40, 50), w(90, 15, 60, 50), w(120, 20, 80, 50)}}
	v := Evaluate(arm, nil, Default())
	if v.Result != Pass {
		t.Fatalf("want PASS, got %s", v.Result)
	}
	found := false
	for _, n := range v.Notes {
		if strings.Contains(n, "breadth untested") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a breadth-untested note, got %v", v.Notes)
	}
}

// Thin buckets must not be read as concentration. A 2h rule on a slow symbol
// can put three trades in a 30-day bucket, and drop-the-best on three trades
// measures variance, not where the edge lives.
func TestThinBucketsSkipBreadth(t *testing.T) {
	arm := Arm{
		Name:    "low frequency",
		Windows: []Window{w(60, 10, 40, 50), w(90, 15, 60, 50), w(120, 20, 80, 50)},
		// medians: 3 trades — well under the floor of 8
		Buckets: []Window{b(120, -2, 3), b(90, 12, 4), b(60, -1, 2), b(30, 0, 3), b(0, -1, 3)},
	}
	v := Evaluate(arm, nil, Default())
	if v.Result != Pass {
		t.Fatalf("want PASS (breadth untestable on thin buckets), got %s\n%s", v.Result, v)
	}
	if v.Buckets != 0 || v.Concentrated {
		t.Errorf("breadth must be skipped, got Buckets=%d concentrated=%v", v.Buckets, v.Concentrated)
	}
	found := false
	for _, n := range v.Notes {
		if strings.Contains(n, "too thin") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a thin-bucket note, got %v", v.Notes)
	}
}

// collapse must use the MEDIAN, not the mean: one re-sequenced run should not
// drag the figure every criterion is then judged on.
func TestCollapseUsesMedian(t *testing.T) {
	got, sp := collapse([]Window{
		{Days: 60, NetR: 1, Trades: 10}, {Days: 60, NetR: 2, Trades: 10}, {Days: 60, NetR: 100, Trades: 10},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed window, got %d", len(got))
	}
	if got[0].NetR != 2 {
		t.Errorf("got netR %+.2f, want the median +2.00 (the mean would be +34.33)", got[0].NetR)
	}
	s := sp[60]
	if s.Lo != 1 || s.Hi != 100 || s.N != 3 {
		t.Errorf("got span %+v, want {1 100 3}", s)
	}
}

// ETH htf-snr on 2026-10-07: shifting the 60d window's end by one day reads
// +10, -1, +5. The median clears the gate's "netR > 0 in every window" and
// one reading does not, so the PASS is a statement about the run date.
func TestFragileWindowBecomesUndecided(t *testing.T) {
	arm := Arm{Name: "ETH htf-snr", Windows: []Window{
		{Days: 60, NetR: 10, Trades: 89}, {Days: 60, NetR: -1, Trades: 91}, {Days: 60, NetR: 5, Trades: 85},
		{Days: 90, NetR: 11, Trades: 160},
		{Days: 120, NetR: 38, Trades: 220},
	}}
	v := Evaluate(arm, nil, Default())
	if v.Result != Undecided {
		t.Fatalf("want UNDECIDED, got %s\n%s", v.Result, v)
	}
	if !v.Jittered || len(v.Fragile) != 1 {
		t.Errorf("want one fragile window, got jittered=%v fragile=%v", v.Jittered, v.Fragile)
	}
	if !strings.Contains(v.Fragile[0], "60d") {
		t.Errorf("fragility should name the 60d window, got %q", v.Fragile[0])
	}
	// Without the jitter readings the same rule PASSes — that contrast is
	// the reason the flag exists.
	bare := Arm{Name: "bare", Windows: []Window{
		{Days: 60, NetR: 5, Trades: 89}, {Days: 90, NetR: 11, Trades: 160}, {Days: 120, NetR: 38, Trades: 220},
	}}
	if vb := Evaluate(bare, nil, Default()); vb.Result != Pass {
		t.Fatalf("median-only must still PASS, got %s\n%s", vb.Result, vb)
	}
}

// ETH engine over the same shifts: +15.05 / +17.56 / +13.49. Never near zero,
// so the jitter changes nothing.
func TestStableJitterStaysPass(t *testing.T) {
	arm := Arm{Name: "ETH engine", Windows: []Window{
		{Days: 60, NetR: 15.05, Trades: 46}, {Days: 60, NetR: 17.56, Trades: 47}, {Days: 60, NetR: 13.49, Trades: 45},
		{Days: 90, NetR: 15.73, Trades: 72},
		{Days: 120, NetR: 15.47, Trades: 98},
	}}
	v := Evaluate(arm, nil, Default())
	if v.Result != Pass {
		t.Fatalf("want PASS, got %s\n%s", v.Result, v)
	}
	if len(v.Fragile) != 0 {
		t.Errorf("want no fragile windows, got %v", v.Fragile)
	}
}

// The concentration call itself moved once between two runs. When the bucket
// jitter can flip it, it must not decide anything.
//
// medians  -13 +15 +2  +0 +1 → total  +5, best +15, drop -10 → CONCENTRATED
// at Hi    -13 +15 +2 +20 +1 → total +25, best +20, drop  +5 → spread
func TestBreadthFlipWithinJitterIsNotDecided(t *testing.T) {
	arm := Arm{
		Name:    "flippy",
		Windows: []Window{{Days: 60, NetR: 13, Trades: 92}, {Days: 90, NetR: 11, Trades: 160}, {Days: 120, NetR: 38, Trades: 220}},
		Buckets: []Window{
			{Days: 120, NetR: -13, Trades: 43}, {Days: 90, NetR: 15, Trades: 55}, {Days: 60, NetR: 2, Trades: 50},
			{Days: 30, NetR: 0, Trades: 47}, {Days: 30, NetR: 20, Trades: 47}, {Days: 30, NetR: 0, Trades: 47},
			{Days: 0, NetR: 1, Trades: 42},
		},
	}
	v := Evaluate(arm, nil, Default())
	if v.Concentrated {
		t.Errorf("concentration flips inside the jitter and must not stand")
	}
	if v.Result != Pass {
		t.Fatalf("want PASS (breadth undecided, windows fine), got %s\n%s", v.Result, v)
	}
	found := false
	for _, n := range v.Notes {
		if strings.Contains(n, "flips within the bucket jitter") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a flip note, got %v", v.Notes)
	}
}
