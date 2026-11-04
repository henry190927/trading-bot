package main

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// histCache keeps the long candle history /ops/autotrade replays on, per
// "SYM|TF", across requests. Fetching it whole is ~1.7k bars per key and
// ~10s for the page; after the first load only the newest 500 bars are
// fetched and spliced on (mergeCandles). The zero value is ready to use.
//
// mu is held across the network fetch on purpose: two panels loading at
// once would otherwise both pull the full history and double the rate-limit
// pressure that made the fetch fail in the first place.
type histCache struct {
	mu sync.Mutex
	m  map[string][]market.Candle
}

// klinesSince returns candles reaching back to from: the usual 500 bars when
// they cover it, else the whole range — from the cache when it already
// reaches back that far, else paged with KlinesRange.
func (s *server) klinesSince(ctx context.Context, sym, tf string, from time.Time) ([]market.Candle, error) {
	dur := market.BarDuration(market.Timeframe(tf))
	if from.IsZero() || dur <= 0 || time.Since(from) < 480*dur {
		return klinesWithRetry(ctx, s.client, sym, tf, 500)
	}
	start := from.Add(-2 * dur)
	key := sym + "|" + tf

	h := &s.hist
	h.mu.Lock()
	defer h.mu.Unlock()

	// Covered = the first bar opened at or before the oldest fire (as in
	// autotrade.coversFire). Not `start`: that is from-2 bars and rarely on a
	// bar boundary, so the first fetched bar always opens after it and the
	// cache would never hit.
	if cached := h.m[key]; len(cached) > 0 && !cached[0].OpenTime.After(from) {
		recent, err := klinesWithRetry(ctx, s.client, sym, tf, 500)
		if err != nil {
			// Stale but whole: the page shows the fetch error, and only the
			// bars since the last load are missing.
			return cached, err
		}
		if merged, ok := mergeCandles(cached, recent, dur); ok {
			h.m[key] = merged
			return merged, nil
		}
		// A gap between the cache and the newest 500 bars (the page was not
		// opened for weeks): fetch the whole range again.
	}

	cs, err := fullRangeWithRetry(ctx, s, sym, tf, start)
	if err != nil {
		return nil, err
	}
	if h.m == nil {
		h.m = map[string][]market.Candle{}
	}
	h.m[key] = cs
	return cs, nil
}

func fullRangeWithRetry(ctx context.Context, s *server, sym, tf string, start time.Time) ([]market.Candle, error) {
	ms, err := resolveWebSymbol(sym)
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		cs, err := s.client.KlinesRange(ctx, ms, market.Timeframe(tf), start, time.Now())
		if err == nil && len(cs) > 0 {
			return cs, nil
		}
		if err == nil {
			err = errors.New("empty response")
		}
		last = err
	}
	return nil, last
}

// mergeCandles splices a fresh fetch of the newest bars onto the cached
// history.
//
// Contract:
//   - cached and recent are both non-empty and ascending by OpenTime, one bar
//     per dur.
//   - ok=false when they do not meet: recent[0] opened more than one bar
//     (dur) after the last cached bar. Exactly one bar after is fine.
//   - Otherwise merged = the cached bars that opened BEFORE recent[0],
//     followed by all of recent. Where the two overlap, recent wins: the last
//     cached bar was usually still forming when it was fetched.
//   - cached must not be modified — another request may still be replaying
//     on it. (Mind what append does when the slice has spare capacity.)
func mergeCandles(cached, recent []market.Candle, dur time.Duration) (merged []market.Candle, ok bool) {
	if recent[0].OpenTime.Sub(cached[len(cached)-1].OpenTime) > dur {
		return nil, false
	}
	// How many cached bars opened before recent[0]. Ascending, so the first
	// one that does not is where recent takes over.
	keep := sort.Search(len(cached), func(i int) bool {
		return !cached[i].OpenTime.Before(recent[0].OpenTime)
	})
	merged = make([]market.Candle, 0, keep+len(recent))
	merged = append(merged, cached[:keep]...)
	merged = append(merged, recent...)
	return merged, true
}
