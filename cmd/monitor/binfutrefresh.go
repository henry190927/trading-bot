package main

// Binance futures positioning refresher.
//
// The oi/ sampler exists because BingX publishes no OI history endpoint, so a
// series has to be accumulated by polling — and the venue republishes only
// about every ten minutes, which puts an honest floor of one hour on anything
// read from it. Binance publishes the series itself in real 5-minute buckets,
// and additionally splits long/short by top traders versus all accounts, which
// is the whale-vs-retail question measured rather than inferred.
//
// So this loop does NOT accumulate. It refreshes a cache that expires, because
// the history already exists upstream. That is the whole reason it is simpler
// than oisample.go despite covering more.
//
// Like every other data loop here, cmd/monitor is the only caller of the
// network fetch and cmd/web reads the cache — a page load must not spend an
// upstream request.

import (
	"context"
	"log"
	"time"

	"github.com/henry190927/trading-bot/binfut"
)

const binfutTick = 2 * time.Minute

func runBinFutRefresh(ctx context.Context) {
	log.Printf("binfutrefresh: up — wake every %s, fetch when older than %s, %d symbols -> %s",
		binfutTick, binfut.MinRefresh, len(binfut.Symbols), binfut.Path())

	refresh := func() {
		if !binfut.Load().Stale(time.Now().UTC()) {
			return
		}
		c, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		st, err := binfut.Fetch(c)
		if err != nil {
			// Never fatal and the old cache stays: an unreachable upstream
			// must not blank figures that were already correct. This is a
			// cross-reference, so losing it degrades a reading rather than
			// stopping anything.
			log.Printf("binfutrefresh: fetch failed, keeping cache: %v", err)
			return
		}
		if err := binfut.Save(st); err != nil {
			log.Printf("binfutrefresh: save failed: %v", err)
			return
		}
		if n := len(st.Symbols); n < len(binfut.Symbols) {
			// Partial results are kept, but a shrinking roster is worth a
			// line — a delisting or a rename shows up here first.
			log.Printf("binfutrefresh: %d/%d symbols returned data", n, len(binfut.Symbols))
		}
	}

	refresh()
	tick := time.NewTicker(binfutTick)
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
