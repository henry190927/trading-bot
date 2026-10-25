package main

// "The journal says open, the exchange has no position" — noticed by the web
// service, so it can raise an alert and put a close-from-exchange button on
// the /today card (docs/fill_detection_design.md, option A).
//
// The monitor's bracket guard already reaches this verdict (StateStale) every
// 30s, but it only writes a log line, and it is read-only on the journal by
// design. It lives here instead because cmd/web is the one journal writer: an
// automatic close (option B) has to happen in this process, under sweepMu,
// and it will start from this same state.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
)

// goneAfter is how many consecutive reads must miss a position before it
// counts as gone. One empty read is not enough: the positions endpoint has
// returned an empty list for a live book before (see package risk's
// contradiction check), and a false "gone" would invite closing a live trade.
// At the 30s sweep cadence, two reads is 30-60 seconds.
const goneAfter = 2

// goneTrade is one open journal trade whose position the exchange no longer has.
type goneTrade struct {
	ID     int
	Symbol string
	Side   string
	Since  time.Time // the read that first missed it
}

// goneState is the tracker. known is false while the last read failed, so
// readers can tell "nothing is gone" from "could not check".
type goneState struct {
	mu     sync.Mutex
	misses map[int]int
	since  map[int]time.Time
	gone   []goneTrade
	known  bool
}

// nextMisses advances the consecutive-miss count of every active trade:
// present resets it, absent increments it. Trades no longer active drop out,
// so a closed row never stays "gone".
func nextMisses(prev map[int]int, active []int, present map[int]bool) map[int]int {
	next := make(map[int]int, len(active))
	for _, id := range active {
		if !present[id] {
			next[id] = prev[id] + 1
		}
	}
	return next
}

// positionFor finds the live position a journal trade's side would be in.
func positionFor(poss []bingx.Position, sym market.Symbol, side string) *bingx.Position {
	for i := range poss {
		if poss[i].Symbol == sym && strings.EqualFold(poss[i].Side, side) && poss[i].Quantity > 0 {
			return &poss[i]
		}
	}
	return nil
}

// refreshGone reads the journal and the account's positions once and updates
// the tracker. Read-only on both: it runs outside sweepMu.
func (s *server) refreshGone(ctx context.Context) {
	trades, err := journal.ReadAll("")
	if err != nil {
		s.markGoneUnknown()
		return
	}
	var active []journal.Trade
	for _, t := range trades {
		if t.IsActive() {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		s.setGone(nil, nil, time.Now())
		return
	}
	if s.client == nil || s.client.APIKey == "" {
		s.markGoneUnknown()
		return
	}
	poss, err := s.client.AllPositions(ctx)
	if err != nil {
		s.markGoneUnknown() // counts kept: an outage neither confirms nor clears
		return
	}
	present := map[int]bool{}
	for _, t := range active {
		sym, err := resolveWebSymbol(t.Symbol)
		if err != nil || positionFor(poss, sym, t.Side) != nil {
			present[t.ID] = true // unresolvable symbols are never called gone
		}
	}
	s.setGone(active, present, time.Now())
}

func (s *server) markGoneUnknown() {
	s.gone.mu.Lock()
	s.gone.known = false
	s.gone.mu.Unlock()
}

func (s *server) setGone(active []journal.Trade, present map[int]bool, now time.Time) {
	g := &s.gone
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]int, len(active))
	for i, t := range active {
		ids[i] = t.ID
	}
	g.misses = nextMisses(g.misses, ids, present)
	since := map[int]time.Time{}
	g.gone = g.gone[:0]
	for _, t := range active {
		n := g.misses[t.ID]
		if n == 0 {
			continue
		}
		first, ok := g.since[t.ID]
		if !ok {
			first = now
		}
		since[t.ID] = first
		if n >= goneAfter {
			g.gone = append(g.gone, goneTrade{ID: t.ID, Symbol: t.Symbol, Side: t.Side, Since: first})
		}
	}
	sort.Slice(g.gone, func(i, j int) bool { return g.gone[i].ID < g.gone[j].ID })
	g.since = since
	g.known = true
}

// goneSnapshot returns the trades currently judged gone and whether the last
// read succeeded.
func (s *server) goneSnapshot() ([]goneTrade, bool) {
	s.gone.mu.Lock()
	defer s.gone.mu.Unlock()
	return append([]goneTrade(nil), s.gone.gone...), s.gone.known
}
