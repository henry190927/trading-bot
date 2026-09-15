package risk

import (
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

// The three unplanned trades of the liquidation this package exists to
// prevent, with equity as it stood at each point. These are the inputs the
// system had and did not use.
var (
	equityBeforeA  = 220.0 // after the preceding trade stopped out
	equityBeforeBC = 140.0 // after A closed at a loss
	notionalA      = 8200.0
	notionalB      = 8800.0
	notionalC      = 7700.0
)

func flat(equity float64) Exposure {
	return Exposure{Equity: equity, EquityKnown: true, UsedMargin: 0, UsedMarginKnown: true}
}

// The whole point of the package: replay the sequence that took the account to
// zero and confirm a leverage cap stops it where the margin cap did not.
func TestFatalSequenceIsBlocked(t *testing.T) {
	l := Limits{MaxAccountLev: 40}

	// A opens against a flat book: 37.8x, under the cap. Allowed — and it
	// should be, because A alone did not kill the account.
	v := Check(l, flat(equityBeforeA), notionalA)
	if v.Blocked {
		t.Errorf("A alone was blocked: %s", v.Reason)
	}
	if !near(v.LevAfter, 37.2727) {
		t.Errorf("A LevAfter = %.4f, want 37.2727", v.LevAfter)
	}
	if !near(v.KillDistancePct, 2.6829) {
		t.Errorf("A kill distance = %.4f%%, want 2.6829%%", v.KillDistancePct)
	}

	// B opens while A is still on: 77.3x. This is the order that had to be
	// refused, and no margin-based cap could see it — margin was 136.33u
	// against a 140u ceiling.
	withA := flat(equityBeforeA)
	withA.OpenNotional, withA.OpenCount = notionalA, 1
	withA.Legs = []string{"ETH short 8,200u"}
	v = Check(l, withA, notionalB)
	if !v.Blocked {
		t.Fatalf("A+B was ALLOWED at %.1fx — this is the order the cap exists to refuse", v.LevAfter)
	}
	if !v.Enforced {
		t.Error("verdict not marked Enforced despite full inputs")
	}
	if !near(v.LevAfter, 77.2727) {
		t.Errorf("A+B LevAfter = %.4f, want 77.2727", v.LevAfter)
	}
	for _, want := range []string{"max-account-leverage", "77.3x", "cap 40.0x", "ETH short 8,200u"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q does not contain %q", v.Reason, want)
		}
	}

	// B+C, the pair actually held to zero: 117.9x, 0.848% kill distance.
	withB := flat(equityBeforeBC)
	withB.OpenNotional, withB.OpenCount = notionalB, 1
	v = Check(l, withB, notionalC)
	if !v.Blocked {
		t.Fatal("B+C was allowed")
	}
	if !near(v.LevAfter, 117.8571) {
		t.Errorf("B+C LevAfter = %.4f, want 117.8571", v.LevAfter)
	}
	if !near(v.KillDistancePct, 0.8485) {
		t.Errorf("B+C kill distance = %.4f%%, want 0.8485%%", v.KillDistancePct)
	}
}

// A pair kept on deliberately at 55.1x after the arithmetic was laid out. A
// cap set at 60 must allow it: this package does not overrule a decision that
// was made on purpose.
func TestAcceptedPairIsNotOverruled(t *testing.T) {
	e := flat(340)
	e.OpenNotional, e.OpenCount = 9375, 1
	v := Check(Limits{MaxAccountLev: 60}, e, 9375)
	if v.Blocked {
		t.Errorf("55.8x was blocked under a 60x cap: %s", v.Reason)
	}
	if !near(v.LevAfter, 55.1471) {
		t.Errorf("LevAfter = %.4f, want 55.1471", v.LevAfter)
	}
	if !near(v.KillDistancePct, 1.8133) {
		t.Errorf("kill distance = %.4f%%, want 1.8133%%", v.KillDistancePct)
	}
}

// Zero means unlimited, everywhere. An .env written before these variables
// existed must not freeze the order path — autotrade's caps shipped with
// exactly this bug once.
func TestZeroMeansUnlimited(t *testing.T) {
	e := flat(140)
	e.OpenNotional, e.OpenCount = 16500, 2
	v := Check(Limits{}, e, 50000)
	if v.Blocked {
		t.Fatalf("an empty Limits blocked an order: %s", v.Reason)
	}
	if v.Enforced {
		t.Error("Enforced is true with no limits configured")
	}
	if len(v.Warnings) == 0 || !strings.Contains(v.Warnings[0], "no risk limits configured") {
		t.Errorf("warnings = %v, want a note that nothing is configured", v.Warnings)
	}
	if !(Limits{}).Enabled() {
		return
	}
	t.Error("empty Limits reported Enabled()")
}

func TestEnabled(t *testing.T) {
	for _, l := range []Limits{
		{MaxAccountLev: 40}, {WarnAccountLev: 30},
		{MaxNotionalUSDT: 10000}, {MaxConcurrent: 2},
	} {
		if !l.Enabled() {
			t.Errorf("%+v reported not Enabled", l)
		}
	}
}

