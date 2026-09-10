package zone

import (
	"strings"
	"testing"

	"myFirstGo/trading-bot/signal"
)

// The band that caused this feature: BTC 77390–77476.3 (EQL 3x + 日開 77396.9
// + EQH 3x), armed 2026-09-02. The 21:00 UTC+8 bar printed H 77413.4 and
// closed 77132 — a touch alert called that "gate open".
const (
	btcLo = 77390.0
	btcHi = 77476.3
)

func band(mode string) Zone {
	return Zone{Symbol: "BTC", Lo: btcLo, Hi: btcHi, Dir: "watch", TF: "1h", Confirm: mode}
}

func TestEvalConfirm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		closePx   float64
		want      bool
		wantKnown bool
	}{
		// The real bar. Every mode except close-below must stay silent, and
		// close-below must NOT fire either — 77132 is under Lo, so it would.
		// Traced: 77132 < 77390 → true. That is correct for a break-DOWN gate
		// and is why the two directions are separate modes, not one "close".
		{"the wick bar does not confirm close-in", ConfirmCloseIn, 77132, false, true},
		{"the wick bar does not confirm close-above", ConfirmCloseAbove, 77132, false, true},
		{"the wick bar DOES confirm close-below", ConfirmCloseBelow, 77132, true, true},

		// close-in: inclusive on both edges (a close exactly on the level is
		// inside the band, matching how the band was drawn from pool Lo/Hi).
		{"close-in at Lo edge", ConfirmCloseIn, btcLo, true, true},
		{"close-in at Hi edge", ConfirmCloseIn, btcHi, true, true},
		{"close-in mid band", ConfirmCloseIn, 77420, true, true},
		{"close-in just under Lo", ConfirmCloseIn, 77389.9, false, true},
		{"close-in just over Hi", ConfirmCloseIn, 77476.4, false, true},

		// close-above is STRICT: closing exactly on 77476.3 has not cleared
		// the cluster, it is sitting on it.
		{"close-above exactly on Hi is not a break", ConfirmCloseAbove, btcHi, false, true},
		{"close-above one tick over Hi", ConfirmCloseAbove, 77476.4, true, true},
		{"close-above well clear", ConfirmCloseAbove, 77600, true, true},
		{"close-above inside the band is not a break", ConfirmCloseAbove, 77420, false, true},

		// close-below, same strictness at the other edge.
		{"close-below exactly on Lo is not a break", ConfirmCloseBelow, btcLo, false, true},
		{"close-below one tick under Lo", ConfirmCloseBelow, 77389.9, true, true},

		// Unknown / touch must report known=false so the caller skips rather
		// than silently degrading to a wick alert.
		{"touch mode is not close-evaluable", ConfirmTouch, 77420, false, false},
		{"typo mode is not close-evaluable", "close_above", 77600, false, false},
		{"garbage mode is not close-evaluable", "yes", 77600, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := EvalConfirm(band(tc.mode), tc.closePx)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("EvalConfirm(%q, %.1f) = (%v,%v), want (%v,%v)",
					tc.mode, tc.closePx, got, known, tc.want, tc.wantKnown)
			}
		})
	}
}

// Case-insensitive and whitespace-tolerant, because these come from a
// hand-edited zones.json.
func TestEvalConfirmSloppyInput(t *testing.T) {
	for _, m := range []string{"CLOSE-ABOVE", " close-above ", "Close-Above"} {
		if got, known := EvalConfirm(band(m), 77600); !got || !known {
			t.Errorf("mode %q should be recognised, got (%v,%v)", m, got, known)
		}
	}
}

func TestNeedsClose(t *testing.T) {
	if band(ConfirmTouch).NeedsClose() {
		t.Error("default (empty) must stay a live-touch zone — auto pivot zones rely on it")
	}
	if band("   ").NeedsClose() {
		t.Error("whitespace-only must read as touch, not as an unknown close mode")
	}
	for _, m := range []string{ConfirmCloseIn, ConfirmCloseAbove, ConfirmCloseBelow, "close_above"} {
		if !band(m).NeedsClose() {
			t.Errorf("%q must require a closed bar (unknown modes included, so they get skipped not touched)", m)
		}
	}
}

