package autotrade

import (
	"time"

	"myFirstGo/trading-bot/market"
)

// Book assembly, shared by the executor (which enforces the caps) and the /ops
// panel (which reports them). Keeping ONE implementation is the point: a panel
// that computed its own view could disagree with what actually gates a trade,
// which is the exact failure mode this whole area was just audited for.

// CandleFn resolves candles for a fire's (symbol, TF). Injected so BuildBook is
// testable without an exchange, and so the executor and the /ops panel can each
// supply their own source while sharing the accounting.
type CandleFn func(symbol, tf string) []market.Candle

// BuildBook assembles the current book from the fires log.
//
// Open/pending: the newest fire per (symbol, strategy). One position per rule is
// already enforced structurally, so the newest fire per rule IS the complete
// open book. Both OutOpen and OutPending occupy a slot — a resting limit has
// committed the capital just as much as a fill has.
//
// RealizedRToday: every fire whose settled exit falls on `now`'s UTC date.
// Scanning fires rather than deduped positions keeps this independent of dedup
// bookkeeping; a fire that never filled contributes 0R by construction.
//
// FAIL-SAFE on missing candles: if a rule's newest fire can't be scored (API
// hiccup, unknown symbol), it is counted as OPEN rather than skipped. Skipping
// would UNDERCOUNT the book and let the caps under-enforce, which is the wrong
// direction to fail for a risk control — better to refuse a new entry than to
// exceed the ceiling because a fetch timed out. Realized R can only be
// undercounted in that case, so the breaker may trip late; that is logged.
// fires MUST be newest-first (autotrade.ReadFires guarantees this by reversing
// the file); the open book is "newest fire per rule", so a reversed order would
// silently score the oldest fire instead.
func BuildBook(fires []PaperFire, candles CandleFn, now time.Time) (Book, int) {
	today := now.UTC().Format("2006-01-02")

	var bk Book
	unscored := 0
	seen := map[string]bool{} // symbol|strategy → newest already counted

	for _, f := range fires {
		key := f.Symbol + "|" + f.Strategy
		newestForRule := !seen[key]

		cs := candles(f.Symbol, f.TF)
		if len(cs) == 0 {
			// Unscoreable. Only the newest fire per rule matters for the open
			// book, and for that one we assume the worst.
			if newestForRule {
				seen[key] = true
				bk.OpenCount++
				bk.OpenMargin += f.Margin
				unscored++
			}
			continue
		}

		oc := EvaluateFire(f, cs, 6)

		if newestForRule {
			seen[key] = true
			if oc.Status == OutOpen || oc.Status == OutPending {
				bk.OpenCount++
				bk.OpenMargin += f.Margin
			}
		}

		// Circuit breaker — settled outcomes dated today, across all rules.
		if oc.Status == OutTP || oc.Status == OutStop {
			if !oc.ExitAt.IsZero() && oc.ExitAt.UTC().Format("2006-01-02") == today {
				bk.RealizedRToday += oc.NetR
			}
		}
	}
	return bk, unscored
}
