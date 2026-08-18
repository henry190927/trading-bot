package fundamental

import (
	"testing"

	"myFirstGo/trading-bot/earnings/finnhub"
)

func TestScore_NVDA_StrongButExpensive_Hold(t *testing.T) {
	// Real NVDA metrics probed 2026-08-18.
	m := finnhub.Metrics{
		Symbol: "NVDA", PE: 33.98, PS: 21.39, PB: 26.93,
		RevGrowthYoY: 70.68, EPSGrowthYoY: 110.34,
		NetMargin: 62.97, GrossMargin: 74.15, OpMargin: 64.02, ROE: 111.66,
		CurrentRatio: 3.44, DebtToEquity: 0.043,
	}
	r := Score(m)
	if r.Label != "hold" {
		t.Errorf("NVDA label = %q, want hold (great company, rich valuation)", r.Label)
	}
	if r.Quality < 95 {
		t.Errorf("NVDA quality = %.1f, want ~100", r.Quality)
	}
	if r.Valuation >= 40 {
		t.Errorf("NVDA valuation = %.1f, want <40 (expensive)", r.Valuation)
	}
	if r.Confidence != "high" {
		t.Errorf("NVDA confidence = %q, want high", r.Confidence)
	}
}

func TestScore_CheapQuality_Buy(t *testing.T) {
	m := finnhub.Metrics{
		Symbol: "GOOD", PE: 12, PS: 2,
		RevGrowthYoY: 30, EPSGrowthYoY: 30,
		NetMargin: 25, ROE: 25,
		CurrentRatio: 2.0, DebtToEquity: 0.3,
	}
	r := Score(m)
	if r.Label != "buy" {
		t.Errorf("label = %q, want buy (strong quality + cheap)", r.Label)
	}
	if r.Quality < 90 || r.Valuation < 90 {
		t.Errorf("expected high quality+valuation, got Q=%.1f V=%.1f", r.Quality, r.Valuation)
	}
}

func TestScore_WeakFundamentals_Avoid(t *testing.T) {
	m := finnhub.Metrics{
		Symbol: "BAD", PS: 15, // negative-earnings P/E = 0 → abstains
		RevGrowthYoY: -5, EPSGrowthYoY: -20,
		NetMargin: -10, ROE: -5,
		CurrentRatio: 0.8, DebtToEquity: 3.0,
	}
	r := Score(m)
	if r.Label != "avoid" {
		t.Errorf("label = %q, want avoid (weak quality)", r.Label)
	}
	if r.Quality >= 40 {
		t.Errorf("quality = %.1f, want <40", r.Quality)
	}
}

func TestScore_NoData_Unknown(t *testing.T) {
	r := Score(finnhub.Metrics{Symbol: "EMPTY"})
	if r.Label != "unknown" {
		t.Errorf("label = %q, want unknown (no fundamentals)", r.Label)
	}
	if r.Confidence != "low" {
		t.Errorf("confidence = %q, want low", r.Confidence)
	}
}

func TestScore_PartialData_AbstainsNotPenalized(t *testing.T) {
	// Only valuation present, no quality metrics → should not be "avoid"
	// (missing quality must abstain, not score as weak).
	m := finnhub.Metrics{Symbol: "PART", PE: 12, PS: 2}
	r := Score(m)
	if r.Label == "avoid" {
		t.Errorf("label = avoid, but quality is absent (should abstain, not penalize)")
	}
	if r.Confidence == "high" {
		t.Errorf("confidence = high with only valuation present; want med/low")
	}
}
