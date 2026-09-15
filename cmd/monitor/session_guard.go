package main

// Cash-open stop guard for the auto executor.
//
// Two of the three SNDK entries this has taken died the same way. Both were
// placed within seconds of 9:30 ET with a stop tighter than the cash-open
// bar's ordinary travel — one of them at 0.5065%, a seventh of the 3.75%
// median that session.MedianOpenBarRangePct measures over 148 days — and both
// were stopped out inside half a minute, one in twenty-four seconds.
//
// session.StopWarning has been able to say this all along. It just was not
// wired anywhere the auto executor could hear it: a grep for `session.` across
// cmd/monitor and autostrat returned nothing, so the guard lived only in
// cmd/validate and the web order flow. Both of those are paths a human walks;
// neither is the one the executor takes. Meanwhile SNDK's live rule carries
// stop_pct 0.5, which is exactly the geometry that failed.
//
// Scope, stated because the guard is narrower than the problem: it gates the
// ENTRY, and only for a fire landing within one bar of the open. A position
// opened earlier in the day and still held when the bell rings is exposed to
// the same bar and this does nothing about it — that is position management,
// not entry gating, and it needs a different mechanism.
//
// Fails OPEN. If the history fetch errors the fire proceeds, because today's
// behaviour is no guard at all and an API hiccup must not become a silent
// trading halt on five symbols. The skip is logged rather than swallowed.

import (
	"context"
	"log"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/session"
)

// autoSessionBlock returns a non-empty reason when this fire should not be
// placed because its stop sits inside the cash-open bar's ordinary range.
//
// The horizon is the rule's own bar duration, which is the granularity at
// which it could next react: if the open falls between this bar's close and
// the next one, the position is carried through that bar with no opportunity
// to re-evaluate. Wider would suppress fires that do get a look before the
// bell; narrower would miss the run-up entirely.
func autoSessionBlock(
	ctx context.Context,
	client *bingx.Client,
	sym market.Symbol,
	tf string,
	trig autostrat.Trigger,
	now time.Time,
) string {
	untilOpen, applies := autoSessionApplies(sym, tf, trig, now)
	if !applies {
		return ""
	}

	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cs, err := client.Klines(c, sym, market.TF1h, session.OpenBarLookbackBars)
	if err != nil {
		log.Printf("autoexec: session guard skipped for %s — klines: %v", sym, err)
		return ""
	}
	med, n := session.MedianOpenBarRangePct(cs)
	// StopWarning returns "" when the stop already clears the median, or when
	// there are fewer than session.MinSamples open bars to form one. Both are
	// correct passes: the second means the history cannot support the claim,
	// and inventing a median from three samples would block on noise.
	return session.StopWarning(trig.Entry, trig.Stop, med, n, untilOpen)
}

// autoSessionApplies decides whether this fire is even in scope, and returns
// how long until the open so StopWarning can name it.
//
// Split out from autoSessionBlock so the gating is testable without a network
// client: every "not in scope" answer is reached before any history fetch, and
// those branches are where the guard's blast radius is decided.
func autoSessionApplies(
	sym market.Symbol,
	tf string,
	trig autostrat.Trigger,
	now time.Time,
) (untilOpen time.Duration, applies bool) {
	if !sym.IsUSStock() || trig.Entry <= 0 || trig.Stop <= 0 {
		return 0, false
	}
	horizon := market.BarDuration(market.Timeframe(tf))
	if horizon <= 0 {
		// An unrecognised timeframe has no "next chance to react", so there
		// is no defensible horizon. Pass rather than invent one.
		return 0, false
	}
	untilOpen = session.NextCashOpen(now).Sub(now)
	if untilOpen <= 0 || untilOpen > horizon {
		return untilOpen, false
	}
	return untilOpen, true
}