// Failure posture: unusable inputs ALLOW and warn. Blocking on a transient
// balance read would stop a correctly-sized trade, and a gate that fires on
// its own plumbing gets switched off.
func TestUnusableInputsAllowLoudly(t *testing.T) {
	l := Limits{MaxAccountLev: 40}
	cases := []struct {
		name      string
		e         Exposure
		wantInMsg string
	}{
		{
			name:      "equity unreadable",
			e:         Exposure{EquityKnown: false},
			wantInMsg: "could not be read",
		},
		{
			name:      "equity is a real zero",
			e:         Exposure{Equity: 0, EquityKnown: true},
			wantInMsg: "insufficient margin",
		},
		{
			// The endpoint that lists positions was only verified against a
			// flat account, so used margin is the independent witness.
			name: "used margin contradicts an empty positions list",
			e: Exposure{
				Equity: 200, EquityKnown: true,
				UsedMargin: 131.71, UsedMarginKnown: true,
				OpenNotional: 0, OpenCount: 0,
			},
			wantInMsg: "understated",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Check(l, c.e, 9375)
			if v.Blocked {
				t.Errorf("blocked on unusable input: %s", v.Reason)
			}
			if v.Enforced {
				t.Error("Enforced is true on unusable input — the caller would believe it was checked")
			}
			joined := strings.Join(v.Warnings, " | ")
			if !strings.Contains(joined, c.wantInMsg) {
				t.Errorf("warnings %q do not contain %q", joined, c.wantInMsg)
			}
		})
	}
}

// Concurrency is the one limit that still binds without equity, so it must be
// checked before the equity gate returns early.
func TestConcurrencyBindsWithoutEquity(t *testing.T) {
	e := Exposure{EquityKnown: false, OpenCount: 2, Legs: []string{"BTC long", "ETH long"}}
	v := Check(Limits{MaxAccountLev: 40, MaxConcurrent: 2}, e, 9375)
	if !v.Blocked {
		t.Fatal("max-concurrent did not bind with equity unknown")
	}
	if !v.Enforced {
		t.Error("Enforced false on a concurrency block that was genuinely evaluated")
	}
	if !strings.Contains(v.Reason, "max-concurrent") {
		t.Errorf("reason %q", v.Reason)
	}
	if !strings.Contains(v.Reason, "BTC long") || !strings.Contains(v.Reason, "ETH long") {
		t.Errorf("reason %q does not name what is already open", v.Reason)
	}
}

func TestMaxNotionalBindsWithoutEquity(t *testing.T) {
	e := Exposure{EquityKnown: false, OpenNotional: 8000, OpenCount: 1}
	v := Check(Limits{MaxNotionalUSDT: 15000}, e, 9375)
	if !v.Blocked {
		t.Fatal("max-notional did not bind")
	}
	if !strings.Contains(v.Reason, "17375u > cap 15000u") {
		t.Errorf("reason %q does not show the arithmetic", v.Reason)
	}
}

// Warn-only is how this ships: advisory numbers with nothing refused, so the
// figure becomes visible before anyone has to choose a hard ceiling.
func TestWarnOnlyNeverBlocks(t *testing.T) {
	e := flat(140)
	e.OpenNotional, e.OpenCount = 8800, 1
	v := Check(Limits{WarnAccountLev: 30}, e, 7700)
	if v.Blocked {
		t.Fatalf("warn-only mode blocked: %s", v.Reason)
	}
	if !v.Enforced {
		t.Error("Enforced false despite complete inputs")
	}
	if !near(v.LevAfter, 117.8571) {
		t.Errorf("LevAfter = %.4f, want 117.8571", v.LevAfter)
	}
	joined := strings.Join(v.Warnings, " | ")
	for _, want := range []string{"117.9x", "0.848%", "30.0x"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q do not contain %q", joined, want)
		}
	}
}

// Below the warn threshold there should be no noise at all — a gate that warns
// on every order teaches you to ignore it.
func TestQuietWhenWithinLimits(t *testing.T) {
	v := Check(Limits{MaxAccountLev: 60, WarnAccountLev: 40}, flat(317), 9375)
	if v.Blocked {
		t.Fatalf("blocked: %s", v.Reason)
	}
	if len(v.Warnings) != 0 {
		t.Errorf("warnings = %v, want none at 29.6x under a 40x warn threshold", v.Warnings)
	}
	if !near(v.LevAfter, 29.5741) {
		t.Errorf("LevAfter = %.4f, want 29.5741", v.LevAfter)
	}
	if !near(v.KillDistancePct, 3.3813) {
		t.Errorf("kill distance = %.4f%%, want 3.3813%%", v.KillDistancePct)
	}
}

// The daily-loss halt outranks sizing in autotrade; here the ordering that
// matters is that a hard block always beats a warning, so a blocked verdict
// never carries a "would have been" warning that reads like approval.
func TestBlockedVerdictHasNoApprovalNoise(t *testing.T) {
	e := flat(140)
	e.OpenNotional, e.OpenCount = 8800, 1
	v := Check(Limits{MaxAccountLev: 40, WarnAccountLev: 30}, e, 7700)
	if !v.Blocked {
		t.Fatal("not blocked")
	}
	if len(v.Warnings) != 0 {
		t.Errorf("a blocked verdict also carried warnings %v", v.Warnings)
	}
}