// Changing the mode must re-arm the debounce, otherwise a band that was
// already "inside" under touch mode would never fire its first close.
func TestKeyIncludesConfirm(t *testing.T) {
	touch := Key(band(ConfirmTouch))
	above := Key(band(ConfirmCloseAbove))
	in := Key(band(ConfirmCloseIn))
	if touch == above || above == in || touch == in {
		t.Errorf("keys must differ per mode: touch=%q above=%q in=%q", touch, above, in)
	}
	if Key(band(ConfirmCloseAbove)) != Key(band(" CLOSE-ABOVE ")) {
		t.Error("the same mode written sloppily must map to the same key, or the zone re-arms on every reformat")
	}
}

func TestConfirmWord(t *testing.T) {
	for mode, want := range map[string]string{
		ConfirmTouch:      "即時觸價",
		ConfirmCloseIn:    "收在帶內",
		ConfirmCloseAbove: "收破上緣",
		ConfirmCloseBelow: "收破下緣",
		"nonsense":        "未知模式",
	} {
		if got := ConfirmWord(mode); got != want {
			t.Errorf("ConfirmWord(%q) = %q, want %q", mode, got, want)
		}
	}
}

// TriggerFault is the single gate the daemon and /ops share. Every reason a
// zone can silently do nothing has to surface here, or the armed list lies.
func TestTriggerFault(t *testing.T) {
	ok := Zone{Symbol: "BTC", Lo: 77390, Hi: 77476.3, Dir: "watch", TF: "1h", Confirm: ConfirmCloseAbove}

	t.Run("a well-formed zone has no fault", func(t *testing.T) {
		if f := TriggerFault(ok); f != "" {
			t.Errorf("want no fault, got %q", f)
		}
		touch := ok
		touch.Confirm, touch.TF = ConfirmTouch, ""
		if f := TriggerFault(touch); f != "" {
			t.Errorf("a touch zone needs no tf, got %q", f)
		}
	})

	// The bug this closed: zones.json could name SNDK/NVDA, the daemon's map
	// lookup failed, it `continue`d, and /ops still showed the row as armed.
	t.Run("the US-stock synthetics resolve", func(t *testing.T) {
		for _, sym := range []string{"SNDK", "NVDA", "nvda", " SNDK "} {
			z := ok
			z.Symbol, z.Lo, z.Hi = sym, 226.89, 227.1
			if f := TriggerFault(z); f != "" {
				t.Errorf("%q should resolve, got fault %q", sym, f)
			}
		}
	})

	t.Run("an unresolvable symbol is a fault, not a silent skip", func(t *testing.T) {
		z := ok
		z.Symbol = "DOGE"
		if f := TriggerFault(z); f == "" {
			t.Fatal("unknown symbol must fault")
		} else if !strings.Contains(f, "DOGE") {
			t.Errorf("fault should name the symbol, got %q", f)
		}
	})

	t.Run("an inverted or degenerate band is a fault", func(t *testing.T) {
		for _, tc := range [][2]float64{{77476.3, 77390}, {77390, 77390}} {
			z := ok
			z.Lo, z.Hi = tc[0], tc[1]
			if f := TriggerFault(z); f == "" {
				t.Errorf("lo=%v hi=%v must fault", tc[0], tc[1])
			}
		}
	})

	t.Run("confirm without tf is a fault", func(t *testing.T) {
		z := ok
		z.TF = "  "
		if f := TriggerFault(z); !strings.Contains(f, "tf") {
			t.Errorf("want a tf fault, got %q", f)
		}
	})

	t.Run("an unknown confirm mode is a fault", func(t *testing.T) {
		z := ok
		z.Confirm = "close_above"
		if f := TriggerFault(z); !strings.Contains(f, "close_above") {
			t.Errorf("fault should quote the bad mode, got %q", f)
		}
	})

	// Ordering matters: a zone that is wrong in two ways must report the
	// symbol first, because that is what makes the row unfixable.
	t.Run("symbol fault outranks a confirm fault", func(t *testing.T) {
		z := ok
		z.Symbol, z.Confirm = "DOGE", "nonsense"
		if f := TriggerFault(z); !strings.Contains(f, "symbol") {
			t.Errorf("want the symbol fault, got %q", f)
		}
	})
}

