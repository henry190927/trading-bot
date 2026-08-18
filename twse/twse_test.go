package twse

import (
	"strings"
	"testing"
)

func TestAtof(t *testing.T) {
	cases := map[string]struct {
		f  float64
		ok bool
	}{
		"27.82": {27.82, true}, "1,234,567": {1234567, true},
		"-": {0, false}, "": {0, false}, "N/A": {0, false}, "abc": {0, false},
	}
	for in, want := range cases {
		f, ok := atof(in)
		if f != want.f || ok != want.ok {
			t.Errorf("atof(%q) = %v,%v want %v,%v", in, f, ok, want.f, want.ok)
		}
	}
}

func TestRate(t *testing.T) {
	cheap := Stock{Code: "X", PE: 10, PB: 1.2, DividendYield: 5, RevYoYPct: 25, GrossMarginPct: 40, HasRev: true, HasMargin: true}
	if r := Rate(cheap); r.Label != "buy" || r.Valuation < 90 || r.Quality < 90 || !r.HighYield || !strings.Contains(r.Note, "高股息") {
		t.Errorf("cheap-quality-highyield: %+v", r)
	}
	rich := Stock{Code: "X", PE: 40, PB: 8, RevYoYPct: 30, GrossMarginPct: 50, HasRev: true, HasMargin: true}
	if r := Rate(rich); r.Label != "hold" || r.Valuation >= 40 {
		t.Errorf("rich: want hold+low val, got %+v", r)
	}
	weak := Stock{Code: "X", PE: 15, PB: 2, RevYoYPct: -10, GrossMarginPct: 5, HasRev: true, HasMargin: true}
	if r := Rate(weak); r.Label != "avoid" {
		t.Errorf("weak: want avoid, got %+v", r)
	}
	valOnly := Stock{Code: "X", PE: 10, PB: 1.2} // no rev/margin
	if r := Rate(valOnly); r.Label != "buy" || !strings.Contains(r.Note, "valuation-only") {
		t.Errorf("valuation-only cheap: want buy+note, got %+v", r)
	}
	noData := Stock{Code: "X"}
	if r := Rate(noData); r.Label != "unknown" {
		t.Errorf("no data: want unknown, got %+v", r)
	}
}
