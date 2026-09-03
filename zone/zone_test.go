package zone

import (
	"strings"
	"testing"
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
