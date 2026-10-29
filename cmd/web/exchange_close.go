package main

// Closing a journal trade from what the exchange recorded (fill-detection
// design, option A — docs/fill_detection_design.md).
//
// Nothing here writes the journal. /journal/:id/close?from=exchange uses
// exchangeCloseFor to PREFILL the ordinary close form, and the trader submits
// it. The reason is #93 (SUI, 2026-10-25): its position closed at 13:38:16
// and the journal still read "open" an hour later, because nothing in the
// system turns a vanished position into a closed row. Typing the exit from
// memory is how an R baseline drifts; the exchange already knows the price.

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
)

// exchangeClose is what the exchange's fills say about how a trade ended.
type exchangeClose struct {
	ExitPrice float64   // size-weighted average of the closing fills
	ClosedAt  time.Time // the last closing fill
	Qty       float64   // total closing quantity, excluding a recorded TP1 leg
	LegQty    float64   // recorded partial closes' fills, left out of Qty and ExitPrice
	EntryQty  float64   // the entry order's fill, else the opening fills; 0 if unknown
	PnL       float64   // realised PnL the exchange reported on the closing fills
	Fees      float64   // fees on the entry and closing fills (negative = paid)
	Fills     int       // closing fills used
	OrderIDs  []string  // their order ids, for the close notes
	Outcome   string    // a GUESS — see guessOutcome
}

// outcomeTol is how close (relative to entry) an exit must be to a planned
// level to be read as that level. Stop-market fills slip, so exact equality
// would never match; 0.2% is about one ordinary 1m bar on the majors.
const outcomeTol = 0.002

// exchangeCloseFor reduces an order history to the close of one trade.
//
// Closing fills are the opposite side of the trade (SELL closes a long), on
// the trade's position side when the exchange reports one, at or after the
// trade was opened. A TP1 fill the journal has already recorded as a leg is
// left out: the close form blends the remainder with that leg itself, and
// counting it twice would pull the exit toward TP1.
//
// ok is false when no closing fill exists — the position may still be open,
// or the history window does not reach the close.
func exchangeCloseFor(t journal.Trade, fills []bingx.FilledOrder) (ec exchangeClose, ok bool) {
	closeSide, openSide, posSide := "SELL", "BUY", "LONG"
	if t.Side == "short" {
		closeSide, openSide, posSide = "BUY", "SELL", "SHORT"
	}
	// Partial closes already recorded (the legacy TP1 leg and exits) are
	// left out of the exit: the close form blends the remainder with them.
	recorded := map[string]bool{}
	for _, l := range journal.Legs(t) {
		if l.OrderID != "" {
			recorded[l.OrderID] = true
		}
	}
	var notional, openQty float64
	for _, f := range fills {
		if f.Time.Before(t.OpenedAt) || f.AvgPrice <= 0 || f.Quantity <= 0 {
			continue
		}
		if ps := strings.ToUpper(f.PositionSide); ps != "" && ps != "BOTH" && ps != posSide {
			continue // the other leg of a hedge
		}
		switch strings.ToUpper(f.Side) {
		case openSide:
			if t.EntryOrderID != "" && f.OrderID == t.EntryOrderID {
				ec.EntryQty = f.Quantity
			}
			openQty += f.Quantity
			ec.Fees += f.FeeUSDT
		case closeSide:
			if recorded[f.OrderID] {
				ec.LegQty += f.Quantity
				continue
			}
			notional += f.AvgPrice * f.Quantity
			ec.Qty += f.Quantity
			ec.PnL += f.ProfitUSDT
			ec.Fees += f.FeeUSDT
			ec.Fills++
			ec.OrderIDs = append(ec.OrderIDs, f.OrderID)
			if f.Time.After(ec.ClosedAt) {
				ec.ClosedAt = f.Time
			}
			if ec.Outcome == "" {
				ec.Outcome = outcomeFromType(f.Type)
			}
		}
	}
	if ec.Qty <= 0 {
		return exchangeClose{}, false
	}
	if ec.EntryQty <= 0 {
		ec.EntryQty = openQty
	}
	ec.ExitPrice = notional / ec.Qty
	if ec.Outcome == "" {
		ec.Outcome = guessOutcome(t, ec.ExitPrice)
	}
	return ec, true
}

// outcomeFromType reads the outcome off the closing order's type when the
// endpoint reports one. fillHistory — the endpoint that answered for #93 —
// does not, so this is a bonus, not the main path.
func outcomeFromType(typ string) string {
	switch u := strings.ToUpper(typ); {
	case strings.Contains(u, "LIQUIDATION"):
		return "liquidated"
	case strings.HasPrefix(u, "TAKE_PROFIT"):
		return "tp2"
	case strings.HasPrefix(u, "STOP"), strings.HasPrefix(u, "TRAILING"):
		return "stop"
	}
	return ""
}

// guessOutcome names the planned level the exit sits on, or "manual".
// Only a guess: a stop that was MOVED (/today) fills nowhere near the
// journal's stop, which is the R baseline, so such a close reads "manual"
// and the trader picks "stop" by hand. The form says it is a guess.
func guessOutcome(t journal.Trade, exit float64) string {
	if t.Entry <= 0 {
		return "manual"
	}
	near := func(level float64) bool {
		return level > 0 && math.Abs(exit-level)/t.Entry <= outcomeTol
	}
	switch {
	case near(t.TP2):
		return "tp2"
	case near(t.TP1) && !t.HasTP1Leg():
		return "tp1"
	case near(t.Stop):
		return "stop"
	}
	return "manual"
}

// exchangeHistoryMax is how far back an order-history query may reach;
// BingX bounds the range (see tp1HistoryWindow).
const exchangeHistoryMax = tp1HistoryWindow

// fetchExchangeClose asks the exchange for t's fills and reduces them.
// clipped reports that the trade opened before the history window, so an
// early partial close could be missing.
func (s *server) fetchExchangeClose(ctx context.Context, t journal.Trade) (ec exchangeClose, ok, clipped bool, err error) {
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		return ec, false, false, err
	}
	end := time.Now()
	start := t.OpenedAt.Add(-time.Minute)
	if earliest := end.Add(-exchangeHistoryMax); start.Before(earliest) {
		start, clipped = earliest, true
	}
	fills, err := s.client.OrderHistory(ctx, sym, start, end)
	if err != nil {
		return ec, false, clipped, err
	}
	ec, ok = exchangeCloseFor(t, fills)
	return ec, ok, clipped, nil
}
