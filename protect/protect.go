// Package protect plans reduce-only stop / take-profit orders against an
// ALREADY-OPEN position.
//
// It exists so the rules live in exactly ONE place. They were previously only
// in cmd/protect, reachable only over SSH, which is how positions have sat
// naked for hours with nobody at a terminal — and how one went naked
// entirely. cmd/protect and the /ops/verify button now
// call the same BuildPlan, so a fix to the sanity rules cannot land in one and
// miss the other.
//
// Two invariants carried over from the CLI, both load-bearing:
//
//  1. SIZE COMES FROM THE LIVE POSITION, never from a caller-supplied number.
//     A reduce-only order sized by hand is how a position ends up
//     partially protected — the worst of both states, because it reads as
//     protected.
//  2. SANITY IS CHECKED AGAINST THE MARK, NOT THE ENTRY. A stop above entry
//     on a long is a legitimate profit-lock; a stop above the MARK is an
//     order that fires the instant it lands and closes at market. Those are
//     entirely different mistakes and only the second one is a mistake. An
//     earlier entry-based check forbade every trailing stop.
package protect

import (
	"context"
	"fmt"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/market"
)

// Plan is the preview of what attaching stop/tp to a live position would do.
// Faults non-empty means the plan must NOT be sent.
type Plan struct {
	Symbol   market.Symbol
	Side     string // "long" | "short"
	Qty      float64
	Entry    float64
	Mark     float64
	Leverage int
	Hedge    bool

	Stop float64 // 0 = not requested
	TP   float64 // 0 = not requested

	// StopLocksUSDT is the profit the stop would secure when it sits BEYOND
	// entry in the favourable direction (a profit-lock rather than a loss
	// cap). Zero when the stop is still on the losing side, which is the
	// ordinary case and not worth annotating.
	StopLocksUSDT float64
	// TPGainUSDT is the realised gain if the take-profit fills.
	TPGainUSDT float64

	Faults []string
	// FaultMsgs carries each fault as a code plus its values, in the same
	// order as Faults, so a caller can render it in its own language.
	// Faults stays the English text the CLI prints.
	FaultMsgs []Fault
}

// Fault is one refusal reason as a code and the values its sentence needs.
type Fault struct {
	Code string
	Args []any
}

// EnglishFormat is the CLI wording of each fault code.
var EnglishFormat = map[string]string{
	"no-position":   "no open position on the exchange — nothing to protect",
	"zero-qty":      "position quantity is 0 — nothing to protect",
	"no-mark":       "live mark price unavailable — refusing to place without a reference price",
	"nothing-asked": "give a stop, a tp, or both",
	"bad-side":      "unexpected position side %q",
	"stop-long":     "stop %.4f is at/above the live mark %.4f (long) — it would trigger immediately and close at market",
	"stop-short":    "stop %.4f is at/below the live mark %.4f (short) — it would trigger immediately and close at market",
	"tp-long":       "tp %.4f is at/below the live mark %.4f (long) — it would fill immediately instead of resting",
	"tp-short":      "tp %.4f is at/above the live mark %.4f (short) — it would fill immediately instead of resting",
}

// English renders the fault the way the CLI prints it.
func (f Fault) English() string { return fmt.Sprintf(EnglishFormat[f.Code], f.Args...) }

func (p *Plan) fault(code string, args ...any) {
	f := Fault{Code: code, Args: args}
	p.FaultMsgs = append(p.FaultMsgs, f)
	p.Faults = append(p.Faults, f.English())
}

// OK reports whether the plan is safe to send.
func (p Plan) OK() bool { return len(p.Faults) == 0 }

