package macro

import (
	"testing"
	"time"
)

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func names(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// The curated windows these cases lean on, from macro/events.json:
//
//	US CPI (August)                 2026-09-11T12:30Z  -60/+90  -> [11:30Z, 14:00Z)
//	FOMC Rate Decision (September)  2026-09-16T18:00Z  -60/+120 -> [17:00Z, 20:00Z)
func TestUncovered(t *testing.T) {
	from := mustUTC(t, "2026-09-10T14:00:00Z")

	cands := []Candidate{
		// Before `from` — today's PPI, already past by then.
		{Name: "PPI m/m", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-10T12:30:00Z")},
		// Dead centre of the CPI window.
		{Name: "CPI m/m", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-11T12:30:00Z")},
		// Same day, hours before the CPI window opens.
		{Name: "GDP m/m", Country: "GBP", DatetimeUTC: mustUTC(t, "2026-09-11T06:00:00Z")},
		// Exactly the CPI window's end instant. Contains() is
		// end-exclusive, so this is NOT covered.
		{Name: "Prelim UoM", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-11T14:00:00Z")},
		// Beyond a 48h horizon.
		{Name: "FOMC", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-16T18:00:00Z")},
	}

	got := names(Uncovered(cands, from, 48*time.Hour))
	want := []string{"GDP m/m", "Prelim UoM"} // soonest first
	if len(got) != len(want) {
		t.Fatalf("Uncovered() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Uncovered()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// The 2026-09-10 failure as a fixture: PPI was not in the table and must be
// reported, while the CPI row added in the same change must now be covered.
// If someone deletes the CPI entry, the second half of this fails.
func TestUncoveredReportsThePPIGapAndNotTheCPIFix(t *testing.T) {
	from := mustUTC(t, "2026-09-10T00:00:00Z")
	cands := []Candidate{
		{Name: "PPI m/m", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-10T12:30:00Z")},
		{Name: "CPI m/m", Country: "USD", DatetimeUTC: mustUTC(t, "2026-09-11T12:30:00Z")},
	}

	got := names(Uncovered(cands, from, 48*time.Hour))
	if len(got) != 1 || got[0] != "PPI m/m" {
		t.Fatalf("Uncovered() = %v, want exactly [PPI m/m] — "+
			"CPI 2026-09-11T12:30Z should be covered by the curated table", got)
	}
}

func TestUncoveredEdgeCases(t *testing.T) {
	from := mustUTC(t, "2026-09-10T00:00:00Z")
	one := []Candidate{{Name: "x", DatetimeUTC: mustUTC(t, "2026-09-10T06:00:00Z")}}

	if got := Uncovered(nil, from, 48*time.Hour); got != nil {
		t.Errorf("Uncovered(nil) = %v, want nil", got)
	}
	if got := Uncovered(one, from, 0); got != nil {
		t.Errorf("Uncovered(horizon=0) = %v, want nil", got)
	}
	if got := Uncovered(one, from, -time.Hour); got != nil {
		t.Errorf("Uncovered(horizon<0) = %v, want nil", got)
	}
	// A non-UTC input must be compared in UTC, not wall-clock.
	tpe := time.FixedZone("Asia/Taipei", 8*3600)
	shifted := []Candidate{{
		Name:        "CPI m/m",
		DatetimeUTC: mustUTC(t, "2026-09-11T12:30:00Z").In(tpe), // 20:30 TPE
	}}
	if got := Uncovered(shifted, from, 48*time.Hour); len(got) != 0 {
		t.Errorf("Uncovered(TPE-zoned CPI) = %v, want empty (it is inside the CPI window)", names(got))
	}
}
