package signal

import (
	"math"
	"testing"

	"myFirstGo/trading-bot/market"
)

// mkC builds a candle from high/low/close (open/time irrelevant to structure).
func mkC(high, low, close float64) market.Candle {
	return market.Candle{High: high, Low: low, Close: close, Open: (high + low) / 2}
}

// upStructure builds a series with a single confirmed swing low at idx7
// (L=93) and swing high at idx10 (H=112) under a fractal strength of 2.
// idx11-12 stay below the high; the caller supplies the terminal bar.
func upStructure(lastHigh, lastLow, lastClose float64) []market.Candle {
	cs := []market.Candle{
		mkC(105, 100, 102), // 0
		mkC(104, 99, 101),  // 1
		mkC(103, 98, 100),  // 2
		mkC(102, 97, 99),   // 3
		mkC(101, 96, 98),   // 4
		mkC(100, 95, 97),   // 5
		mkC(99, 94, 96),    // 6
		mkC(98, 93, 95),    // 7  <- swing low (L=93)
		mkC(101, 95, 100),  // 8
		mkC(104, 97, 103),  // 9
		mkC(112, 100, 110), // 10 <- swing high (H=112)
		mkC(108, 99, 105),  // 11
		mkC(107, 100, 104), // 12
		mkC(lastHigh, lastLow, lastClose), // 13 terminal bar
	}
	return cs
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestAnalyzeStructure_BOSUp(t *testing.T) {
	// Terminal bar closes at 114 > swing high 112 -> BOS-up.
	st := AnalyzeStructure(upStructure(115, 106, 114), 2)

	if st.Event != EvBOSUp {
		t.Fatalf("event = %v, want BOS-up", st.Event)
	}
	if !approx(st.BOSLevel, 112) || !approx(st.Protected, 93) {
		t.Errorf("levels: BOSLevel=%.4f Protected=%.4f, want 112 / 93", st.BOSLevel, st.Protected)
	}
	if st.Zone == nil {
		t.Fatal("expected a 樞紐區 for a BOS-up leg, got nil")
	}
	// Leg [93,112], span 19: Hi = 112-0.5*19 = 102.5, Lo = 112-0.705*19 = 98.605.
	if !approx(st.Zone.Hi, 102.5) || !approx(st.Zone.Lo, 98.605) {
		t.Errorf("zone band = [%.4f, %.4f], want [98.605, 102.5]", st.Zone.Lo, st.Zone.Hi)
	}
	if st.Zone.Dir != StructUptrend {
		t.Errorf("zone dir = %v, want uptrend", st.Zone.Dir)
	}
	if !approx(st.Zone.Invalidate, 93) {
		t.Errorf("invalidate = %.4f, want 93 (leg origin)", st.Zone.Invalidate)
	}
	if !approx(st.Zone.Target, 131) { // 112 + 19
		t.Errorf("target = %.4f, want 131 (1:1 measured move)", st.Zone.Target)
	}
	// Price broke out ABOVE the zone -> not yet in the pullback zone.
	if st.InZone {
		t.Errorf("InZone = true at breakout, want false (price above the 樞紐區)")
	}
}

func TestAnalyzeStructure_CHoCHDown(t *testing.T) {
	// Same up-structure, but the terminal bar closes at 90 < protected low 93
	// -> the up-leg's origin fails -> CHoCH-down, and the up-zone is voided.
	st := AnalyzeStructure(upStructure(96, 89, 90), 2)

	if st.Event != EvCHoCHDown {
		t.Fatalf("event = %v, want CHoCH-down", st.Event)
	}
	if st.Zone != nil {
		t.Errorf("expected zone voided on CHoCH, got %+v", st.Zone)
	}
}

func TestAnalyzeStructure_NoEventInsideRange(t *testing.T) {
	// Terminal bar closes at 104 — between protected low (93) and BOS
	// level (112): no break, persistent up-leg 樞紐區 still reported.
	st := AnalyzeStructure(upStructure(107, 100, 104), 2)
	if st.Event != EvNone {
		t.Fatalf("event = %v, want none", st.Event)
	}
	if st.Zone == nil {
		t.Fatal("expected persistent 樞紐區 for the standing up-leg, got nil")
	}
}
