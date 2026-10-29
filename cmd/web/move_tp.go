package main

// POST /api/trade/:id/move-tp — move a trade's final take-profit on BingX.
//
// Step (a) of docs/position_orders_design.md. Same two-step shape as
// move-stop: without confirm=1 it returns a preview and sends nothing; size
// comes from the live position; every rule is checked against the mark.
//
// Unlike the stop, the journal's take-profit is not an R baseline, so a
// successful move does update it (tp2) along with tp2_order_id — the /today
// track then draws the target where the order actually rests.
//
// Which order is "the final take-profit": the resting take-profits are read
// from the exchange (bracket.TakesProfit). With one, it is that one. With
// several (a scale-out resting beside the final target), the caller must name
// it with replace=<orderId>; guessing would cancel the wrong one.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/bracket"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/protect"
)

// takeProfits lists every resting order that would take profit on pos.
func takeProfits(resting []bingx.OpenOrder, pos *bingx.Position) []liveStop {
	var out []liveStop
	for _, o := range resting {
		if bracket.TakesProfit(o, pos) {
			out = append(out, liveStop{ID: o.OrderID, Price: bracket.StopTrigger(o), Qty: o.Quantity})
		}
	}
	return out
}

// pickTPs decides which resting take-profits a move replaces. ok is false
// when the choice is ambiguous or names an order that is not resting.
func pickTPs(current []liveStop, replace string) (old []protect.RestingTP, ok bool) {
	if replace != "" {
		for _, c := range current {
			if c.ID == replace {
				return []protect.RestingTP{{ID: c.ID, Price: c.Price}}, true
			}
		}
		return nil, false
	}
	if len(current) > 1 {
		return nil, false
	}
	for _, c := range current {
		old = append(old, protect.RestingTP{ID: c.ID, Price: c.Price})
	}
	return old, true
}

func (s *server) handleMoveTP(c *gin.Context) {
	tr := s.translator(langOf(c))
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	t, _, _, err := s.loadTradeByID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if !t.IsActive() {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("ms.err.notActive", t.ID)})
		return
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tp, err := optionalPrice(c.PostForm("tp"))
	if err != nil || tp <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("mt.err.price")})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read positions: " + err.Error()})
		return
	}
	mark, err := s.markFor(ctx, sym)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read mark price: " + err.Error()})
		return
	}
	var current []liveStop
	if pos != nil {
		resting, err := s.client.OpenOrders(ctx, sym)
		if err != nil {
			// Without the resting list the old take-profit cannot be found,
			// and "move" would silently become "add". Refuse instead.
			c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
			return
		}
		current = takeProfits(resting, pos)
	}

	plan := protect.BuildPlan(pos, mark, 0, tp)
	out := gin.H{
		"id": t.ID, "symbol": t.Symbol, "side": t.Side,
		"qty": plan.Qty, "entry": plan.Entry, "mark": plan.Mark,
		"current": current, "tp": tp, "planned": t.TP2,
		"r_at_tp": rIfStopped(t, tp), "usdt_at_tp": plan.TPGainUSDT,
		"faults": protectFaults(tr, plan.FaultMsgs), "ok": plan.OK(),
	}
	old, picked := pickTPs(current, strings.TrimSpace(c.PostForm("replace")))
	if !picked {
		out["stage"] = "choose"
		out["error"] = tr("mt.err.choose")
		c.JSON(http.StatusBadRequest, out)
		return
	}
	out["replace"] = old
	if !plan.OK() {
		out["stage"] = "refused"
		c.JSON(http.StatusBadRequest, out)
		return
	}
	if c.PostForm("confirm") != "1" {
		out["stage"] = "preview"
		c.JSON(http.StatusOK, out)
		return
	}

	res := protect.MoveTP(ctx, s.client, plan, old)
	out["stage"] = "sent"
	out["new_tp_id"] = res.NewTPID
	out["cancelled"] = res.Cancelled
	out["restored"] = res.Restored
	out["errors"] = res.Errors
	if res.NewTPID == "" {
		c.JSON(http.StatusBadGateway, out)
		return
	}
	s.recordFinalTP(t.ID, tp, res.NewTPID)
	c.JSON(http.StatusOK, out)
}

// recordFinalTP points the row's final take-profit at the new order: tp2 and
// tp2_order_id. Under sweepMu, like recordStopOrderID, so it cannot
// interleave with the background sweep's read-merge-write.
func (s *server) recordFinalTP(id int, price float64, orderID string) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	trades, err := journal.ReadAll("")
	if err != nil {
		return
	}
	for i := range trades {
		if trades[i].ID == id {
			trades[i].TP2, trades[i].TP2OrderID = price, orderID
			_ = journal.WriteAll("", trades)
			return
		}
	}
}
