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
//
// newMargin is the margin the candidate rule would commit.
func CheckCaps(cfg Config, bk Book, newMargin float64) CapsVerdict {
	// Daily-loss halt first: it outranks the sizing caps, and when it trips
	// nothing should open regardless of how much room the others have.
	if cfg.DailyLossHaltR < 0 && bk.RealizedRToday <= cfg.DailyLossHaltR {
		return CapsVerdict{
			Blocked: true, Halt: true,
			Reason: fmt.Sprintf("daily-loss halt: today %+.2fR <= %+.2fR (re-arms at 00:00 UTC)",
				bk.RealizedRToday, cfg.DailyLossHaltR),
		}
	}
	if cfg.MaxConcurrentTotal > 0 && bk.OpenCount >= cfg.MaxConcurrentTotal {
		return CapsVerdict{
			Blocked: true,
			Reason: fmt.Sprintf("max-concurrent-total: %d open >= cap %d",
				bk.OpenCount, cfg.MaxConcurrentTotal),
		}
	}
	if cfg.MaxMarginTotalUSDT > 0 && bk.OpenMargin+newMargin > cfg.MaxMarginTotalUSDT {
		return CapsVerdict{
			Blocked: true,
			Reason: fmt.Sprintf("max-margin-total: %.0fu open + %.0fu new > cap %.0fu",
				bk.OpenMargin, newMargin, cfg.MaxMarginTotalUSDT),
		}
	}
	return CapsVerdict{}
}
