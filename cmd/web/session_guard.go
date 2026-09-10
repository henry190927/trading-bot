package main

// Cash-open-bar stop guard for the US-stock synthetics.
//
// The measurement behind it lives in package session; the short version is
// that one bar a day (the one holding 9:30 ET) carries 2.5-7.25x these
// symbols' median hourly range and sets the day's extreme 25-58% of the time.
// A stop inside that bar's ordinary travel is reached by noise, not by being
// wrong — which is what happened to SNDK #62 on 2026-09-03.
//
// Deliberately ADVISORY. It rides along with the placement result and shows on
// /ops/verify; it never refuses an order. A stop tighter than the open bar is
// a fine choice for a position that will be flat before the open, and the last
// guard that tried to encode intent (stop-vs-entry) forbade every legitimate
// trailing stop. This one states a fact and lets the desk decide.

import (
	"context"
	"time"

	"myFirstGo/trading-bot/earnings"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/session"
)

// The median moves at most once a day, so an hour of staleness is free and
// keeps /ops/verify from re-fetching 400 candles per row.
const openBarCacheTTL = time.Hour

type openBarEntry struct {
	pct float64
	n   int
	at  time.Time
}

// sessionGuardApplies reports whether the cash-open guard is in scope for sym.
//
// Stock synthetics ONLY, and the exclusion of crypto is deliberate rather than
// an oversight. BTC's busiest hour is also the US cash open (7.2x hour-of-day
// swing in relative volume, measured 2026-09-04), so the session effect is
// real there too — but BTC trades continuously, which makes both halves of
// this guard's advice wrong for it: there is no "before the open" to be flat
// by, and a 0.81% median for that hour is unremarkable next to BTC's daily
// range. Widening the scope needs its own measurement, not this one's.
func sessionGuardApplies(sym market.Symbol) bool {
	return earnings.IsStockSymbol(string(sym))
}

// openBarBaseline returns the cash-open-bar median range (percent of open) and
// its sample count for a stock synthetic, or (0, 0) for anything else —
// including on a fetch error, because a missing baseline must read as "no
// opinion", never as "no risk".
func (s *server) openBarBaseline(ctx context.Context, sym market.Symbol) (float64, int) {
	if s.client == nil || !sessionGuardApplies(sym) {
		return 0, 0
	}

	s.openBarMu.Lock()
	if e, ok := s.openBar[sym]; ok && time.Since(e.at) < openBarCacheTTL {
		s.openBarMu.Unlock()
		return e.pct, e.n
	}
	s.openBarMu.Unlock()

	cs, err := s.client.Klines(ctx, sym, market.TF1h, session.OpenBarLookbackBars)
	if err != nil || len(cs) == 0 {
		return 0, 0
	}
	pct, n := session.MedianOpenBarRangePct(cs)

	s.openBarMu.Lock()
	if s.openBar == nil {
		s.openBar = map[market.Symbol]openBarEntry{}
	}
	s.openBar[sym] = openBarEntry{pct: pct, n: n, at: time.Now()}
	s.openBarMu.Unlock()

	return pct, n
}

// sessionStopWarning renders the advisory for one trade, or "" when there is
// nothing to say (not a stock, no stop, thin history, or a stop already wider
// than the open bar).
func (s *server) sessionStopWarning(ctx context.Context, sym market.Symbol, entry, stop float64, now time.Time) string {
	pct, n := s.openBarBaseline(ctx, sym)
	if pct <= 0 {
		return ""
	}
	return session.StopWarning(entry, stop, pct, n, session.NextCashOpen(now).Sub(now))
}
