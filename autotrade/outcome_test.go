package autotrade

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

func TestEvaluateFireLive(t *testing.T) {
	fireT := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	b := func(i int, lo, hi float64) market.Candle { return bar(fireT.Add(time.Duration(i)*time.Hour), lo, hi) }
	longOpen := PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110}
	longOpenCS := []market.Candle{b(0, 99, 101)} // fills bar0, last close 100, open on closed bars

	cases := []struct {
		name       string
		f          PaperFire
		cs         []market.Candle
		live       float64
		wantStatus OutcomeStatus
		wantR      float64 // netR for resolved, UnrealR for open
	}{
		{"open marks unreal to live", longOpen, longOpenCS, 105, OutOpen, 1.0},
		{"live past tp resolves tp", longOpen, longOpenCS, 111, OutTP, 2.0},
		{"live past stop resolves stop", longOpen, longOpenCS, 94, OutStop, -1.0},
		{
			// resting short limit above spot; live rallies to entry → fills → open
			name: "pending fills when live reaches entry", live: 2484,
			f:          PaperFire{Time: fireT, Side: "short", Entry: 2483, Stop: 2521, TP: 2446},
			cs:         []market.Candle{b(0, 2460, 2472)},
			wantStatus: OutOpen, wantR: (2483.0 - 2484.0) / (2521.0 - 2483.0),
		},
		{
			// closed-bar already stopped; a live spike above tp must NOT un-resolve it
			name: "resolved stop stays stop despite live spike", live: 120,
			f:          longOpen,
			cs:         []market.Candle{b(0, 100, 100), b(1, 94, 101)},
			wantStatus: OutStop, wantR: -1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateFireLive(tc.f, tc.cs, 6, tc.live)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			gotR := got.NetR
			if tc.wantStatus == OutOpen {
				gotR = got.UnrealR
			}
			if d := gotR - tc.wantR; d > 1e-9 || d < -1e-9 {
				t.Errorf("R = %v, want %v", gotR, tc.wantR)
			}
		})
	}
}

func bar(open time.Time, lo, hi float64) market.Candle {
	return market.Candle{OpenTime: open, CloseTime: open.Add(time.Hour), Low: lo, High: hi, Close: (lo + hi) / 2}
}

func TestEvaluateFire(t *testing.T) {
	fireT := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	// bars all close strictly after fireT
	b := func(i int, lo, hi float64) market.Candle { return bar(fireT.Add(time.Duration(i)*time.Hour), lo, hi) }

	cases := []struct {
		name       string
		f          PaperFire
		cs         []market.Candle
		wantStatus OutcomeStatus
		wantR      float64
		wantFill   int
		wantHeld   int
	}{
		{
			name:       "long tp 2R",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110},
			cs:         []market.Candle{b(0, 99, 101), b(1, 98, 112)},
			wantStatus: OutTP, wantR: 2.0, wantFill: 1, wantHeld: 1,
		},
		{
			name:       "long stop -1R",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110},
			cs:         []market.Candle{b(0, 100, 100), b(1, 94, 101)},
			wantStatus: OutStop, wantR: -1.0, wantFill: 1, wantHeld: 1,
		},
		{
			// only 3 bars closed, none touch entry, expiry is 6 → still resting
			name:       "short pending (limit above spot, not reached, within expiry)",
			f:          PaperFire{Time: fireT, Side: "short", Entry: 2483, Stop: 2521, TP: 2446},
			cs:         []market.Candle{b(0, 2460, 2472), b(1, 2458, 2470), b(2, 2461, 2475)},
			wantStatus: OutPending,
		},
		{
			// 6 bars closed, entry never touched → expired
			name:       "long no-fill (price ran up, expiry elapsed)",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110},
			cs:         []market.Candle{b(0, 101, 103), b(1, 102, 104), b(2, 101, 105), b(3, 103, 106), b(4, 104, 107), b(5, 105, 108)},
			wantStatus: OutNoFill,
		},
		{
			name:       "short tp 2R",
			f:          PaperFire{Time: fireT, Side: "short", Entry: 100, Stop: 105, TP: 90},
			cs:         []market.Candle{b(0, 99, 100), b(1, 89, 101)},
			wantStatus: OutTP, wantR: 2.0, wantFill: 1, wantHeld: 1,
		},
		{
			name:       "same-bar tie resolves to stop",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110},
			cs:         []market.Candle{b(0, 100, 100), b(1, 94, 111)},
			wantStatus: OutStop, wantR: -1.0, wantFill: 1, wantHeld: 1,
		},
		{
			name:       "filled but open",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110},
			cs:         []market.Candle{b(0, 99, 101), b(1, 98, 102)},
			wantStatus: OutOpen,
		},
		{
			// marketable entry with no closed bar yet → immediately OPEN (not pending)
			name:       "market entry, no bar yet → open",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110, Market: true},
			cs:         nil,
			wantStatus: OutOpen,
		},
		{
			// marketable entry fills at fire (bar 0), tp on same bar → barsToFill 0
			name:       "market entry hits tp same bar",
			f:          PaperFire{Time: fireT, Side: "long", Entry: 100, Stop: 95, TP: 110, Market: true},
			cs:         []market.Candle{b(0, 98, 112)},
			wantStatus: OutTP, wantR: 2.0, wantFill: 0, wantHeld: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateFire(tc.f, tc.cs, 6)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantStatus == OutTP || tc.wantStatus == OutStop {
				if got.NetR != tc.wantR {
					t.Errorf("netR = %v, want %v", got.NetR, tc.wantR)
				}
				if got.BarsToFill != tc.wantFill {
					t.Errorf("barsToFill = %d, want %d", got.BarsToFill, tc.wantFill)
				}
				if got.BarsHeld != tc.wantHeld {
					t.Errorf("barsHeld = %d, want %d", got.BarsHeld, tc.wantHeld)
				}
			}
		})
	}
}
