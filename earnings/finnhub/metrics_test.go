package finnhub

import (
	"encoding/json"
	"testing"
)

func TestMapMetrics(t *testing.T) {
	// Includes the slash-key ("totalDebt/totalEquityQuarterly"), a null, a
	// missing field, and a string-encoded number — all must decode robustly.
	raw := `{
		"peTTM": 33.98,
		"psTTM": null,
		"pbQuarterly": 26.93,
		"revenueGrowthTTMYoy": 70.68,
		"netProfitMarginTTM": "62.97",
		"roeTTM": 111.66,
		"currentRatioQuarterly": 3.44,
		"totalDebt/totalEquityQuarterly": 0.043,
		"52WeekHigh": 236.54,
		"52WeekLow": 164.07
	}`
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	got := mapMetrics("NVDA", m)
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"PE", got.PE, 33.98},
		{"PS(null→0)", got.PS, 0},
		{"PB", got.PB, 26.93},
		{"RevGrowth", got.RevGrowthYoY, 70.68},
		{"NetMargin(string→num)", got.NetMargin, 62.97},
		{"ROE", got.ROE, 111.66},
		{"CurrentRatio", got.CurrentRatio, 3.44},
		{"DebtToEquity(slash key)", got.DebtToEquity, 0.043},
		{"Week52High", got.Week52High, 236.54},
		{"EPSGrowth(missing→0)", got.EPSGrowthYoY, 0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if got.Symbol != "NVDA" {
		t.Errorf("Symbol = %q", got.Symbol)
	}
}
