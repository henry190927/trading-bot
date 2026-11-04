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

// Hand-traced:
//
//	tp:   2 × 2.1 = 4.2, fee 2000 × 0.1 = 200 notional × 6bp = 0.12 → 4.08
//	stop: -1 × 2 = -2,   fee 100 × 5    = 500 notional × 6bp = 0.30 → -2.30
func TestPnLUSDT(t *testing.T) {
	tp := PaperFire{RiskUSDT: 2.1, Entry: 2000, Qty: 0.1}
	stop := PaperFire{RiskUSDT: 2, Entry: 100, Qty: 5}
	cases := []struct {
		name   string
		f      PaperFire
		o      Outcome
		want   float64
		wantOK bool
	}{
		{"tp pays the fee", tp, Outcome{Status: OutTP, NetR: 2}, 4.08, true},
		{"stop pays it too", stop, Outcome{Status: OutStop, NetR: -1}, -2.30, true},
		{"open has no settled value", tp, Outcome{Status: OutOpen, UnrealR: 0.5}, 0, false},
		{"pending", tp, Outcome{Status: OutPending}, 0, false},
		{"no-fill moved no money", tp, Outcome{Status: OutNoFill}, 0, false},
		{"unscored", tp, Outcome{Status: OutUnscored}, 0, false},
		{"before risk sizing", PaperFire{Entry: 100, Qty: 5}, Outcome{Status: OutStop, NetR: -1}, 0, false},
	}
	for _, c := range cases {
		got, ok := PnLUSDT(c.f, c.o)
		if ok != c.wantOK || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got (%.4f, %v), want (%.4f, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

// The balance books the same after-fee number the fires table shows.
func TestPaperBalanceRealizedIsAfterFees(t *testing.T) {
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ps := []Position{
		{Fire: PaperFire{Time: since, RiskUSDT: 2.1, Entry: 2000, Qty: 0.1}, Outcome: Outcome{Status: OutTP, NetR: 2}},
		{Fire: PaperFire{Time: since, RiskUSDT: 2, Entry: 100, Qty: 5}, Outcome: Outcome{Status: OutStop, NetR: -1}},
	}
	b := ComputePaperBalance(ps, 140, since)
	if math.Abs(b.Realized-1.78) > 1e-9 || math.Abs(b.Balance-141.78) > 1e-9 {
		t.Errorf("realized %.4f balance %.4f, want 1.78 / 141.78", b.Realized, b.Balance)
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
