package main

// POST /ops/entry — place a resting LIMIT entry with a BingX-bundled stop and
// take-profit, from the phone.
//
// WHY IT EXISTS: a level identified hours in advance — a three-way confluence
// of a 4h HVN, a 3-touch EQL and a long pivot shelf — filled three separate
// times in one session and held its stop every time. No order was ever placed,
// because placing one meant leaving this tool and opening the exchange app.
// The journal already measures a 31% no-fill rate on limits that WERE placed;
// an order never sent does not even appear in that number.
//
// THIS ROUTE OPENS POSITIONS. Every other order surface here is reduce-only.
// The rails, in the order they matter:
//
//  1. A stop is REQUIRED, with no override (package entryplan). Not "warned
//     about" — refused. A bundled stop has silently failed to attach before,
//     and a live stop has been removed by hand; a route where naked
//     is one blank field away will eventually place a naked one.
//  2. A limit that would fill instantly against the mark is refused unless
//     allow_marketable=1. A marketable limit is a market order wearing a
//     limit's clothes, and an entry placed on a feeling that a level is about
//     to break is the failure this route exists to make deliberate.
//  3. Size ceilings come from package risk — the SAME gate the rest of the
//     system uses, warn-only at the owner's explicit choice.
//  4. Two-step: without confirm=1 this returns a PREVIEW and sends nothing.
//  5. The response always says to re-verify. An order id is not proof the
//     exchange is holding the bundled stop.
import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/entryplan"
	"github.com/henry190927/trading-bot/journal"
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

	// BOTH in one-way mode, LONG/SHORT only in hedge mode — the same mapping
	// placeEntryOnBingX uses. Sending "LONG" to a one-way account sets a side
	// that account does not have.
	levSide := "BOTH"
	if hedgeModeEnabled() {
		levSide = strings.ToUpper(plan.Side)
	}
	if err := s.client.SetLeverage(ctx, sym, levSide, plan.Leverage); err != nil {
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
	orderID := ""
	if res != nil {
		orderID = res.OrderID
		out["order_id"] = orderID
	}

	// Journal the row in the SAME call that placed the order.
	//
	// /ops/verify's whole job is comparing the journal against the exchange,
	// so an order placed without a row becomes the orphan position that page
	// is built to flag — and the flag fires at whatever hour the limit fills,
	// which for a level 1.3% away is usually the middle of the night. The
	// +record path has always written its row first for this reason.
	//
	// Written AFTER the placement, not before, and deliberately: this route's
	// preview/confirm split means a refused plan never reaches here, and a
	// row for an order the exchange rejected is a lie in the opposite
	// direction. A failed write is reported, never fatal — the money is
	// already committed by this point and the caller needs to know the order
	// exists more than it needs a clean response.
	if jerr := s.journalEntry(ctx, plan, symbolRaw, orderID, c); jerr != nil {
		out["journal"] = "FAILED: " + jerr.Error()
		out["journal_ok"] = false
	} else {
		out["journal_ok"] = true
	}
	// The bundled stop is the part that has failed silently before. An order id
	// says BingX accepted the ENTRY; it says nothing about the SL/TP riding on
	// it, and a resting entry has no position for /ops/verify to check yet.
	out["next"] = "run /ops/orders (or trading-acct) to confirm the exchange is holding the bundled stop, " +
		"and /ops/verify again once it fills"
	c.JSON(http.StatusOK, out)
}

// journalEntry writes the placed order into journal.csv as an open trade.
//
// tf/score/anchor/notes are optional passthroughs: this route is usually
// driven from a phone or a shell, where the full /journal/new form is not
// available, and a row with the numbers and a blank anchor is far better than
// no row. TP1 is left at 0 — the bundled take-profit closes 100%, so a partial
// TP1 is a separate reduce-only order placed after the fill, and recording one
// that does not exist would make /ops/verify report a stop that is not there.
func (s *server) journalEntry(ctx context.Context, plan entryplan.Plan, symbol, orderID string, c *gin.Context) error {
	trades, err := journal.ReadAll("")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	notes := strings.TrimSpace(c.PostForm("notes"))
	if notes == "" {
		notes = fmt.Sprintf("placed via /ops/entry — resting LIMIT %.4f x %.4f (%.2fu x %dx), "+
			"bundled stop %.4f, bundled tp %.4f, mark at placement %.4f",
			plan.Entry, plan.Qty, plan.MarginUSDT, plan.Leverage, plan.Stop, plan.TP, plan.Entry)
	}
	t := journal.Trade{
		ID:         journal.NextID(trades),
		OpenedAt:   now,
		AnalyzedAt: now,
		Symbol:     strings.ToUpper(symbol),
		Side:       plan.Side,
		TF:         strings.TrimSpace(c.PostForm("tf")),
		Score:      strings.TrimSpace(c.PostForm("score")),
		Entry:      plan.Entry,
		Stop:       plan.Stop,
		TP2:        plan.TP,
		Anchor:     strings.TrimSpace(c.PostForm("anchor")),
		OpenNotes:  notes,
		Leverage:   plan.Leverage,
		MarginUSDT: plan.MarginUSDT,
		// FALSE, and this is the important line in the function.
		//
		// StopAuto does NOT mean "a stop already exists". bracket.Decide reads
		// it as "place the journal stop for me" — it is the opt-in that makes
		// the sweep act instead of alert. Setting it true here, on the reading
		// that the bundled legs made it redundant, turned every /ops/entry
		// trade into one the daemon would keep re-arming: on 2026-09-15 it
		// re-placed a stop the desk had deliberately cancelled seven times,
		// and the seventh filled two seconds after it landed, closing a
		// position its owner had chosen to keep.
		//
		// The bundled stop and take-profit ride ON the entry order and BingX
		// activates them the instant it fills, so there is nothing for the
		// sweep to place. If one silently fails to attach, the sweep's DEFAULT
		// alert mode says so loudly — which is the behaviour that belongs
		// here. A guard may shout; it may not overrule.
		StopAuto:     false,
		TP2Auto:      false,
		EntryOrderID: orderID,
		EquityUSDT:   s.equityAtEntry(ctx),
	}
	// FilledAt stays zero: the order is RESTING. Marking it filled here is
	// what would make /ops/verify demand a position that does not exist yet.
	return journal.WriteAll("", append(trades, t))
}
