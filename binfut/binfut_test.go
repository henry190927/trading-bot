package binfut

import (
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)

// Buckets every 5 minutes ending at base, with round values so every expected
// figure below is exact arithmetic rather than a number read back off a run.
func oiSeries(vals ...float64) []OIPoint {
	out := make([]OIPoint, 0, len(vals))
	n := len(vals)
	for i, v := range vals {
		out = append(out, OIPoint{
			Time:  base.Add(-time.Duration(n-1-i) * 5 * time.Minute),
			Value: v,
		})
	}
	return out
}

func ratio(v float64) []RatioPoint {
	return []RatioPoint{{Time: base, Ratio: v}}
}

func TestOIChange(t *testing.T) {
	// 13 buckets x 5m = the oldest is 60 minutes before the newest.
	s := Store{Symbols: map[string]SymbolData{
		"ETH": {OI: oiSeries(100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 110)},
	}}

	// One hour back from the newest bucket is the oldest one, 100 -> 110.
	if v, ok := s.OIChange("ETH", time.Hour); !ok || math.Abs(v-0.10) > 1e-9 {
		t.Errorf("OIChange(1h) = (%v, %v), want (0.10, true)", v, ok)
	}
	// 30 minutes back is the bucket at index 6 (106): 110/106 - 1.
	if v, ok := s.OIChange("ETH", 30*time.Minute); !ok {
		t.Errorf("OIChange(30m) not ok")
	} else if want := 110.0/106.0 - 1; math.Abs(v-want) > 1e-9 {
		t.Errorf("OIChange(30m) = %v, want %v", v, want)
	}

	// Beyond the series must report not-found, never 0 — beside a real
	// reading a zero would look like "flat" rather than "cannot say".
	if v, ok := s.OIChange("ETH", 4*time.Hour); ok {
		t.Errorf("OIChange(4h) = (%v, true), want ok=false — series is only 1h", v)
	}
	if _, ok := s.OIChange("NOPE", time.Hour); ok {
		t.Error("OIChange on an unknown symbol reported ok=true")
	}
	if _, ok := s.OIChange("ETH", 0); ok {
		t.Error("OIChange(window=0) reported ok=true")
	}
	// A single bucket cannot express a change.
	one := Store{Symbols: map[string]SymbolData{"X": {OI: oiSeries(100)}}}
	if _, ok := one.OIChange("X", time.Hour); ok {
		t.Error("OIChange on a one-bucket series reported ok=true")
	}
}

func TestWhales(t *testing.T) {
	// The live 2026-09-11 reading: ETH top traders 1.4021 by account count
	// against 2.4941 for all accounts, with the size-weighted top at 1.2659.
	s := Store{Symbols: map[string]SymbolData{
		"ETH": {
			TopAccounts:  ratio(1.4021),
			AllAccounts:  ratio(2.4941),
			TopPositions: ratio(1.2659),
		},
		// BTC the same session, leaning the other way.
		"BTC": {TopAccounts: ratio(2.1234), AllAccounts: ratio(1.6015)},
		// Ratios missing entirely.
		"SUI": {OI: oiSeries(1, 2)},
	}}

	w, ok := s.Whales("ETH")
	if !ok {
		t.Fatal("Whales(ETH) not ok")
	}
	if math.Abs(w.Gap-(1.4021-2.4941)) > 1e-9 {
		t.Errorf("ETH gap = %v, want %v", w.Gap, 1.4021-2.4941)
	}
	if got := w.Crowd(); got != "retail-longer" {
		t.Errorf("ETH Crowd() = %q, want retail-longer (gap %.4f)", got, w.Gap)
	}
	// The size-weighted whale reading sits below the headcount one — carried,
	// not blended into the gap.
	if w.TopSz >= w.Top {
		t.Errorf("TopSz %.4f should be below Top %.4f in this fixture", w.TopSz, w.Top)
	}

	if w, _ := s.Whales("BTC"); w.Crowd() != "retail-shorter" {
		t.Errorf("BTC Crowd() = %q, want retail-shorter (gap %.4f)", w.Crowd(), w.Gap)
	}
	if _, ok := s.Whales("SUI"); ok {
		t.Error("Whales with no ratio series reported ok=true")
	}
	if _, ok := s.Whales("NOPE"); ok {
		t.Error("Whales on an unknown symbol reported ok=true")
	}
}

// The two populations never sit exactly equal, so a small gap must stay
// unnamed rather than being reported as a crowd read.
func TestCrowdNoiseBand(t *testing.T) {
	for _, c := range []struct {
		gap  float64
		want string
	}{
		{-1.09, "retail-longer"},  // the live ETH case
		{+0.52, "retail-shorter"}, // the live BTC case
		{-WhaleGapMeaningful, "retail-longer"},
		{+WhaleGapMeaningful, "retail-shorter"},
		{-0.24, ""},
		{+0.24, ""},
		{0, ""},
	} {
		if got := (Whale{Gap: c.gap}).Crowd(); got != c.want {
			t.Errorf("Crowd(gap %.4f) = %q, want %q", c.gap, got, c.want)
		}
	}
}

func TestStale(t *testing.T) {
	s := Store{FetchedAt: base}
	if s.Stale(base.Add(MinRefresh - time.Second)) {
		t.Errorf("younger than MinRefresh (%v) should not be stale", MinRefresh)
	}
	if !s.Stale(base.Add(MinRefresh)) {
		t.Errorf("exactly %v old should be stale", MinRefresh)
	}
	if !(Store{}).Stale(base) {
		t.Error("an unfetched cache must always be stale")
	}
}

// Every roster short name must map, or the symbol silently vanishes from the
// cross-reference with no error anywhere.
func TestSymbolsCoverTheRoster(t *testing.T) {
	roster := []string{"BTC", "ETH", "XAU", "XAG", "SNDK", "NVDA", "SPCX",
		"MSTR", "APP", "SOL", "LINK", "SUI", "HYPE", "NEAR"}
	for _, short := range roster {
		if _, ok := Symbols[short]; !ok {
			t.Errorf("roster symbol %q has no Binance mapping", short)
		}
	}
	if len(Symbols) != len(roster) {
		t.Errorf("Symbols has %d entries, roster has %d — one side drifted",
			len(Symbols), len(roster))
	}
}
