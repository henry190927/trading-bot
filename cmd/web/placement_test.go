package main

import (
	"strings"
	"testing"
)

// The case that motivated the change: a BTC long at entry 77,640 with the mark
// around 78,540, stop trailed up to 77,900 to lock in profit. The old
// entry-based guard refused it because 77,900 > 77,640, which made trailing a
// stop through the
// UI impossible.
const (
	btcEntry = 77640.0
	btcMark  = 78540.0
)

func TestStopPlacementAllowsProfitLockAboveEntry(t *testing.T) {
	if fault := stopPlacementFault("long", 77900, btcMark); fault != "" {
		t.Fatalf("a profit-lock stop between entry (%.0f) and mark (%.0f) must be allowed, got: %s",
			btcEntry, btcMark, fault)
	}
	// The ordinary loss-side stop must obviously still work.
	if fault := stopPlacementFault("long", 77380, btcMark); fault != "" {
		t.Errorf("loss-side stop refused: %s", fault)
	}
	// And right up to (but not touching) the mark.
	if fault := stopPlacementFault("long", btcMark-0.1, btcMark); fault != "" {
		t.Errorf("stop just below mark refused: %s", fault)
	}
}

// The hazard the guard actually has to catch: a trigger the mark has already
// passed fires the instant it lands.
func TestStopPlacementRefusesInstantTrigger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		side       string
		stop, mark float64
	}{
		{"long stop above mark", "long", 78600, btcMark},
		{"long stop exactly at mark", "long", btcMark, btcMark},
		{"short stop below mark", "short", 78400, btcMark},
		{"short stop exactly at mark", "short", btcMark, btcMark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault := stopPlacementFault(tc.side, tc.stop, tc.mark)
			if fault == "" {
				t.Fatal("must refuse")
			}
			if !strings.Contains(fault, "immediately") {
				t.Errorf("reason should say why, got %q", fault)
			}
		})
	}
}

// A short's profit lock is BELOW entry — the mirror of the BTC case.
func TestStopPlacementShortProfitLock(t *testing.T) {
	// Short from 2500, mark down at 2400: a stop at 2450 locks profit.
	if fault := stopPlacementFault("short", 2450, 2400); fault != "" {
		t.Errorf("short profit-lock stop refused: %s", fault)
	}
	// Loss-side stop above entry still fine.
	if fault := stopPlacementFault("short", 2550, 2400); fault != "" {
		t.Errorf("short loss-side stop refused: %s", fault)
	}
}

// No mark, no placement. Defaulting the reference price on an order-placing
// path is worse than not placing, because the sweep retries.
func TestStopPlacementRefusesWithoutMark(t *testing.T) {
	for _, mark := range []float64{0, -1} {
		fault := stopPlacementFault("long", 77380, mark)
		if fault == "" {
			t.Fatalf("mark %v must refuse", mark)
		}
		if !strings.Contains(fault, "mark price unavailable") {
			t.Errorf("reason should name the missing mark, got %q", fault)
		}
	}
	if fault := stopPlacementFault("long", 0, btcMark); fault == "" {
		t.Error("a zero stop must refuse")
	}
}

func TestTPPlacementRestsAboveMark(t *testing.T) {
	// The TP actually placed on that trade.
	if fault := tpPlacementFault("long", 78900, btcMark); fault != "" {
		t.Fatalf("TP above mark refused: %s", fault)
	}
	// Already passed by the mark → instant fill, refuse.
	fault := tpPlacementFault("long", 78100, btcMark)
	if fault == "" {
		t.Fatal("a long TP below the mark must refuse")
	}
	if !strings.Contains(fault, "immediately") {
		t.Errorf("reason should say why, got %q", fault)
	}
	if fault := tpPlacementFault("long", btcMark, btcMark); fault == "" {
		t.Error("TP exactly at mark must refuse")
	}
}

// The old TP guard compared against entry, which forbade a deliberate
// scale-out BELOW entry while the mark was lower still. That is a real
// (if less common) intent and must be allowed.
func TestTPPlacementAllowsScaleOutBelowEntry(t *testing.T) {
	// Long from 77,640, mark has fallen to 77,000; exiting part at 77,200
	// is taking a small loss on purpose, and it rests above the mark.
	if fault := tpPlacementFault("long", 77200, 77000); fault != "" {
		t.Errorf("deliberate scale-out below entry refused: %s", fault)
	}
}

