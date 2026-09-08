// Package bracket decides whether an open journal trade is actually protected
// on the exchange, and what to do when it is not.
//
// # WHY THIS IS A PACKAGE AND NOT A HANDLER
//
// The existing auto-stop placement lives inline in cmd/web's
// buildOpenTradeCards, whose only callers are the dashboard and /today
// handlers. It therefore runs when — and only when — a browser loads a page.
// That is the mechanism behind every naked position in the journal: #60 went
// naked entirely on 2026-08-31, and #62/#63/#64 carried a planned stop that
// never reached BingX because the fills landed while nobody was at a desk.
// A protective order that depends on a human refreshing a tab is not a
// protective order.
//
// It also never re-checked. cmd/web skips placement once StopOrderID is
// non-empty, so a stop that was placed and then cancelled, expired or
// silently rejected leaves the journal reading "protected" forever. #60's
// bundled stop failed to attach exactly this way.
//
// THREE INVARIANTS
//
//  1. THE EXCHANGE IS THE STATE. Decide is given the live position and the
//     live resting orders and trusts nothing else. It does not consult
//     FilledAt (a position that exists has filled, whatever the CSV says) and
//     it does not consult StopOrderID to conclude "protected" — only a stop
//     the exchange currently reports counts.
//
//  2. READ-ONLY ON THE JOURNAL. Decide returns a decision; the daemon never
//     writes journal.csv. cmd/web writes it on every dashboard render, and two
//     processes doing read-modify-write on one CSV is how a row disappears.
//     Because state is read from the exchange each cycle, no local bookkeeping
//     is needed: after a stop is placed, the next cycle simply sees it.
//
//  3. FALSE-NAKED IS CHEAP, FALSE-PROTECTED IS NOT. Every ambiguity resolves
//     toward "naked". A duplicate reduce-only stop is harmless — BingX will
//     not over-close — while one missed naked position is the whole reason
//     this package exists.
//
// What counts as protection is deliberately loose on PRICE and strict on
// EXISTENCE: any reduce-only stop on the closing side protects the position,
// whatever its trigger. A trailing stop or a profit-lock is still a stop, and
// an earlier price-matching check in cmd/protect forbade every one of them
// (see feedback_order_guard_vs_mark).
package bracket

import (
	"fmt"
	"strings"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/journal"
	"myFirstGo/trading-bot/market"
)

// State is what the exchange says about one open journal trade.
type State string

const (
	// StateSkip — the trade is not a candidate: closed, marked no-fill, or
	// carrying no planned stop to place or compare against.
	StateSkip State = "skip"
	// StateWaiting — journal says open, no position yet. The entry limit has
	// not filled. Nothing to protect and nothing wrong.
	StateWaiting State = "waiting"
	// StateProtected — position live, a reduce-only stop is live with it.
	StateProtected State = "protected"
	// StateNaked — position live, NO reduce-only stop on the exchange. This
	// is the alarm.
	StateNaked State = "naked"
	// StateStale — journal says open, exchange has no position, and the trade
	// was previously seen filled. The position is gone and the CSV has not
	// caught up. Never place anything here.
	StateStale State = "stale"
)

// Decision is the result for one trade. Act is what the daemon should send;
// everything else is for the log and the push.
type Decision struct {
	TradeID int
	Short   string // journal's name, e.g. "BTC"
	Symbol  market.Symbol
	Side    string // "long" | "short"
	State   State
	Reason  string

	// PlannedStop is the trade's journal stop — the R baseline. Reported for
	// the alert even when nothing is placed, because "you are naked and this
	// is where you said the stop was" is the useful message.
	PlannedStop float64
	// PlaceStop is > 0 only when the daemon should send a reduce-only
	// STOP_MARKET at this trigger. Zero on every alert-only path.
	PlaceStop float64
	// LiveStopID / LiveStopPrice describe the protection actually found.
	LiveStopID    string
	LiveStopPrice float64
	// GhostStopID is set when journal.StopOrderID names an order the exchange
	// does not have resting. The journal reads protected and is not.
	GhostStopID string
}

// Naked reports whether this decision describes an unprotected live position.
func (d Decision) Naked() bool { return d.State == StateNaked }

// Mode controls whether Decide is allowed to propose a placement at all.
type Mode string

const (
	// ModeAlert never proposes a placement. It reports naked positions and
	// leaves the sending to a human via /ops/verify.
	//
	// This is the default, and the reason is not timidity. journal.Stop is
	// the ANALYSIS stop — the R baseline — and the trader keeps it
	// deliberately distinct from whatever price is actually resting on the
	// exchange (see feedback_journal_stop_two_meanings; #61 recorded an
	// analysis stop of 77,380 while a 77,900 profit-lock rested live).
	// Auto-placing the analysis stop would collapse two numbers that were
	// separated on purpose.
	ModeAlert Mode = "alert"
	// ModePlace proposes placing the journal stop on any naked position.
	// Opt-in, for a trader who wants the analysis stop to also be the live
	// order.
	ModePlace Mode = "place"
)

