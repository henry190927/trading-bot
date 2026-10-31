package autotrade

import (
	"math"
	"testing"
	"time"
)

func TestComputePaperBalance(t *testing.T) {
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return since.Add(time.Duration(h) * time.Hour) }
	ps := []Position{
		{Fire: PaperFire{Time: at(-5), RiskUSDT: 2}, Outcome: Outcome{Status: OutTP, NetR: 2}},         // before since: ignored
		{Fire: PaperFire{Time: at(1), RiskUSDT: 2.1}, Outcome: Outcome{Status: OutTP, NetR: 2}},        // +4.2
		{Fire: PaperFire{Time: at(2), RiskUSDT: 2.0}, Outcome: Outcome{Status: OutStop, NetR: -1}},     // -2.0
		{Fire: PaperFire{Time: at(3), RiskUSDT: 1.5}, Outcome: Outcome{Status: OutOpen, UnrealR: 0.4}}, // +0.6 unrealized
		{Fire: PaperFire{Time: at(4), RiskUSDT: 2}, Outcome: Outcome{Status: OutNoFill}},               // no money moved
		{Fire: PaperFire{Time: at(5)}, Outcome: Outcome{Status: OutStop, NetR: -1}},                    // no RiskUSDT: counted, not valued
	}
	b := ComputePaperBalance(ps, 140, since)
	near := func(a, w float64) bool { return math.Abs(a-w) < 1e-9 }
	if !near(b.Realized, 2.2) || !near(b.Balance, 142.2) || !near(b.Unrealized, 0.6) {
		t.Errorf("realized %.4f balance %.4f unrealized %.4f, want 2.2 / 142.2 / 0.6", b.Realized, b.Balance, b.Unrealized)
	}
	if b.Closed != 2 || b.Open != 1 || b.NoRisk != 1 {
		t.Errorf("closed %d open %d noRisk %d, want 2 / 1 / 1", b.Closed, b.Open, b.NoRisk)
	}
}

func TestParsePaperSince(t *testing.T) {
	if got := ParsePaperSince("2026-11-01"); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date form: got %v", got)
	}
	if got := ParsePaperSince("2026-11-01T03:00:00+08:00"); !got.Equal(time.Date(2026, 10, 31, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 form: got %v", got)
	}
	if !ParsePaperSince("").IsZero() || !ParsePaperSince("soon").IsZero() {
		t.Errorf("unset or garbage must be the zero time")
	}
}