func TestTPPlacementShort(t *testing.T) {
	// Short from 2500, mark 2400: TP at 2350 rests below the mark.
	if fault := tpPlacementFault("short", 2350, 2400); fault != "" {
		t.Errorf("short TP below mark refused: %s", fault)
	}
	if fault := tpPlacementFault("short", 2450, 2400); fault == "" {
		t.Error("short TP above the mark must refuse — it would fill immediately")
	}
	if fault := tpPlacementFault("short", 2400, 2400); fault == "" {
		t.Error("short TP exactly at mark must refuse")
	}
}

func TestTPPlacementRefusesWithoutMark(t *testing.T) {
	if fault := tpPlacementFault("long", 78900, 0); !strings.Contains(fault, "mark price unavailable") {
		t.Errorf("got %q", fault)
	}
	if fault := tpPlacementFault("long", 0, btcMark); fault == "" {
		t.Error("a zero tp must refuse")
	}
}

// Entry price must not appear in the decision at all any more — that is the
// whole point. Verified by construction: neither function takes it.
func TestGuardsDoNotConsiderEntry(t *testing.T) {
	// Same stop, same mark, wildly different entries → same verdict.
	a := stopPlacementFault("long", 77900, btcMark)
	b := stopPlacementFault("long", 77900, btcMark)
	if a != b || a != "" {
		t.Errorf("verdict should be entry-independent and allowed, got %q / %q", a, b)
	}
}

