// Package protect plans reduce-only stop / take-profit orders against an
// ALREADY-OPEN position.
//
// It exists so the rules live in exactly ONE place. They were previously only
// in cmd/protect, reachable only over SSH, which is how #61 and #62 sat naked
// for hours on 2026-09-03 while the trader was at work — and how #60 went
// naked entirely on 2026-08-31. cmd/protect and the /ops/verify button now
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

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/market"
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
}

// OK reports whether the plan is safe to send.
func (p Plan) OK() bool { return len(p.Faults) == 0 }

// BuildPlan is pure: it decides everything from the position, the live mark,
// and the requested prices, and sends nothing. Callers preview it, then Apply.
func BuildPlan(pos *bingx.Position, mark, stop, tp float64) Plan {
	p := Plan{Stop: stop, TP: tp, Mark: mark}
	if pos == nil {
		p.Faults = append(p.Faults, "no open position on the exchange — nothing to protect")
		return p
	}
	p.Symbol, p.Side, p.Qty = pos.Symbol, pos.Side, pos.Quantity
	p.Entry, p.Leverage = pos.EntryPrice, pos.Leverage
	// Hedge mode is inferred from what the exchange reports, not configured:
	// positionSide LONG/SHORT means hedge, BOTH means one-way, and the two
	// need different order payloads.
	p.Hedge = pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"

	if pos.Quantity <= 0 {
		p.Faults = append(p.Faults, "position quantity is 0 — nothing to protect")
	}
	if mark <= 0 {
		p.Faults = append(p.Faults, "live mark price unavailable — refusing to place without a reference price")
	}
	if stop <= 0 && tp <= 0 {
		p.Faults = append(p.Faults, "give a stop, a tp, or both")
	}
	if pos.Side != "long" && pos.Side != "short" {
		p.Faults = append(p.Faults, fmt.Sprintf("unexpected position side %q", pos.Side))
	}
	if !p.OK() {
		return p
	}

	if stop > 0 {
		switch {
		case pos.Side == "long" && stop >= mark:
			p.Faults = append(p.Faults, fmt.Sprintf("stop %.4f is at/above the live mark %.4f (long) — it would trigger immediately and close at market", stop, mark))
		case pos.Side == "short" && stop <= mark:
			p.Faults = append(p.Faults, fmt.Sprintf("stop %.4f is at/below the live mark %.4f (short) — it would trigger immediately and close at market", stop, mark))
		}
		if locked, isLock := lockedProfit(pos.Side, pos.EntryPrice, stop, pos.Quantity); isLock {
			p.StopLocksUSDT = locked
		}
	}
	if tp > 0 {
		switch {
		case pos.Side == "long" && tp <= mark:
			p.Faults = append(p.Faults, fmt.Sprintf("tp %.4f is at/below the live mark %.4f (long) — it would fill immediately instead of resting", tp, mark))
		case pos.Side == "short" && tp >= mark:
			p.Faults = append(p.Faults, fmt.Sprintf("tp %.4f is at/above the live mark %.4f (short) — it would fill immediately instead of resting", tp, mark))
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
