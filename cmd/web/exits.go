package main

// Recording partial closes (step b of docs/position_orders_design.md).
//
// Take-profits are now added on the exchange after the position opens, any
// number of them, so the journal can no longer know in advance which order
// is "TP1". It learns afterwards instead: every 30s, for each open trade
// whose live position is smaller than what the journal accounts for, the
// sweep reads the order history and records each new partial close in
// Trade.Exits. The final close is NOT an exit — it goes through the close
// form (option A) — which is why newExitsFrom stops before the fill that
// would take the position to zero.

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
)

// newExitsFrom picks, from an order history, the partial closes of t that
// are not recorded yet.
//
// Contract (exits_test.go is the definition of done):
//   - A partial close is a fill on the CLOSING side (SELL for a long, BUY for
//     a short), with Quantity > 0 and AvgPrice > 0, at or after the time the
//     trade filled (t.FilledAt; t.OpenedAt when FilledAt is zero).
//   - When the fill reports a position side other than "" or "BOTH", it must
//     be the trade's (LONG for a long, SHORT for a short) — the other side of
//     a hedge is a different position.
//   - A fill whose Status is set and is not "FILLED" is still moving; skip it.
//   - Skip any fill whose OrderID is already one of journal.Legs(t) — that
//     covers both earlier exits and the legacy TP1 leg.
//   - Walk the remaining fills in TIME order. Start the running closed
//     quantity at ClosedFrac(t) × entryQty. A fill that would bring it to
//     99.9% of entryQty or more is the FINAL close: record nothing from it
//     onward.
//   - Each recorded fill becomes Exit{Price: AvgPrice, Qty: Quantity,
//     Pct: Quantity / entryQty × 100, At: Time, OrderID, Src: "exchange"}.
//   - entryQty <= 0 means the denominator is unknown: return nil.
//   - Do not modify fills (the caller may reuse the slice).
func newExitsFrom(t journal.Trade, fills []bingx.FilledOrder, entryQty float64) []journal.Exit {
	if entryQty <= 0 {
		return nil
	}

	slice := make([]bingx.FilledOrder, 0, len(fills))

	// 平倉方向, 倉位方向 init based on t.Side is "long"
	closeSide, positionSide := "SELL", "LONG"
	if t.Side == "short" {
		closeSide, positionSide = "BUY", "SHORT"
	}

	since := t.FilledAt
	if since.IsZero() {
		since = t.OpenedAt
	}

	seen := make(map[string]struct{})
	for _, exit := range journal.Legs(t) {
		if exit.OrderID != "" {
			seen[exit.OrderID] = struct{}{}
		}
	}

	for _, fill := range fills {
		if fill.Side != closeSide {
			continue
		}
		if fill.Quantity <= 0 || fill.AvgPrice <= 0 {
			continue
		}
		if fill.Time.Before(since) {
			continue
		}
		if fill.Status != "" && fill.Status != "FILLED" {
			continue
		}
		if fill.PositionSide != "" && fill.PositionSide != "BOTH" && fill.PositionSide != positionSide {
			continue
		}
		if _, ok := seen[fill.OrderID]; ok {
			continue
		}

		slice = append(slice, fill)
	}

	slices.SortFunc(slice, func(i, j bingx.FilledOrder) int {
		return i.Time.Compare(j.Time)
	})

	// 沒有結果就回傳 nil
	var exits []journal.Exit
	closed := journal.ClosedFrac(t) * entryQty

	for _, fill := range slice {
		if closed+fill.Quantity >= 0.999*entryQty {
			break
		}
		exits = append(exits, journal.Exit{Price: fill.AvgPrice, Qty: fill.Quantity, Pct: fill.Quantity / entryQty * 100, At: fill.Time, OrderID: fill.OrderID, Src: "exchange"})
		closed += fill.Quantity
	}

	return exits
}