// Decide inspects one journal trade against the exchange.
//
// pos is the live position for the trade's symbol and side, or nil if there is
// none. resting is every open order the exchange reports for that symbol.
//
// A trade whose StopAuto flag is set proposes a placement in EITHER mode: that
// flag has always meant "place this for me", and honouring it only when a
// browser happens to be open is the delivery bug, not a policy choice.
func Decide(t journal.Trade, pos *bingx.Position, resting []bingx.OpenOrder, mode Mode) Decision {
	d := Decision{
		TradeID: t.ID, Short: t.Symbol, Side: t.Side,
		PlannedStop: t.Stop,
	}
	if sym, ok := market.Resolve(t.Symbol); ok {
		d.Symbol = sym
	}

	switch {
	case !t.IsOpen():
		d.State, d.Reason = StateSkip, "trade is closed"
		return d
	case t.IsNoFill():
		d.State, d.Reason = StateSkip, "trade is marked no-fill"
		return d
	case t.Side != "long" && t.Side != "short":
		d.State, d.Reason = StateSkip, fmt.Sprintf("unexpected side %q", t.Side)
		return d
	case d.Symbol == "":
		d.State, d.Reason = StateSkip, fmt.Sprintf("unknown symbol %q", t.Symbol)
		return d
	case t.Stop <= 0:
		// No planned stop means there is no intent to compare against. A
		// position with no stop anywhere is still worth flagging, but this
		// package cannot say what the stop should be and will not invent one.
		d.State, d.Reason = StateSkip, "no stop recorded in the journal"
		return d
	}

	if pos == nil {
		// FilledAt is cmd/web's bookkeeping and can be stale in both
		// directions, so it is used only to tell "never filled" from
		// "filled and since closed" — never to decide protection.
		if t.FilledAt.IsZero() {
			d.State, d.Reason = StateWaiting, "entry has not filled — no position on the exchange"
		} else {
			d.State, d.Reason = StateStale, "journal says open but the exchange has no position"
		}
		return d
	}

	stop, found := FindProtectiveStop(resting, pos)
	if found {
		d.LiveStopID, d.LiveStopPrice = stop.OrderID, StopTrigger(stop)
		d.State = StateProtected
		d.Reason = fmt.Sprintf("reduce-only %s live at %g (orderId=%s)", stop.Type, d.LiveStopPrice, stop.OrderID)
		if t.StopOrderID != "" && t.StopOrderID != stop.OrderID {
			// Protected, but by a different order than the CSV names. Worth
			// saying out loud: it usually means the stop was re-placed by
			// hand, and it is the benign twin of the ghost case below.
			d.GhostStopID = t.StopOrderID
			d.Reason += fmt.Sprintf("; journal names a different order %s", t.StopOrderID)
		}
		return d
	}

	d.State = StateNaked
	d.Reason = "position is live with NO reduce-only stop on the exchange"
	if t.StopOrderID != "" {
		// The case cmd/web structurally cannot reach, because it stops
		// checking once this field is set.
		d.GhostStopID = t.StopOrderID
		d.Reason = fmt.Sprintf("position is live and journal names stop order %s, but the exchange has no resting stop", t.StopOrderID)
	}
	if mode == ModePlace || t.StopAuto {
		d.PlaceStop = t.Stop
	}
	return d
}

// FindProtectiveStop returns the resting stop order that closes pos.
//
// "Reduce-only" is not a single flag on this exchange. In ONE-WAY mode BingX
// takes reduceOnly=true; in HEDGE mode it REJECTS that parameter and carries
// the close-side semantics in positionSide instead (see
// bingx.PlaceStopMarket). A genuine hedge-mode protective stop therefore comes
// back with ReduceOnly=false, and a check that required the flag would call
// every hedge-mode position naked and place a second stop on all of them.
//
// So the flag is accepted as sufficient, and positionSide is accepted as an
// alternative ONLY when the position itself is hedge-mode. In one-way mode a
// non-reduce-only stop on the closing side is genuinely ambiguous — it could
// close, or it could stop-and-reverse — and ambiguity resolves toward naked.
//
// Strictness is one-directional throughout: a wrongly-rejected order costs a
// duplicate stop, which BingX will not let over-close, while a wrongly-
// accepted one costs the position. Orders that do not report a side are
// accepted rather than dropped, so a response that stops populating the field
// degrades to over-inclusive instead of double-stopping everything.
func FindProtectiveStop(resting []bingx.OpenOrder, pos *bingx.Position) (bingx.OpenOrder, bool) {
	for _, o := range resting {
		if Protects(o, pos) {
			return o, true
		}
	}
	return bingx.OpenOrder{}, false
}

// Protects reports whether one resting order would close pos.
//
// Exported because /ops/verify renders the same verdict. That page used to ask
// only whether an order's type contained "STOP", which is the dangerous
// direction to be wrong in: a stop that is not reduce-only (and so could OPEN
// a position), or one belonging to the opposite book in hedge mode, both
// rendered as "🛡️ stop live" on the surface whose entire purpose is to be
// trusted over local bookkeeping.
func Protects(o bingx.OpenOrder, pos *bingx.Position) bool {
	if !strings.Contains(strings.ToUpper(o.Type), "STOP") {
		return false
	}
	want := "SELL"
	if pos.Side == "short" {
		want = "BUY"
	}
	if o.Side != "" && strings.ToUpper(o.Side) != want {
		return false
	}
	if o.ReduceOnly {
		return true
	}
	hedge := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"
	return hedge && strings.EqualFold(o.PositionSide, pos.PositionSide)
}

// StopTrigger prefers StopPrice and falls back to Price — BingX populates one
// or the other depending on the order type.
func StopTrigger(o bingx.OpenOrder) float64 {
	if o.StopPrice > 0 {
		return o.StopPrice
	}
	return o.Price
}