// SymToShort must round-trip everything ShortToSym accepts, otherwise
// ComputeAuto-derived zones for a symbol would carry an empty Symbol field.
func TestSymbolMapsRoundTrip(t *testing.T) {
	for short, sym := range ShortToSym {
		if back := SymToShort[sym]; back != short {
			t.Errorf("%s → %s → %q, want %s", short, sym, back, short)
		}
	}
	if len(SymToShort) != len(ShortToSym) {
		t.Errorf("map sizes differ: %d vs %d", len(SymToShort), len(ShortToSym))
	}
}

// Every symbol on the traded roster must be armable as a MANUAL zone.
//
// This map has now gone stale against the roster twice: SNDK/NVDA bands sat in
// zones.json unresolvable until 2026-09-02, and SUI's band sat the same way
// until 2026-09-08. Both times the zone channel — the only thing that pages a
// manual setup — was dead for a symbol being actively watched, and nothing
// failed until someone armed the row by hand. Keep this list in step with the
// roster and the next omission fails here instead.
func TestRosterSymbolsArmable(t *testing.T) {
	roster := []string{"BTC", "ETH", "XAG", "SUI", "SNDK"}
	for _, short := range roster {
		z := Zone{Symbol: short, Lo: 1, Hi: 2, Dir: "watch", TF: "1h", Confirm: ConfirmCloseIn}
		if f := TriggerFault(z); f != "" {
			t.Errorf("roster symbol %s is not armable: %s", short, f)
		}
	}
}

// ---------------------------------------------------------------------
// AutoZoneFrom — the arming rules for the generated half of the channel.
// ---------------------------------------------------------------------

func upZone() *signal.PivotZone {
	return &signal.PivotZone{
		Dir: signal.StructUptrend, Lo: 78864.4, Hi: 79853.1,
		Invalidate: 77441.6, Target: 87087.8, LegLow: 77441.6, LegHigh: 82264.7,
	}
}

// The bug this asserts against: ComputeAuto built its Zone without setting
// Confirm, so every generated zone inherited ConfirmTouch (""). On a band as
// wide as BTC 2h (78,864.4–79,853.1 ≈ 1,000 points) touch fires at the far
// edge, ~a full band-width early, and cmd/confirmbt priced that difference at
// -0.41R/trade vs -0.12R/trade. Auto zones must match the hand-armed ones.
func TestAutoZoneFromUsesCloseConfirmation(t *testing.T) {
	z, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{
		Trend: signal.StructUptrend, Zone: upZone(),
	})
	if !ok {
		t.Fatal("a clean uptrend with an active pivot zone must arm")
	}
	if z.Confirm != ConfirmCloseIn {
		t.Errorf("Confirm = %q, want %q — touch semantics fire a band-width early",
			z.Confirm, ConfirmCloseIn)
	}
	if !z.NeedsClose() {
		t.Error("NeedsClose() must be true, or zonealert takes the live-price path")
	}
}

// downZone mirrors upZone through buildPivotZone's down-leg branch:
// span 4823.1, Lo = legLow+0.5*span, Hi = legLow+0.705*span,
// Invalidate = legHigh, Target = legLow-span.
func downZone() *signal.PivotZone {
	return &signal.PivotZone{
		Dir: signal.StructDowntrend, Lo: 79853.15, Hi: 80841.8855,
		Invalidate: 82264.7, Target: 72618.5, LegLow: 77441.6, LegHigh: 82264.7,
	}
}

func TestAutoZoneFromDirectionFollowsTrend(t *testing.T) {
	for _, tc := range []struct {
		trend signal.TrendStructure
		zone  *signal.PivotZone
		want  string
	}{
		{signal.StructUptrend, upZone(), "long"},
		{signal.StructDowntrend, downZone(), "short"},
	} {
		z, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{Trend: tc.trend, Zone: tc.zone})
		if !ok {
			t.Fatalf("%v should arm", tc.trend)
		}
		if z.Dir != tc.want {
			t.Errorf("%v → dir %q, want %q (fade WITH the trend, never against)", tc.trend, z.Dir, tc.want)
		}
	}
}

