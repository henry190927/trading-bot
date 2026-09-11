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

	"myFirstGo/trading-bot/bls"
)

// blsRefreshTick is how often the loop WAKES. Whether it actually fetches is
// bls.Store.Stale's call, so the interval can be short without costing
// queries — which matters on release mornings, where the difference between
// picking up a number at 20:35 and at 02:00 the next day is the whole point.
const blsRefreshTick = 30 * time.Minute

func runBLSRefresh(ctx context.Context) {
	log.Printf("blsrefresh: up — wake every %s, fetch when older than %s -> %s",
		blsRefreshTick, bls.MinRefresh, bls.Path())

	refresh := func() {
		cur := bls.Load()
		if !cur.Stale(time.Now().UTC()) {
			return
		}
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
