package main

import (
	"math"
	"testing"
)

// Blank must mean "leave this leg alone", and a bad value must be an ERROR,
// never a silent 0. A silent zero here places nothing while the UI reads as
// handled — which is the exact shape of every naked-position incident this
// endpoint exists to prevent.
func TestOptionalPriceBlankIsSkipNotZero(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		v, err := optionalPrice(raw)
		if err != nil {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
		if v != 0 {
			t.Errorf("%q: got %v, want 0 (meaning skip)", raw, v)
		}
	}
}

func TestOptionalPriceParses(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"79880", 79880},
		{" 2495.5 ", 2495.5},
		{"0.7139", 0.7139},
		{"0", 0},
	} {
		got, err := optionalPrice(tc.raw)
		if err != nil {
			t.Errorf("%q: %v", tc.raw, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%q: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestOptionalPriceRejectsGarbageAndNegatives(t *testing.T) {
	for _, raw := range []string{"abc", "79,880", "1e", "--5", "-100"} {
		if v, err := optionalPrice(raw); err == nil {
			t.Errorf("%q parsed to %v — must be an error, not a silent value", raw, v)
		}
	}
}
