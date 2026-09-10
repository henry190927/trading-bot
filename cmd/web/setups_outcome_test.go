package main

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// Both /setups outcome classifiers were untested, which is how classifyOutcome
// shipped with NO fill gate at all: it walked bars asking only whether a CLOSE
// had passed stop or target, so a limit that never filled still got a verdict.
//
// Every expectation below is hand-traced against the actual comparisons in
// each function before being written down — classifyOutcome checks the CLOSE
// against stop then target, classifyRealOutcome checks the bar's LOW/HIGH in
// the same order.
var soT0 = time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)

func soBar(i int, low, high, close float64) market.Candle {
	return market.Candle{
		Low: low, High: high, Close: close,
		CloseTime: soT0.Add(time.Duration(i+1) * time.Hour),
	}
}

// pad repeats a bar so n crosses the 20-bar expiry threshold, without ever
// spanning the entry.
func soPad(from int, n int, low, high, close float64) []market.Candle {
	out := make([]market.Candle, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, soBar(from+i, low, high, close))
	}
	return out
}

// The real id=40 shape: SNDK long resting at 1512 while price left for 1545
// and never came back. classifyOutcome used to call this a WIN.
func TestClassifyOutcomeRefusesVerdictWithoutFill(t *testing.T) {
	su := Setup{Entry: 1512, Stop: 1496, Target: 1541.71, Dir: "long", RecordedAt: soT0}
	// Never spans 1512 (every Low is above it) and closes through the target.
	bars := append([]market.Candle{soBar(0, 1520, 1530, 1525)},
		soPad(1, 25, 1535, 1550, 1545)...)

	oc, _, _, _ := classifyOutcome(su, bars)
	if oc == "win" || oc == "loss" {
		t.Errorf("classifyOutcome = %q on a setup that never filled — this is the id=40 defect", oc)
	}
	if oc != "no-fill" {
		t.Errorf("classifyOutcome = %q, want no-fill", oc)
	}
	roc, _, _, _ := classifyRealOutcome(su, bars)
	if roc != "no-fill" {
		t.Errorf("classifyRealOutcome = %q, want no-fill", roc)
	}
}

// The real id=20 shape: XAG short resting at 63.1398 while price rose to 63.45.
// The close cleared the 63.43 stop, so the old code called it a LOSS.
func TestClassifyOutcomeRefusesShortVerdictWithoutFill(t *testing.T) {
	su := Setup{Entry: 63.139825, Stop: 63.43, Target: 61.97, Dir: "short", RecordedAt: soT0}
	bars := soPad(0, 25, 63.20, 63.50, 63.45) // Low 63.20 never reaches 63.1398

	oc, _, _, _ := classifyOutcome(su, bars)
	if oc == "loss" {
		t.Error("classifyOutcome = loss on a short that never filled — this is the id=20 defect")
	}
	if oc != "no-fill" {
		t.Errorf("classifyOutcome = %q, want no-fill", oc)
	}
}

// A filled setup still resolves normally — the gate must not swallow real ones.
func TestClassifyOutcomeFilledLongWins(t *testing.T) {
	su := Setup{Entry: 100, Stop: 90, Target: 120, Dir: "long", RecordedAt: soT0}
	bars := []market.Candle{
		soBar(0, 98, 102, 101),  // spans 100 → filled; no stop, no target
		soBar(1, 118, 125, 122), // close 122 >= 120 → win
	}
	if oc, _, _, _ := classifyOutcome(su, bars); oc != "win" {
		t.Errorf("classifyOutcome = %q, want win", oc)
	}
	if roc, _, _, _ := classifyRealOutcome(su, bars); roc != "win" {
		t.Errorf("classifyRealOutcome = %q, want win", roc)
	}
}

// The gap the page exists to show, and the ONLY reason the two columns should
// ever disagree: the close reached the target while the wick had already taken
// the stop out first.
func TestClassifyOutcomeGapIsCloseVersusWick(t *testing.T) {
	su := Setup{Entry: 100, Stop: 90, Target: 120, Dir: "long", RecordedAt: soT0}
	bars := []market.Candle{
		soBar(0, 98, 102, 101),
		// close 121 never touches the stop, but Low 88 did.
		soBar(1, 88, 122, 121),
	}
	oc, _, _, _ := classifyOutcome(su, bars)
	roc, _, _, _ := classifyRealOutcome(su, bars)
	if oc != "win" {
		t.Errorf("close-based = %q, want win (close 121 >= target 120, close never <= stop 90)", oc)
	}
	if roc != "loss" {
		t.Errorf("path-based = %q, want loss (Low 88 <= stop 90 first)", roc)
	}
}

// The property that makes the Hit-rate / Real-hit-rate comparison meaningful:
// the two classifiers must agree on WHICH SETUPS COUNT. They may disagree only
// about the verdict on a filled one.
func TestBothClassifiersAgreeOnFill(t *testing.T) {
	cases := []struct {
		name string
		su   Setup
		bars []market.Candle
	}{
		{"long never fills", Setup{Entry: 1512, Stop: 1496, Target: 1541.71, Dir: "long", RecordedAt: soT0},
			soPad(0, 25, 1535, 1550, 1545)},
		{"short never fills", Setup{Entry: 63.139825, Stop: 63.43, Target: 61.97, Dir: "short", RecordedAt: soT0},
			soPad(0, 25, 63.20, 63.50, 63.45)},
		{"filled and resolved", Setup{Entry: 100, Stop: 90, Target: 120, Dir: "long", RecordedAt: soT0},
			[]market.Candle{soBar(0, 98, 102, 101), soBar(1, 118, 125, 122)}},
		{"filled then expires", Setup{Entry: 100, Stop: 90, Target: 200, Dir: "long", RecordedAt: soT0},
			append([]market.Candle{soBar(0, 98, 102, 101)}, soPad(1, 25, 99, 105, 103)...)},
	}
	for _, tc := range cases {
		oc, _, _, _ := classifyOutcome(tc.su, tc.bars)
		roc, _, _, _ := classifyRealOutcome(tc.su, tc.bars)
		if (oc == "no-fill") != (roc == "no-fill") {
			t.Errorf("%s: outcome=%q real=%q — the two disagree about whether it filled, "+
				"which makes the hit-rate gap meaningless", tc.name, oc, roc)
		}
	}
}
