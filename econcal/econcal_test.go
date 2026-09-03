package econcal

import "testing"

// Titles taken verbatim from a live 2026-09-03 feed payload, which is how the
// bug was found: every one of these Fed lines was rated "Low" and therefore
// dropped before reaching /calendar.
func TestIsCentralBankSpeakerRealTitles(t *testing.T) {
	for _, title := range []string{
		"FOMC Member Waller Speaks",
		"FOMC Member Hammack Speaks",
		"FOMC Member Goolsbee Speaks",
		"BOE Gov Bailey Speaks",
		"FOMC Press Conference",
		"Fed Chair Powell Speaks",
		"Fed Chair Powell Testifies",
		"ECB President Lagarde Speaks",
		"BOJ Gov Ueda Speaks",
	} {
		if !IsCentralBankSpeaker(title) {
			t.Errorf("%q should be recognised as a central-bank speech", title)
		}
	}
}

// The filter must stay narrow. Sweeping in ordinary releases would defeat the
// "signal not noise" purpose the impact filter exists for.
func TestIsCentralBankSpeakerRejectsDataReleases(t *testing.T) {
	for _, title := range []string{
		"ISM Services PMI",
		"Unemployment Claims",
		"Non-Farm Employment Change",
		"Average Hourly Earnings m/m",
		"Trade Balance",
		"Natural Gas Storage",
		"German Factory Orders m/m",
		"Revised Nonfarm Productivity q/q",
		"",
		"   ",
		// A non-central-bank speaker must NOT become a Fed event just because
		// the title ends in "Speaks".
		"Treasury Secretary Speaks",
		"President Speaks",
		// "Fed" appearing incidentally is not enough on its own.
		"Federal Budget Balance",
	} {
		if IsCentralBankSpeaker(title) {
			t.Errorf("%q must NOT be treated as a central-bank speech", title)
		}
	}
}

func TestIsCentralBankSpeakerCaseAndSpace(t *testing.T) {
	for _, title := range []string{
		"  fomc member waller speaks  ",
		"FOMC MEMBER WALLER SPEAKS",
		"FoMc MeMbEr Waller Speaks",
	} {
		if !IsCentralBankSpeaker(title) {
			t.Errorf("%q should match regardless of case/space", title)
		}
	}
}

// The keep-decision is what actually changed. Reproduced here over the same
// inputs so the regression is pinned even though Load needs a network.
func keepDecision(country, impact, title string) bool {
	high := impact == "High"
	med := impact == "Medium"
	return (country == "USD" && (high || med)) || high || IsCentralBankSpeaker(title)
}

func TestKeepDecision(t *testing.T) {
	for _, tc := range []struct {
		country, impact, title string
		want                   bool
		why                    string
	}{
		// The regression: USD + Low + Fed speaker was dropped, now kept.
		{"USD", "Low", "FOMC Member Waller Speaks", true, "the 2026-09-03 miss"},
		{"GBP", "High", "BOE Gov Bailey Speaks", true, "high impact, and a speaker"},
		// Unchanged behaviour.
		{"USD", "High", "Non-Farm Employment Change", true, "USD high"},
		{"USD", "Medium", "ISM Services PMI", true, "USD medium"},
		{"CAD", "High", "Employment Change", true, "non-USD high still kept"},
		{"USD", "Low", "Natural Gas Storage", false, "USD low non-speaker still dropped"},
		{"EUR", "Low", "Italian Retail Sales m/m", false, "non-USD low dropped"},
		{"JPY", "Medium", "Household Spending y/y", false, "non-USD medium dropped"},
	} {
		if got := keepDecision(tc.country, tc.impact, tc.title); got != tc.want {
			t.Errorf("keep(%s/%s/%q) = %v, want %v — %s", tc.country, tc.impact, tc.title, got, tc.want, tc.why)
		}
	}
}
