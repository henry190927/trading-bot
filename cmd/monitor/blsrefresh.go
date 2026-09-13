package main

// BLS cache refresher.
//
// The calendar had forecasts and no actuals: the ForexFactory feed it reads
// carries an `actual` field that is empty on every row, released or not. So
// on 2026-09-10, when a PPI print moved BTC 1,290 points in one hour, the only
// way to ask "did it come in hot?" was to infer it from the price — and that
// inference was wrong. Headline PPI final demand printed +0.40% m/m against a
// +0.4% forecast. Exactly in line.
//
// package bls reads the released figures from the agency that publishes them.
// This is the only thing allowed to call it over the network, for one reason:
// the keyless v1 API allows 25 QUERIES PER DAY. A fetch from a web handler
// would spend one of those per page load and exhaust the budget in a minute,
// so cmd/web only ever reads the cache this writes.
//
// One query per refresh, because v1 takes up to 25 series in a single POST.
// At MinRefresh (6h) that is 4 of the 25 per day, leaving the rest for manual
// runs.

import (
	"context"
	"log"
	"time"

	"github.com/henry190927/trading-bot/bls"
)

// blsRefreshTick is how often the loop WAKES. Whether it actually fetches is
// bls.Store.Stale's call, so the interval can be short without costing
// queries — which matters on release mornings, where the difference between
// picking up a number at 20:35 and at 02:00 the next day is the whole point.
const blsRefreshTick = 30 * time.Minute

// budgetWarned keeps the exhausted-budget line to once per exhaustion rather
// than once per 30-minute wake.
var budgetWarned bool

func runBLSRefresh(ctx context.Context) {
	log.Printf("blsrefresh: up — wake every %s, fetch when older than %s, budget %d/day (cap %d) -> %s",
		blsRefreshTick, bls.MinRefresh, bls.DailyBudget, bls.DailyCap, bls.Path())

	refresh := func() {
		now := time.Now().UTC()
		cur := bls.Load()
		if !cur.Stale(now) {
			return
		}
		// Budget before staleness matters: at MinRefresh 1h the loop wants 24
		// queries a day against a cap of 25, so a single extra caller would
		// push it over and the API would start refusing. Exceeding fails as a
		// silently frozen cache, which is the worst shape for this data — stop
		// short instead, and say so once rather than every wake.
		if left := cur.BudgetLeft(now); left <= 0 {
			if !budgetWarned {
				budgetWarned = true
				log.Printf("blsrefresh: daily budget spent (%d/%d) — holding the cache until UTC midnight",
					cur.QueryCount, bls.DailyBudget)
			}
			return
		}
		budgetWarned = false

		c, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		st, err := bls.Fetch(c)
		if err != nil {
			// Logged, never fatal, and the old cache is left in place: a
			// throttled or unreachable API must not blank out numbers that
			// were already correct.
			log.Printf("blsrefresh: fetch failed, keeping cache: %v", err)
			return
		}
		// Carry the meter forward: Fetch returns a fresh Store that knows
		// nothing about today's spend.
		st.QueryDay, st.QueryCount = cur.SpendQuery(now)
		if err := bls.Save(st); err != nil {
			log.Printf("blsrefresh: save failed: %v", err)
			return
		}
		// Report what moved. A monthly series that did not advance is the
		// normal case; the line worth seeing is the one where it did.
		for id, label := range bls.Series {
			now, ok := st.Latest(id)
			if !ok {
				continue
			}
			was, hadBefore := cur.Latest(id)
			if !hadBefore || was.Year != now.Year || was.Month != now.Month {
				log.Printf("blsrefresh: %s now has %d-%02d (%s)", id, now.Year, now.Month, label)
			}
		}
	}

	refresh()
	tick := time.NewTicker(blsRefreshTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			refresh()
		}
	}
}