// BuildPlan is pure: it decides everything from the position, the live mark,
// and the requested prices, and sends nothing. Callers preview it, then Apply.
func BuildPlan(pos *bingx.Position, mark, stop, tp float64) Plan {
	p := Plan{Stop: stop, TP: tp, Mark: mark}
	if pos == nil {
		p.fault("no-position")
		return p
	}
	p.Symbol, p.Side, p.Qty = pos.Symbol, pos.Side, pos.Quantity
	p.Entry, p.Leverage = pos.EntryPrice, pos.Leverage
	// Hedge mode is inferred from what the exchange reports, not configured:
	// positionSide LONG/SHORT means hedge, BOTH means one-way, and the two
	// need different order payloads.
	p.Hedge = pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"

	if pos.Quantity <= 0 {
		p.fault("zero-qty")
	}
	if mark <= 0 {
		p.fault("no-mark")
	}
	if stop <= 0 && tp <= 0 {
		p.fault("nothing-asked")
	}
	if pos.Side != "long" && pos.Side != "short" {
		p.fault("bad-side", pos.Side)
	}
	if !p.OK() {
		return p
	}

	if stop > 0 {
		switch {
		case pos.Side == "long" && stop >= mark:
			p.fault("stop-long", stop, mark)
		case pos.Side == "short" && stop <= mark:
			p.fault("stop-short", stop, mark)
		}
		if locked, isLock := lockedProfit(pos.Side, pos.EntryPrice, stop, pos.Quantity); isLock {
			p.StopLocksUSDT = locked
		}
	}
	if tp > 0 {
		switch {
		case pos.Side == "long" && tp <= mark:
			p.fault("tp-long", tp, mark)
		case pos.Side == "short" && tp >= mark:
			p.fault("tp-short", tp, mark)
		}
		gain := (tp - pos.EntryPrice) * pos.Quantity
		if pos.Side == "short" {
			gain = -gain
		}
		p.TPGainUSDT = gain
	}
	return p
}

// lockedProfit reports the USDT a stop beyond entry would secure, and whether
// the stop is a profit-lock at all.
func lockedProfit(side string, entry, stop, qty float64) (float64, bool) {
	beyond := (side == "long" && stop > entry) || (side == "short" && stop < entry)
	if !beyond {
		return 0, false
	}
	locked := (stop - entry) * qty
	if side == "short" {
		locked = -locked
	}
	return locked, true
}

// Placer is the subset of the BingX client Apply needs, so the send path is
// testable without a network or an API key.
type Placer interface {
	PlaceStopMarket(ctx context.Context, sym market.Symbol, posSide string, qty, stopPrice float64, hedgeMode bool) (*bingx.OrderResult, error)
	PlaceReduceOnlyLimit(ctx context.Context, sym market.Symbol, posSide string, qty, price float64, hedgeMode bool) (*bingx.OrderResult, error)
}

// Result records what Apply actually sent.
type Result struct {
	StopOrderID string
	TPOrderID   string
	Errors      []string
}

// Sent reports whether anything reached the exchange — used to decide whether
// a re-verify is warranted even on a partial failure.
func (r Result) Sent() bool { return r.StopOrderID != "" || r.TPOrderID != "" }

// Apply sends the plan. It REFUSES a plan with faults rather than trusting the
// caller to have checked, because this is the last gate before a live order.
//
// The stop goes first, deliberately. If both are requested and the second call
// fails, the position is left protected-but-without-a-target rather than
// targeted-but-naked.
func Apply(ctx context.Context, c Placer, p Plan) Result {
	var r Result
	if !p.OK() {
		r.Errors = append(r.Errors, "refusing to send a plan with faults: "+p.Faults[0])
		return r
	}
	if p.Stop > 0 {
		res, err := c.PlaceStopMarket(ctx, p.Symbol, p.Side, p.Qty, p.Stop, p.Hedge)
		if err != nil {
			r.Errors = append(r.Errors, "place stop: "+err.Error())
		} else {
			r.StopOrderID = res.OrderID
		}
	}
	if p.TP > 0 {
		res, err := c.PlaceReduceOnlyLimit(ctx, p.Symbol, p.Side, p.Qty, p.TP, p.Hedge)
		if err != nil {
			r.Errors = append(r.Errors, "place tp: "+err.Error())
		} else {
			r.TPOrderID = res.OrderID
		}
	}
	return r
}