// The bug: Dir came from Trend (the 3-swing HH-HL/LH-LL context) while every
// price in the note came from st.Zone (the CURRENT LEG). When the latest leg
// has already turned against a still-unflipped classification those disagree,
// and on 2026-09-10 XAG shipped two ARMED zones reading dir="long" with
// stop 68.37 above the band and target 66.05 below it — a short's bracket
// behind the 🟢 LONG push zonealert.go renders straight off Dir. All eight
// (symbol, TF) combinations disagreed that morning.
//
// Refusing is deliberate: relabelling from Zone.Dir would keep emitting, but
// it would turn a channel documented to "fade WITH the trend, never against"
// into an auto-armed counter-trend fader — a strategy change wearing a
// bug-fix's clothes.
func TestAutoZoneFromRefusesTrendZoneDisagreement(t *testing.T) {
	if _, ok := AutoZoneFrom("XAG", "2h", signal.StructureState{
		Trend: signal.StructUptrend, Zone: downZone(),
	}); ok {
		t.Error("HH-HL uptrend with a down-leg pivot zone must not arm — that is the XAG 2026-09-10 case")
	}
	if _, ok := AutoZoneFrom("XAG", "2h", signal.StructureState{
		Trend: signal.StructDowntrend, Zone: upZone(),
	}); ok {
		t.Error("LH-LL downtrend with an up-leg pivot zone must not arm")
	}
}

// Whatever is emitted, the label and the bracket must describe the SAME trade:
// a long stops below the band and targets above it, a short the reverse. This
// is the invariant the direction bug violated, and it is checkable without
// knowing anything about the market.
func TestAutoZoneBracketMatchesDirection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trend signal.TrendStructure
		zone  *signal.PivotZone
	}{
		{"long", signal.StructUptrend, upZone()},
		{"short", signal.StructDowntrend, downZone()},
	} {
		z, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{Trend: tc.trend, Zone: tc.zone})
		if !ok {
			t.Fatalf("%s should arm", tc.name)
		}
		inv, tgt := tc.zone.Invalidate, tc.zone.Target
		switch z.Dir {
		case "long":
			if !(inv < z.Lo && tgt > z.Hi) {
				t.Errorf("long: want stop %.2f below band [%.2f,%.2f] and target %.2f above it", inv, z.Lo, z.Hi, tgt)
			}
		case "short":
			if !(inv > z.Hi && tgt < z.Lo) {
				t.Errorf("short: want stop %.2f above band [%.2f,%.2f] and target %.2f below it", inv, z.Lo, z.Hi, tgt)
			}
		default:
			t.Fatalf("unexpected dir %q", z.Dir)
		}
	}
}

// Two distinct skips, both of which would otherwise arm a counter-trend fade
// with no structural backing.
func TestAutoZoneFromSkips(t *testing.T) {
	if _, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{
		Trend: signal.StructUptrend, Zone: nil,
	}); ok {
		t.Error("no active 樞紐區 (leg invalidated by CHoCH) must not arm")
	}
	if _, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{
		Trend: signal.StructNeutral, Zone: upZone(),
	}); ok {
		t.Error("a neutral trend must not arm — BTC 1h was exactly this case on 2026-09-04")
	}
}

// Lo/Hi must come out ordered whichever way the pivot zone stored them: the
// zonealert band test is `px >= Lo && px <= Hi`, which is empty if they swap.
func TestAutoZoneFromNormalisesBandOrder(t *testing.T) {
	inverted := upZone()
	inverted.Lo, inverted.Hi = inverted.Hi, inverted.Lo
	z, ok := AutoZoneFrom("BTC", "2h", signal.StructureState{Trend: signal.StructUptrend, Zone: inverted})
	if !ok {
		t.Fatal("should arm")
	}
	if z.Lo > z.Hi {
		t.Errorf("band not normalised: Lo %.1f > Hi %.1f", z.Lo, z.Hi)
	}
	if z.Lo != 78864.4 || z.Hi != 79853.1 {
		t.Errorf("band = %.1f–%.1f, want 78864.4–79853.1", z.Lo, z.Hi)
	}
}