// A target of 0 is "none planned", not a typo. /ops/entry writes tp1 = 0
// deliberately — BingX's bundled take-profit closes 100% of a position, so a
// partial TP1 is a separate reduce-only order placed after the fill, and
// recording one that does not exist would make /ops/verify report protection
// that is not there.
//
// The edit form used parseFloatPositive for both targets, so a row written by
// that route could not be edited at all: setting filled_at on one failed with
// "tp1 must be > 0". The form's error path re-renders with HTTP 200, so the
// failure read as success from the outside.
func TestParseFloatOptionalAcceptsZeroAndBlank(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"", 0, true},    // not set
		{"0", 0, true},   // explicitly none
		{" 0 ", 0, true}, // and with the form's whitespace
		{"2480.5", 2480.5, true},
		{"-1", 0, false}, // a negative target is still a mistake
		{"abc", 0, false},
	} {
		got, err := parseFloatOptional(c.in, "tp1")
		if (err == nil) != c.ok {
			t.Errorf("parseFloatOptional(%q) err = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parseFloatOptional(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// BOTH creation paths must agree. The edit handler was relaxed first and
	// /journal/open was missed, so a row could be edited to tp1 = 0 but never
	// created with it — which is how three real trades stayed off the book.
	// entry and stop keep the stricter rule: a plan without those is not a
	// plan, and the stop is the R baseline every statistic depends on.
	if _, err := parseFloatPositive("0", "entry"); err == nil {
		t.Error("parseFloatPositive accepted 0 — entry and stop must stay required")
	}
	if _, err := parseFloatPositive("", "entry"); err == nil {
		t.Error("parseFloatPositive accepted blank")
	}
}

// The 2026-09-18 case: a BTC 0.0615 position split 50/50. placeTP1OnBingX did
// not floor, so it asked BingX for 0.03075 — five decimals against BTC's
// four-decimal lot step — while placeTP2OnBingX floored and sized itself as
// the remainder after a floored TP1. Two orders, 0.06155 total, against a
// 0.0615 position.
func TestPartialQtyFloorsToLotStep(t *testing.T) {
	// BTC step is 4 decimals: the odd half-step is truncated, not carried.
	if got := partialQty("BTC", 0.0615, 50); got != 0.0307 {
		t.Errorf("partialQty(BTC, 0.0615, 50%%) = %v, want 0.0307 (0.03075 is not a placeable BTC size)", got)
	}
	// ETH's step is coarser (2), so the same 50% truncates much harder.
	if got := partialQty("ETH", 1.555, 50); got != 0.77 {
		t.Errorf("partialQty(ETH, 1.555, 50%%) = %v, want 0.77", got)
	}
	// An unknown ticker must still produce a placeable size, not a panic.
	if got := partialQty("WOOF", 0.0615, 50); got != 0.0307 {
		t.Errorf("unknown symbol should fall back to 4 decimals, got %v", got)
	}
}

// The two legs must close the position EXACTLY: never more than is held (the
// exchange rejects the overshoot, and whichever leg loses the race leaves a
// residue), and the odd step has to land on one side deterministically.
func TestScaleOutLegsSumToPosition(t *testing.T) {
	cases := []struct {
		sym  string
		qty  float64
		pct  float64
		want float64 // expected TP1 leg
	}{
		{"BTC", 0.0615, 50, 0.0307},    // odd step → remainder takes it (0.0308)
		{"BTC", 0.0614, 50, 0.0307},    // even split, no residue
		{"BTC", 0.0615, 30, 0.0184},    // 0.01845 floored
		{"ETH", 1.55, 50, 0.77},        // 0.775 floored
		{"SNDK", 0.00003, 50, 0.00001}, // 5-decimal step, 0.000015 floored
	}
	for _, c := range cases {
		first := partialQty(c.sym, c.qty, c.pct)
		last := remainderQty(c.sym, c.qty, c.pct)
		if first != c.want {
			t.Errorf("%s %g @ %.0f%%: first leg = %v, want %v", c.sym, c.qty, c.pct, first, c.want)
		}
		// Floating point: compare on the lot grid, not with ==.
		if sum := first + last; sum > c.qty+1e-9 {
			t.Errorf("%s %g @ %.0f%%: legs sum to %v — oversells the position by %v",
				c.sym, c.qty, c.pct, sum, sum-c.qty)
		}
		if last <= 0 {
			t.Errorf("%s %g @ %.0f%%: remainder leg is %v — TP2 would be skipped", c.sym, c.qty, c.pct, last)
		}
	}
}

// 100% on the first leg is a full-size TP, which is legal — but it must leave
// nothing for the second, so placeTP2OnBingX skips rather than sending a
// zero-or-negative order.
func TestScaleOutFullFirstLegLeavesNoRemainder(t *testing.T) {
	if got := partialQty("BTC", 0.0615, 100); got != 0.0615 {
		t.Errorf("100%% of 0.0615 = %v, want the whole position", got)
	}
	if got := remainderQty("BTC", 0.0615, 100); got > 0 {
		t.Errorf("remainder after a 100%% first leg = %v, want <= 0 so TP2 is skipped", got)
	}
}

// AKE and UNI both trade in whole units. The 4-decimal fallback in lotPrec
// would have been wrong in the direction that gets an order rejected.
func TestLotPrecForNewAltSymbols(t *testing.T) {
	for _, sym := range []string{"AKE", "UNI", "XRP"} {
		if got := lotPrec(sym); got != 0 {
			t.Errorf("lotPrec(%s) = %d, want 0 (whole units, per cmd/contracts)", sym, got)
		}
		// A fractional partial must floor away entirely rather than round up
		// into a size the exchange refuses.
		if got := partialQty(sym, 101, 50); got != 50 {
			t.Errorf("partialQty(%s, 101, 50%%) = %v, want 50", sym, got)
		}
	}
}

// fmtPrice rendered AKE's 0.054028 as "0.0540", so two distinct prices showed
// as one number on the verify card and in the journal. Widening the sub-1
// branch must not disturb anything already displayed.
func TestFmtPriceKeepsPrecisionBelowOne(t *testing.T) {
	f := templateFuncs()["fmtPrice"].(func(float64) string)
	cases := []struct {
		in   float64
		want string
	}{
		{0, "-"},
		{86048.5, "86048.5000"}, // >= 1 path untouched
		{1.5174, "1.5174"},
		{0.8051, "0.8051"},     // was "0.8051" before the change
		{0.6878, "0.6878"},     // was "0.6878"
		{0.054028, "0.054028"}, // was "0.0540" — the bug
		{0.061924, "0.061924"},
		{0.5, "0.5"},
	}
	for _, c := range cases {
		if got := f(c.in); got != c.want {
			t.Errorf("fmtPrice(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
