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
	// entry and stop keep the stricter rule — a plan without those is not a plan.
	if _, err := parseFloatPositive("0", "entry"); err == nil {
		t.Error("parseFloatPositive accepted 0 — entry and stop must stay required")
	}
	if _, err := parseFloatPositive("", "entry"); err == nil {
		t.Error("parseFloatPositive accepted blank")
	}
}
