package bls

import (
	"math"
	"testing"
	"time"
)

// Round fixture values so every expected figure below is exact arithmetic
// rather than something read back off an implementation run.
func fixture() Store {
	return Store{
		FetchedAt: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
		Series: map[string][]Point{
			"CUSR0000SA0": {
				{Year: 2026, Month: 6, Value: 100},
				{Year: 2026, Month: 7, Value: 101}, // m/m into July = +1.00%
			},
			"CUUR0000SA0": {
				{Year: 2025, Month: 8, Value: 100},
				{Year: 2026, Month: 8, Value: 105}, // y/y into Aug 2026 = +5.00%
			},
			"CUSR0000SA0L1E": {
				{Year: 2026, Month: 6, Value: 200},
				{Year: 2026, Month: 7, Value: 200.4}, // +0.20%
			},
			"CES0000000001": {
				{Year: 2026, Month: 7, Value: 159000},
				{Year: 2026, Month: 8, Value: 159162}, // +162 thousand
			},
			// December -> January, to prove the year rolls back.
			"WPSFD4": {
				{Year: 2025, Month: 12, Value: 150},
				{Year: 2026, Month: 1, Value: 151.5}, // +1.00%
			},
		},
	}
}

func TestMetricForIsExactMatch(t *testing.T) {
	// The substring trap: "Core CPI m/m" contains "CPI m/m", and answering
	// the core row with the headline series would be a wrong number in the
	// one place a wrong number does damage.
	core, ok := MetricFor("Core CPI m/m")
	if !ok || core.SeriesID != "CUSR0000SA0L1E" {
		t.Errorf("MetricFor(Core CPI m/m) = %+v ok=%v, want the core series", core, ok)
	}
	head, ok := MetricFor("CPI m/m")
	if !ok || head.SeriesID != "CUSR0000SA0" {
		t.Errorf("MetricFor(CPI m/m) = %+v ok=%v, want the headline series", head, ok)
	}
	if core.SeriesID == head.SeriesID {
		t.Fatal("core and headline CPI resolved to the same series")
	}

	// m/m uses SA, y/y uses NSA — the convention each figure is published in.
	if m, _ := MetricFor("CPI y/y"); m.SeriesID != "CUUR0000SA0" {
		t.Errorf("CPI y/y resolved to %q, want the NSA series", m.SeriesID)
	}

	// Whitespace is normalised; anything unrecognised stays unanswered.
	if _, ok := MetricFor("  CPI   m/m  "); !ok {
		t.Error("MetricFor should normalise whitespace")
	}
	for _, title := range []string{
		"Core PPI m/m", // deliberately unmapped: four candidate IDs disagree 7x
		"Unemployment Claims",
		"Prelim UoM Consumer Sentiment",
		"GDP m/m",
		"",
	} {
		if m, ok := MetricFor(title); ok {
			t.Errorf("MetricFor(%q) = %+v, want not-found", title, m)
		}
	}
}

func TestDataMonthFor(t *testing.T) {
	for _, c := range []struct {
		release      string
		wantY, wantM int
	}{
		// Verified against the API: after the 2026-09-10 PPI release,
		// WPSFD4's newest observation is 2026-M08.
		{"2026-09-10", 2026, 8},
		{"2026-09-11", 2026, 8},
		{"2026-09-04", 2026, 8},
		{"2026-08-12", 2026, 7},
		// January release reports the previous December.
		{"2026-01-15", 2025, 12},
	} {
		rel, err := time.Parse("2006-01-02", c.release)
		if err != nil {
			t.Fatal(err)
		}
		y, m := DataMonthFor(rel)
		if y != c.wantY || m != c.wantM {
			t.Errorf("DataMonthFor(%s) = %d-%02d, want %d-%02d", c.release, y, m, c.wantY, c.wantM)
		}
	}
}

func TestTransforms(t *testing.T) {
	s := fixture()

	if v, ok := s.MoM("CUSR0000SA0", 2026, 7); !ok || math.Abs(v-1.0) > 1e-9 {
		t.Errorf("MoM = (%v, %v), want (1.00, true)", v, ok)
	}
	if v, ok := s.YoY("CUUR0000SA0", 2026, 8); !ok || math.Abs(v-5.0) > 1e-9 {
		t.Errorf("YoY = (%v, %v), want (5.00, true)", v, ok)
	}
	if v, ok := s.Diff("CES0000000001", 2026, 8); !ok || math.Abs(v-162) > 1e-9 {
		t.Errorf("Diff = (%v, %v), want (162, true)", v, ok)
	}
	// A January m/m must reach back into the previous December.
	if v, ok := s.MoM("WPSFD4", 2026, 1); !ok || math.Abs(v-1.0) > 1e-9 {
		t.Errorf("MoM across the year boundary = (%v, %v), want (1.00, true)", v, ok)
	}

	// Every "cannot answer" must report not-ok, never a zero: a 0.0% printed
	// beside a 0.4% forecast reads as a miss rather than as silence.
	if v, ok := s.MoM("CUSR0000SA0", 2026, 6); ok {
		t.Errorf("MoM with no prior month = (%v, true), want ok=false", v)
	}
	if v, ok := s.MoM("CUSR0000SA0", 2026, 9); ok {
		t.Errorf("MoM for an unreleased month = (%v, true), want ok=false", v)
	}
	if v, ok := s.YoY("CUSR0000SA0", 2026, 7); ok {
		t.Errorf("YoY with no year-ago month = (%v, true), want ok=false", v)
	}
	if v, ok := s.MoM("NOSUCHSERIES", 2026, 7); ok {
		t.Errorf("MoM on an unknown series = (%v, true), want ok=false", v)
	}
}

func TestActual(t *testing.T) {
	s := fixture()

	// Released 2026-08-12 reports July.
	v, unit, ok := s.Actual("CPI m/m", time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC))
	if !ok || math.Abs(v-1.0) > 1e-9 || unit != "%" {
		t.Errorf("Actual(CPI m/m) = (%v, %q, %v), want (1.00, %%, true)", v, unit, ok)
	}

	v, unit, ok = s.Actual("Non-Farm Employment Change", time.Date(2026, 9, 4, 12, 30, 0, 0, time.UTC))
	if !ok || math.Abs(v-162) > 1e-9 || unit != "k" {
		t.Errorf("Actual(NFP) = (%v, %q, %v), want (162, k, true)", v, unit, ok)
	}

	// Unmapped title, and a mapped title whose month is not in the cache.
	if _, _, ok := s.Actual("Core PPI m/m", time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)); ok {
		t.Error("Actual(Core PPI m/m) answered; it is deliberately unmapped")
	}
	if _, _, ok := s.Actual("CPI m/m", time.Date(2026, 10, 13, 12, 30, 0, 0, time.UTC)); ok {
		t.Error("Actual for an unreleased month answered; want not-found")
	}
}

func TestStale(t *testing.T) {
	s := fixture()
	base := s.FetchedAt
	if s.Stale(base.Add(time.Hour)) {
		t.Error("a one-hour-old cache should not be stale")
	}
	if !s.Stale(base.Add(MinRefresh)) {
		t.Errorf("a cache exactly %v old should be stale", MinRefresh)
	}
	if !(Store{}).Stale(base) {
		t.Error("an unfetched cache must always be stale")
	}
}
