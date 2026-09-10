package autotrade

import "fmt"

// Global risk caps for the auto-executor.
//
// MaxConcurrentTotal / MaxMarginTotalUSDT / DailyLossHaltR were declared in
// Config and listed under HARD SAFETY in docs/auto_executor_design.md, but
// until 2026-09-02 their only consumer was the /ops panel that printed them —
// the firing path never read them. The guards that actually ran were per-rule
// per-bar dedup, the macro+earnings blackout, one-position-per-rule, and the
// post-stop cooldown. Harmless in paper, but a live flip with 14 rules × 35u
// could have put ~490u at risk against an advertised 100u ceiling with no
// circuit breaker. This file is the missing enforcement.
//
// The decision is a pure function of (config, book) so it can be tested
// without an exchange or a clock; the book is assembled by the caller.

// Book is the cross-rule state the global caps need, as of one tick.
//
// OpenCount/OpenMargin cover positions that are FILLED or still RESTING —
// a resting limit has committed the capital just as much as a fill, so it
// occupies a slot. RealizedRToday counts only settled outcomes (tp/stop) for
// the current UTC day, matching what the panel labels 已結算; floating R on
// open positions is deliberately excluded, because halting on unrealized
// drawdown would fire on noise that the position may still recover from.
type Book struct {
	OpenCount      int
	OpenMargin     float64
	RealizedRToday float64

	// OpenLegs is one entry per counted-open position, so a cap can reason
	// about WHAT is open rather than only how much. OpenCount stays the
	// authority for the count — a leg with an empty Side (an unscoreable fire,
	// where the side is unknown) still occupies a slot but cannot be matched
	// against, so len(OpenLegs) is not guaranteed to equal OpenCount.
	OpenLegs []Leg
}

// Leg identifies an open position by the two things a concentration rule needs.
type Leg struct {
	Symbol string
	Side   string // "long" | "short"
}

// Candidate is the position CheckCaps is being asked to permit. Passed as a
// struct rather than a widening parameter list because the previous signature
// took only the margin, and the moment a cap needed the symbol and side the
// call sites all had to change anyway — better once, in a shape that absorbs
// the next field.
//
// A zero Candidate (empty Side) is the HALT PROBE: cmd/monitor calls CheckCaps
// once per tick with no candidate purely to read the daily-loss breaker before
// doing any per-rule work. Every candidate-specific rule must skip on it.
type Candidate struct {
	Symbol string
	Side   string
	Margin float64
}

// CapsVerdict explains a caps decision for logging, ntfy and tests.
type CapsVerdict struct {
	Blocked bool
	Reason  string // "" when not blocked
	Halt    bool   // true when the daily-loss circuit breaker tripped
}

// CheckCaps decides whether a NEW position may be opened right now.
//
// A cap of zero (or negative) means UNLIMITED, not "block everything". That
// matters more than it looks: an autotrade.json written before these fields
// existed unmarshals them to 0, and a naive `openCount >= cap` would have
// silently frozen the whole executor the moment this shipped. DailyLossHaltR
// is itself negative in normal use (-3.0R), so zero is its disabled value too.
func CheckCaps(cfg Config, bk Book, cand Candidate) CapsVerdict {
	// Daily-loss halt first: it outranks the sizing caps, and when it trips
	// nothing should open regardless of how much room the others have.
	if cfg.DailyLossHaltR < 0 && bk.RealizedRToday <= cfg.DailyLossHaltR {
		return CapsVerdict{
			Blocked: true, Halt: true,
			Reason: fmt.Sprintf("daily-loss halt: today %+.2fR <= %+.2fR (re-arms at 00:00 UTC)",
				bk.RealizedRToday, cfg.DailyLossHaltR),
		}
	}
	// Same symbol, same direction — refuse a REPEAT of an opinion already on
	// the book. Checked before the global caps because it is the more specific
	// reason: "already short ETH" tells the operator something "8 open >= cap
	// 8" does not.
	//
	// Direction rather than a per-symbol count, on the operator's framing. A
	// count of 2 still permits two ETH longs; this permits ETH long + ETH short
	// (a hedge, which is the actual trading style) and refuses ETH long + ETH
	// long. What hurt was correlation — seven rules expressing one opinion —
	// and direction is the cheapest available proxy for "same opinion".
	//
	// 2026-09-09 is the case: 10 paper positions, 9 of them shorts, ETH 7 of
	// those (6 short) with ETH htf-snr alone firing at 08:00, 17:00 and 18:00.
	// One-position-per-RULE bounds none of that.
	//
	// Zero = UNLIMITED, same convention as the caps above, so an existing
	// autotrade.json unmarshals to today's behaviour.
	if cfg.MaxSameSymbolSide > 0 && cand.Side != "" {
		n := 0
		for _, l := range bk.OpenLegs {
			if l.Symbol == cand.Symbol && l.Side == cand.Side {
				n++
			}
		}
		if n >= cfg.MaxSameSymbolSide {
			return CapsVerdict{
				Blocked: true,
				Reason: fmt.Sprintf("max-same-symbol-side: %d %s %s already open >= cap %d",
					n, cand.Symbol, cand.Side, cfg.MaxSameSymbolSide),
			}
		}
	}
	if cfg.MaxConcurrentTotal > 0 && bk.OpenCount >= cfg.MaxConcurrentTotal {
		return CapsVerdict{
			Blocked: true,
			Reason: fmt.Sprintf("max-concurrent-total: %d open >= cap %d",
				bk.OpenCount, cfg.MaxConcurrentTotal),
		}
	}
	if cfg.MaxMarginTotalUSDT > 0 && bk.OpenMargin+cand.Margin > cfg.MaxMarginTotalUSDT {
		return CapsVerdict{
			Blocked: true,
			Reason: fmt.Sprintf("max-margin-total: %.0fu open + %.0fu new > cap %.0fu",
				bk.OpenMargin, cand.Margin, cfg.MaxMarginTotalUSDT),
		}
	}
	return CapsVerdict{}
}
