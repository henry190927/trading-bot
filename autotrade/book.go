package autotrade

import (
	"time"

	"github.com/henry190927/trading-bot/market"
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
// RealizedRToday: from DEDUPED POSITIONS, not raw fires. A rule whose setup
// persists re-fires every bar while its position is open, so ONE trade can leave
// several fires in the log (verified: BTC range-edge 09-01 16:00→17:00, ETH
// htf-snr 08-30 17:00→18:00, SUI sweep-reject 08-28 01:00→03:00). Counting each
// one's -1R would trip the breaker EARLIER than its configured threshold — the
// dangerous direction for a control the trader relies on. DedupFires collapses
// them the same way the panel's position list does.
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
// FireTrace records one fire's contribution to the book. Diagnostics only —
// the book itself is just the aggregate. Exists because a realized-R value was
// observed moving TOWARD zero between ticks, which for a circuit breaker is the
// dangerous direction (a tripped halt could un-trip within the day), and the
// aggregate alone can't say which fire moved.
type FireTrace struct {
	Symbol, Strategy, TF string
	FireTime             time.Time
	Status               OutcomeStatus
	ExitAt               time.Time
	NetR                 float64
	Bars                 int // candles supplied for this fire
	NewestForRule        bool
	CountedOpen          bool
	CountedToday         bool
	Unscoreable          bool
}

// BuildBook is the aggregate-only form; see BuildBookTraced.
func BuildBook(fires []PaperFire, candles CandleFn, now time.Time, cooldownBars int) (Book, int) {
	bk, un, _ := BuildBookTraced(fires, candles, now, cooldownBars)
	return bk, un
}

// BuildBookTraced is BuildBook plus a per-fire trace, in input order.
func BuildBookTraced(fires []PaperFire, candles CandleFn, now time.Time, cooldownBars int) (Book, int, []FireTrace) {
	today := now.UTC().Format("2006-01-02")

	var bk Book
	unscored := 0
	seen := map[string]bool{} // symbol|strategy → newest already counted
	trace := make([]FireTrace, 0, len(fires))

	for _, f := range fires {
		key := f.Symbol + "|" + f.Strategy
		newestForRule := !seen[key]
		tr := FireTrace{Symbol: f.Symbol, Strategy: f.Strategy, TF: f.TF,
			FireTime: f.Time, NewestForRule: newestForRule}

		cs := candles(f.Symbol, f.TF)
		tr.Bars = len(cs)
		if len(cs) == 0 {
			// Unscoreable. Only the newest fire per rule matters for the open
			// book, and for that one we assume the worst.
			tr.Unscoreable = true
			if newestForRule {
				seen[key] = true
				bk.OpenCount++
				bk.OpenMargin += f.Margin
				// No Side recorded: an unscoreable fire holds its slot but its
				// direction is not trustworthy, so it must not match a
				// same-symbol-side candidate. See Book.OpenLegs.
				bk.OpenLegs = append(bk.OpenLegs, Leg{Symbol: f.Symbol})
				unscored++
				tr.CountedOpen = true
			}
			trace = append(trace, tr)
			continue
		}

		oc := EvaluateFire(f, cs, 6)
		tr.Status, tr.ExitAt, tr.NetR = oc.Status, oc.ExitAt, oc.NetR

		if newestForRule {
			seen[key] = true
			if oc.Status == OutOpen || oc.Status == OutPending {
				bk.OpenCount++
				bk.OpenMargin += f.Margin
				bk.OpenLegs = append(bk.OpenLegs, Leg{Symbol: f.Symbol, Side: f.Side})
				tr.CountedOpen = true
			}
		}

		trace = append(trace, tr)
	}

	// --- circuit breaker input: deduped positions settled today ---
	// DedupFires needs oldest-first, so walk the newest-first input backwards.
	oldest := make([]PaperFire, 0, len(fires))
	for i := len(fires) - 1; i >= 0; i-- {
		oldest = append(oldest, fires[i])
	}
	resolve := func(f PaperFire) Outcome {
		cs := candles(f.Symbol, f.TF)
		if len(cs) == 0 {
			return Outcome{Status: OutOpen} // unscoreable → holds its slot, books no R
		}
		return EvaluateFire(f, cs, 6)
	}
	// barDur is 1h for all rules, matching the panel's convention. Only XAG
	// runs on 2h, where this makes the post-stop absorb window half as long —
	// conservative for the breaker (fewer absorbed = more R counted), so it
	// errs toward tripping rather than toward missing a bad day.
	counted := map[string]bool{}
	for _, pos := range DedupFires(oldest, 6, cooldownBars, time.Hour, resolve) {
		oc := pos.Outcome
		if oc.Status != OutTP && oc.Status != OutStop {
			continue
		}
		if oc.ExitAt.IsZero() || oc.ExitAt.UTC().Format("2006-01-02") != today {
			continue
		}
		bk.RealizedRToday += oc.NetR
		counted[pos.Fire.Symbol+"|"+pos.Fire.Strategy+"|"+pos.Fire.Time.String()] = true
	}
	// Mark the trace so diagnostics show which fires actually fed the breaker.
	for i := range trace {
		k := trace[i].Symbol + "|" + trace[i].Strategy + "|" + trace[i].FireTime.String()
		trace[i].CountedToday = counted[k]
	}
	return bk, unscored, trace
}