// plannedQty is the entry size placeEntryOnBingX sent: margin × leverage /
// entry, floored to the lot. 0 when the row does not record its sizing.
func plannedQty(t journal.Trade) float64 {
	if t.MarginUSDT <= 0 || t.Leverage <= 0 || t.Entry <= 0 {
		return 0
	}
	return floorTo(t.MarginUSDT*float64(t.Leverage)/t.Entry, lotPrec(t.Symbol))
}

// entryQtyFrom prefers the entry order's own fill, then the planned size.
func entryQtyFrom(t journal.Trade, fills []bingx.FilledOrder) float64 {
	if t.EntryOrderID != "" {
		for _, f := range fills {
			if f.OrderID == t.EntryOrderID && f.Quantity > 0 {
				return f.Quantity
			}
		}
	}
	return plannedQty(t)
}

// sweepExits records new partial closes for every active trade. It reads
// positions first and asks for order history only for trades whose position
// is smaller than the journal expects, so a quiet book costs one call.
// Serialised with the other journal writers by sweepMu; the write re-reads
// and merges by trade ID, like sweepOpenTrades.
func (s *server) sweepExits(ctx context.Context) {
	if s.client == nil || s.client.APIKey == "" {
		return
	}
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	trades, err := journal.ReadAll("")
	if err != nil {
		return
	}
	var active []int
	for i, t := range trades {
		if t.IsActive() {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		return
	}
	poss, err := s.client.AllPositions(ctx)
	if err != nil {
		return
	}
	bySym := map[market.Symbol][]int{}
	for _, i := range active {
		t := trades[i]
		sym, err := resolveWebSymbol(t.Symbol)
		if err != nil {
			continue
		}
		pos := positionFor(poss, sym, t.Side)
		if pos == nil {
			continue // gone: the close form's job, not a partial
		}
		if q := plannedQty(t); q > 0 && pos.Quantity >= q*(1-journal.ClosedFrac(t))*0.999 {
			continue // nothing closed beyond what is recorded
		}
		bySym[sym] = append(bySym[sym], i)
	}
	added := map[int][]journal.Exit{}
	now := time.Now()
	for sym, idxs := range bySym {
		start := now.Add(-exchangeHistoryMax)
		for _, i := range idxs {
			if o := trades[i].OpenedAt.Add(-time.Minute); o.After(start) {
				start = o
			}
		}
		fills, err := s.client.OrderHistory(ctx, sym, start, now)
		if err != nil {
			continue
		}
		for _, i := range idxs {
			t := trades[i]
			if ex := newExitsFrom(t, fills, entryQtyFrom(t, fills)); len(ex) > 0 {
				added[t.ID] = ex
			}
		}
	}
	if len(added) == 0 {
		return
	}
	fresh, err := journal.ReadAll("")
	if err != nil {
		return
	}
	if mergeExits(fresh, added) {
		if err := journal.WriteAll("", fresh); err != nil {
			log.Printf("exits: journal write failed: %v", err)
		}
	}
}

// mergeExits appends added exits to the matching rows of fresh, skipping an
// order id the row already holds (a concurrent edit may have recorded it)
// and rows that closed meanwhile. Reports whether anything changed.
func mergeExits(fresh []journal.Trade, added map[int][]journal.Exit) bool {
	changed := false
	for i := range fresh {
		ex, ok := added[fresh[i].ID]
		if !ok || !fresh[i].IsOpen() {
			continue
		}
		have := map[string]bool{}
		for _, l := range journal.Legs(fresh[i]) {
			have[l.OrderID] = true
		}
		for _, e := range ex {
			if e.OrderID != "" && have[e.OrderID] {
				continue
			}
			fresh[i].Exits = append(fresh[i].Exits, e)
			log.Printf("exits: #%d %s closed %.4g%% @ %g (order %s)", fresh[i].ID, fresh[i].Symbol, e.Pct, e.Price, e.OrderID)
			changed = true
		}
	}
	return changed
}
