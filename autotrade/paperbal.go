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
	Realized   float64 // closed positions: NetR × the fire's RiskUSDT
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

// ComputePaperBalance values every position whose fire is at or after since.
// NetR is fee-free, like everywhere else in the paper book.
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
			b.Realized += oc.NetR * f.RiskUSDT
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
