package main

// POST /ops/entry — place a resting LIMIT entry with a BingX-bundled stop and
// take-profit, from the phone.
//
// WHY IT EXISTS: on 2026-09-15 a level the trader had named hours earlier
// (ETH 2441, a three-way confluence of a 4h HVN, a 3-touch EQL and a 109-bar
// pivot shelf) filled three separate times and worked every time — on a day
// when the same account's only loss came from a chased market entry. The order
// was never placed because placing one meant leaving this tool and opening the
// exchange app. The journal already measures a 31% no-fill rate on limits that
// WERE placed; an order never sent does not even appear in that number.
//
// THIS ROUTE OPENS POSITIONS. Every other order surface here is reduce-only.
// The rails, in the order they matter:
//
//  1. A stop is REQUIRED, with no override (package entryplan). Not "warned
//     about" — refused. #60 went naked on a silently-failed bundled stop and
//     #70 had its stop removed by hand at 4am; a route where naked is one
//     blank field away will eventually place a naked one.
//  2. A limit that would fill instantly against the mark is refused unless
//     allow_marketable=1, because "I think it's breaking out" market entries
//     are this account's most expensive documented habit.
//  3. Size ceilings come from package risk — the SAME gate the rest of the
//     system uses, warn-only at the owner's explicit choice.
//  4. Two-step: without confirm=1 this returns a PREVIEW and sends nothing.
//  5. The response always says to re-verify. An order id is not proof the
//     exchange is holding the bundled stop.
import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/entryplan"
)

func (s *server) handleOpsEntry(c *gin.Context) {
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	symbolRaw := strings.TrimSpace(c.PostForm("symbol"))
	sym, err := resolveWebSymbol(symbolRaw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	num := func(field string) (float64, bool) {
		v, perr := strconv.ParseFloat(strings.TrimSpace(c.PostForm(field)), 64)
		if perr != nil {
			return 0, false
		}
		return v, true
	}
	entry, okE := num("entry")
	stop, _ := num("stop") // absence is entryplan's fault to report, not a parse error
	tp, _ := num("tp")
	margin, okM := num("margin")
	if !okE || !okM {
		c.JSON(http.StatusBadRequest, gin.H{"error": "entry and margin are required numbers"})
		return
	}
	lev := 125
	if v, ok := num("leverage"); ok && v > 0 {
		lev = int(v)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	mark, err := s.markFor(ctx, sym)
	if err != nil {
		// Fatal, like /ops/protect: the marketable check is relative to the
		// mark, so no mark means the one rail that stops a chase is blind.
		c.JSON(http.StatusBadGateway, gin.H{"error": "read mark price: " + err.Error()})
		return
	}

	short := strings.ToUpper(symbolRaw)
	prec, ok := qtyPrecision[short]
	if !ok {
		prec = 4
	}
	plan := entryplan.Build(entryplan.Inputs{
		Side: c.PostForm("side"), Entry: entry, Stop: stop, TP: tp,
		MarginUSDT: margin, Leverage: lev, Mark: mark,
		Equity:  s.equityAtEntry(ctx),
		QtyStep: prec,
		// Deliberately not a checkbox in any UI — a caller has to type it.
		AllowMarketable: c.PostForm("allow_marketable") == "1",
	})

	out := gin.H{
		"symbol": symbolRaw, "side": plan.Side, "entry": plan.Entry,
		"stop": plan.Stop, "tp": plan.TP, "qty": plan.Qty,
		"margin_usdt": plan.MarginUSDT, "leverage": plan.Leverage,
		"notional_usdt": plan.NotionalUSDT, "mark": mark,
		"risk_usdt": plan.RiskUSDT, "risk_pct_equity": plan.RiskPctEquity,
		"reward_usdt": plan.RewardUSDT, "rr": plan.RR,
		"marketable": plan.Marketable,
		"faults":     plan.Faults, "warnings": plan.Warnings, "ok": plan.OK(),
	}
	if !plan.OK() {
		out["stage"] = "refused"
		c.JSON(http.StatusBadRequest, out)
		return
	}

	// Size gate on the ACTUAL notional (floored qty x entry), same call the
	// rest of the order path makes.
	verdict, _ := s.checkNewPosition(ctx, plan.NotionalUSDT)
	out["risk_gate"] = gin.H{
		"blocked": verdict.Blocked, "reason": verdict.Reason,
		"enforced": verdict.Enforced, "warnings": verdict.Warnings,
		"lev_before": verdict.LevBefore, "lev_after": verdict.LevAfter,
		"kill_distance_pct": verdict.KillDistancePct,
	}
	if verdict.Blocked {
		out["stage"] = "refused"
		out["faults"] = append(plan.Faults, "risk gate: "+verdict.Reason)
		c.JSON(http.StatusBadRequest, out)
		return
	}

	if c.PostForm("confirm") != "1" {
		out["stage"] = "preview"
		out["note"] = "nothing sent — re-submit with confirm=1 to place"
		c.JSON(http.StatusOK, out)
		return
	}

	if err := s.client.SetLeverage(ctx, sym, plan.Side, plan.Leverage); err != nil {
		// Not fatal: the account may already be at this leverage, and BingX
		// rejects a no-op change on some symbols. The placed order carries its
		// own leverage, and the response says what happened.
		out["leverage_note"] = "SetLeverage: " + err.Error()
	}
	res, err := s.client.PlaceLimit(ctx, sym, plan.Side, plan.Qty, plan.Entry, plan.Stop, plan.TP, hedgeModeEnabled())
	if err != nil {
		out["stage"] = "failed"
		out["error"] = err.Error()
		c.JSON(http.StatusBadGateway, out)
		return
	}
	out["stage"] = "sent"
	if res != nil {
		out["order_id"] = res.OrderID
	}
	// The bundled stop is the part that has failed silently before. An order id
	// says BingX accepted the ENTRY; it says nothing about the SL/TP riding on
	// it, and a resting entry has no position for /ops/verify to check yet.
	out["next"] = "run /ops/orders (or trading-acct) to confirm the exchange is holding the bundled stop, " +
		"and /ops/verify again once it fills"
	c.JSON(http.StatusOK, out)
}