// Mover is what MoveStop needs: Placer plus cancelling an order.
type Mover interface {
	Placer
	CancelOrder(ctx context.Context, sym market.Symbol, orderID string) error
}

// MoveResult records a stop move.
type MoveResult struct {
	NewStopID string
	Cancelled []string // old stop ids that were cancelled
	Errors    []string
}

// MoveStop replaces the position's protective stop(s) with p.Stop.
//
// The NEW stop is placed first and the old ones are cancelled only after it
// is accepted, so at no moment is the position without a stop: a failed place
// leaves the old stop untouched, and a failed cancel leaves two stops resting,
// which reduce-only cannot over-close. The reverse order — cancel, then place —
// is the one that can leave a position naked, and is never used.
//
// p must be a stop-only plan (p.TP == 0); a TP in a "move stop" request is a
// caller bug, refused rather than silently placed.
func MoveStop(ctx context.Context, c Mover, p Plan, oldStopIDs []string) MoveResult {
	var r MoveResult
	if !p.OK() {
		r.Errors = append(r.Errors, "refusing to send a plan with faults: "+p.Faults[0])
		return r
	}
	if p.Stop <= 0 || p.TP != 0 {
		r.Errors = append(r.Errors, "move-stop needs a stop-only plan")
		return r
	}
	res, err := c.PlaceStopMarket(ctx, p.Symbol, p.Side, p.Qty, p.Stop, p.Hedge)
	if err != nil {
		r.Errors = append(r.Errors, "place new stop: "+err.Error()+" — old stop left in place")
		return r
	}
	r.NewStopID = res.OrderID
	for _, id := range oldStopIDs {
		if id == "" || id == r.NewStopID {
			continue
		}
		if err := c.CancelOrder(ctx, p.Symbol, id); err != nil {
			r.Errors = append(r.Errors, "cancel old stop "+id+": "+err.Error())
			continue
		}
		r.Cancelled = append(r.Cancelled, id)
	}
	return r
}

// RestingTP is a take-profit currently on the exchange: what MoveTP cancels,
// and what it puts back if the move fails.
type RestingTP struct {
	ID    string
	Price float64
}

// MoveTPResult records a take-profit move.
type MoveTPResult struct {
	NewTPID   string   // the new take-profit's order id; "" when it was not placed
	Cancelled []string // old take-profit ids that were cancelled
	Restored  []string // order ids of old take-profits put back after a failure
	Errors    []string
}

// MoveTP replaces the position's final take-profit(s) with a reduce-only
// LIMIT at p.TP, for the live position's size (p.Qty).
//
// It runs in the OPPOSITE order to MoveStop, and the reason is the point of
// the exercise. A stop must never be absent, so MoveStop places first and
// accepts two stops resting for a moment. A take-profit may be absent for a
// moment — nothing is unprotected — but two full-size reduce-only orders may
// be refused by the exchange as covering more than the position. So:
//
//  1. Refuse, with no exchange calls, a plan with faults or one that is not
//     take-profit-only (p.Stop != 0 or p.TP <= 0). Put a message in Errors.
//  2. Cancel each old take-profit, in the order given (skip empty ids).
//     If a cancel fails: stop cancelling, do NOT place the new one, put back
//     every take-profit this call already cancelled, and return.
//  3. Place the new take-profit (c.PlaceReduceOnlyLimit with p.Symbol,
//     p.Side, p.Qty, p.TP, p.Hedge).
//     If that fails: put back every cancelled take-profit and return.
//  4. "Put back" = PlaceReduceOnlyLimit at the old price, same qty; collect
//     the new order ids in Restored. A put-back that also fails leaves the
//     position with NO take-profit — say so in Errors, in words that contain
//     "no take-profit", because the caller has to tell the trader.
//
// Every failure adds exactly one entry to Errors. Done = every test in
// protect_test.go named TestMoveTP* passes.
func MoveTP(ctx context.Context, c Mover, p Plan, old []RestingTP) MoveTPResult {
	// TODO(you): implement the contract above.
	return MoveTPResult{Errors: []string{"MoveTP: not implemented"}}
}
