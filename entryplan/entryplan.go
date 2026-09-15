// Package entryplan validates and sizes a LIMIT ENTRY before it is sent.
//
// Every other order surface in this project is reduce-only and therefore
// cannot lose money by itself: /ops/protect attaches stops, cmd/protect
// attaches stops, the bracket guard closes. This one OPENS a position, so its
// rails are the strict ones.
//
// The rule that matters is the first fault below: an entry without a stop is
// REFUSED, not warned about. Trade #60 went naked because a bundled stop
// silently failed to attach; #70 sat naked for 58 minutes and then had its
// stop removed by hand. A surface that makes placing a naked entry one field
// away from placing a protected one will eventually be used to place a naked
// one at 4am. There is no override for this.
//
// Leverage, notional and concurrency caps are NOT re-implemented here — they
// live in package risk, which the handler consults with the same Limits the
// rest of the system uses. Two implementations of "is this position too big"
// is how they drift apart.
package entryplan

import (
	"fmt"
	"math"
	"strings"
)

// Inputs is everything needed to size and check one entry.
type Inputs struct {
	Side       string  // "long" | "short"
	Entry      float64 // limit price
	Stop       float64 // REQUIRED; there is no valid zero
	TP         float64 // 0 = no bundled take-profit
	MarginUSDT float64
	Leverage   int
	Mark       float64 // live mark, for the marketable check
	Equity     float64 // account equity, for the risk-as-%-of-equity readout
	QtyStep    int     // decimals to floor the quantity to

	// AllowMarketable lets a limit that would fill immediately through. Off by
	// default: a long limit ABOVE the mark is a market buy wearing a limit's
	// clothes, and chasing is this account's most expensive documented habit.
	AllowMarketable bool
}

// Plan is the checked, sized order plus everything a human needs to see
// before confirming it.
type Plan struct {
	Side            string
	Entry, Stop, TP float64
	Qty             float64
	NotionalUSDT    float64
	MarginUSDT      float64
	Leverage        int
	RiskUSDT        float64 // what the stop costs if it fills and then triggers
	RiskPctEquity   float64
	RewardUSDT      float64 // 0 when no TP
	RR              float64 // reward:risk on the stop distance; 0 when no TP
	Marketable      bool    // would fill at once against the mark
	Faults          []string
	Warnings        []string
}

// OK reports whether the plan may be sent.
func (p Plan) OK() bool { return len(p.Faults) == 0 }

// RiskWarnPctEquity is where the risk readout starts shouting. Not a block:
// the account's own risk gate ships warn-only by the owner's explicit choice,
// and a second hard wall here would overrule that decision from a different
// file.
const RiskWarnPctEquity = 15.0

// Build sizes and checks an entry. It never returns an error — every problem
// is a fault or a warning on the Plan, so a caller renders one shape.
func Build(in Inputs) Plan {
	p := Plan{
		Side:  strings.ToLower(strings.TrimSpace(in.Side)),
		Entry: in.Entry, Stop: in.Stop, TP: in.TP,
		MarginUSDT: in.MarginUSDT, Leverage: in.Leverage,
	}
	fault := func(f string, a ...any) { p.Faults = append(p.Faults, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }

	long := p.Side == "long"
	if p.Side != "long" && p.Side != "short" {
		fault("side must be long or short, got %q", in.Side)
		return p
	}
	if in.Entry <= 0 {
		fault("entry must be > 0")
	}
	if in.Leverage <= 0 {
		fault("leverage must be > 0")
	}
	if in.MarginUSDT <= 0 {
		fault("margin must be > 0")
	}

	// The one that has no override.
	if in.Stop <= 0 {
		fault("stop is required — this route does not place naked entries")
	} else if in.Entry > 0 {
		if long && in.Stop >= in.Entry {
			fault("long stop %.4f must be BELOW entry %.4f", in.Stop, in.Entry)
		}
		if !long && in.Stop <= in.Entry {
			fault("short stop %.4f must be ABOVE entry %.4f", in.Stop, in.Entry)
		}
	}
	if in.TP > 0 && in.Entry > 0 {
		if long && in.TP <= in.Entry {
			fault("long tp %.4f must be ABOVE entry %.4f", in.TP, in.Entry)
		}
		if !long && in.TP >= in.Entry {
			fault("short tp %.4f must be BELOW entry %.4f", in.TP, in.Entry)
		}
	}

	// Marketable: a long resting AT or ABOVE the mark fills instantly.
	if in.Mark > 0 && in.Entry > 0 {
		p.Marketable = (long && in.Entry >= in.Mark) || (!long && in.Entry <= in.Mark)
		if p.Marketable && !in.AllowMarketable {
			fault("entry %.4f fills immediately against mark %.4f — that is a market order, "+
				"not a resting limit; pass allow_marketable=1 if that is the intent",
				in.Entry, in.Mark)
		}
	}

	if len(p.Faults) > 0 {
		return p
	}

	step := in.QtyStep
	if step < 0 {
		step = 0
	}
	f := math.Pow(10, float64(step))
	p.Qty = math.Floor(in.MarginUSDT*float64(in.Leverage)/in.Entry*f) / f
	if p.Qty <= 0 {
		fault("computed qty rounds to 0 at %d dp (margin %.2f x %d / %.4f)",
			step, in.MarginUSDT, in.Leverage, in.Entry)
		return p
	}
	p.NotionalUSDT = p.Qty * in.Entry
	p.RiskUSDT = p.Qty * math.Abs(in.Entry-in.Stop)
	if in.TP > 0 {
		p.RewardUSDT = p.Qty * math.Abs(in.TP-in.Entry)
		if d := math.Abs(in.Entry - in.Stop); d > 0 {
			p.RR = math.Abs(in.TP-in.Entry) / d
		}
	}
	if in.Equity > 0 {
		p.RiskPctEquity = p.RiskUSDT / in.Equity * 100
		if p.RiskPctEquity > RiskWarnPctEquity {
			warn("stop risk %.1fu is %.1f%% of %.2fu equity", p.RiskUSDT, p.RiskPctEquity, in.Equity)
		}
	}
	// A TP closer than the stop is allowed — it is a real choice on a
	// high-probability level — but it is stated, because a sub-1R target needs
	// a win rate most setups do not have.
	if in.TP > 0 && p.RR > 0 && p.RR < 1 {
		warn("target is %.2fR — below 1R, so this needs a win rate above 50%% just to break even", p.RR)
	}
	if in.TP <= 0 {
		warn("no take-profit: the position will have a stop but no resting exit")
	}
	return p
}
