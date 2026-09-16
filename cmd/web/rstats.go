package main

import (
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/journal"
)

// Shared input for the three portfolio views (equity curve, R histogram,
// daily calendar).
//
// Those builders were written against []journal.Trade. The auto-executor's
// paper record needs the SAME three views, and the tempting move — a parallel
// set of builders over autotrade.Position — would have been two
// implementations of "cumulative R over time" free to drift apart. They now
// take []rTrade, and each source gets a thin adapter instead.
type rTrade struct {
	R      float64   // realized R
	Closed time.Time // when it settled; equity moves on CLOSE, not on entry
}

// rTradesFromJournal keeps the original filter — open trades and no-fills
// never moved the curve, because neither realizes R — and adds trades whose R
// is UNDEFINED for want of a stop. Those have a real P&L but no risk to
// measure it against, and journal.RealizedR returns 0 for them; charting that
// zero would draw a flat step on the equity curve and a break-even bar in the
// histogram for a trade that was neither.
func rTradesFromJournal(trades []journal.Trade) []rTrade {
	out := make([]rTrade, 0, len(trades))
	for _, t := range trades {
		if t.IsOpen() || t.ClosedAt.IsZero() || t.IsNoFill() || !t.HasR() {
			continue
		}
		out = append(out, rTrade{R: t.RRealized, Closed: t.ClosedAt})
	}
	return out
}

// rTradesFromPositions adapts DEDUPED auto positions — the same unit the
// circuit breaker counts. Raw fires would draw a curve of trades that were
// never taken: a persistent setup re-firing every bar is ONE position.
//
// unscoreable counts settled positions whose exit time is unknown because the
// kline window no longer reaches them. Returned rather than dropped silently,
// so a truncated curve can be labelled as truncated.
func rTradesFromPositions(ps []autotrade.Position) (out []rTrade, unscoreable int) {
	for _, p := range ps {
		if p.Outcome.Status != autotrade.OutTP && p.Outcome.Status != autotrade.OutStop {
			continue
		}
		if p.Outcome.ExitAt.IsZero() {
			unscoreable++
			continue
		}
		out = append(out, rTrade{R: p.Outcome.NetR, Closed: p.Outcome.ExitAt})
	}
	return out, unscoreable
}

// openUnrealR sums floating R across the auto positions still running, for
// the "mark-to-market" figure shown beside the realized curve.
func openUnrealR(ps []autotrade.Position) (r float64, n int) {
	for _, p := range ps {
		if p.Outcome.Status == autotrade.OutOpen {
			r += p.Outcome.UnrealR
			n++
		}
	}
	return r, n
}
