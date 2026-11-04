package autotrade

import (
	"strings"
	"time"
)

// PaperBalance is a display-only account for the paper book: a starting
// balance plus what the positions opened since then made or lost in USDT.
// Nothing sizes off it — the executor sizes every fire from the real
// account's equity — so it answers "how would the balance have moved",
// not "what will the next order be".
type PaperBalance struct {
	Base       float64
	Since      time.Time
	Realized   float64 // closed positions: PnLUSDT, so after fees
	Unrealized float64 // open positions: UnrealR × RiskUSDT, marked to the last close
	Balance    float64 // Base + Realized
	Closed     int
	Open       int
	NoRisk     int // positions with no recorded RiskUSDT, so no money value: left out
}

// ParsePaperSince reads autotrade.json's paper_since: an RFC3339 time or a
// plain date (midnight UTC). The zero time means "not set".
func ParsePaperSince(s string) time.Time {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	return time.Time{}
}

// FeeBp is the round-trip fee charged against a settled paper trade, in
// basis points of the entry notional — the 6 vpbt charges by default (user's
// call 2026-11-04). R stays fee-free everywhere; only the USDT figures pay it.
const FeeBp = 6.0

// PnLUSDT is what a settled paper position made or lost in USDT, after fees.
//
// Contract (paperbal_test.go is the definition of done):
//   - Only a settled position has a value: o.Status is OutTP or OutStop.
//     Anything else (open, pending, no-fill, unscored) → (0, false).
//   - f.RiskUSDT <= 0 means the fire predates risk sizing (before 2026-10-18)
//     and has no money value → (0, false).
//   - Otherwise u = o.NetR × f.RiskUSDT − fee, where fee = f.Entry × f.Qty ×
//     FeeBp / 10000 (round trip, on the entry notional), and ok = true.
func PnLUSDT(f PaperFire, o Outcome) (u float64, ok bool) {
	if o.Status != OutTP && o.Status != OutStop {
		return 0, false
	}
	if f.RiskUSDT <= 0 {
		return 0, false
	}
	return o.NetR*f.RiskUSDT - f.Entry*f.Qty*FeeBp/10000, true
}

// ComputePaperBalance values every position whose fire is at or after since.
// Realized is after fees (PnLUSDT); Unrealized is not — no fee is paid until
// the position closes.
func ComputePaperBalance(ps []Position, base float64, since time.Time) PaperBalance {
	b := PaperBalance{Base: base, Since: since}
	for _, p := range ps {
		f, oc := p.Fire, p.Outcome
		if f.Time.Before(since) {
			continue
		}
		switch oc.Status {
		case OutTP, OutStop:
			if f.RiskUSDT <= 0 {
				b.NoRisk++
				continue
			}
			u, _ := PnLUSDT(f, oc)
			b.Realized += u
			b.Closed++
		case OutOpen:
			if f.RiskUSDT <= 0 {
				b.NoRisk++
				continue
			}
			b.Unrealized += oc.UnrealR * f.RiskUSDT
			b.Open++
		}
	}
	b.Balance = b.Base + b.Realized
	return b
}
