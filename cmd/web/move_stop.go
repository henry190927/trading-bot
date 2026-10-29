package main

// POST /api/trade/:id/move-stop — move a journal trade's LIVE stop on BingX.
//
// Two things this is deliberately not:
//
//   - It is not an edit of the journal's stop. That column is the R baseline
//     (the risk taken at entry); moving it to a trailing level would rescale
//     every R the trade has, and a TP1 leg at +1.29R would read as +4.7R. The
//     row keeps its stop and only stop_order_id follows the new order.
//   - It is not /ops/protect, which only ADDS a stop. Used to "move" one it
//     leaves the old stop resting beside the new.
//
// Two-step like /ops/protect: without confirm=1 it returns a preview and sends
// nothing. Size comes from the live position (package protect), the new stop
// is placed before the old ones are cancelled (protect.MoveStop), and every
// rule is checked against the mark, so a profit-lock above entry is allowed
// and a stop the mark has already passed is not.

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

// liveStop is one resting order that protects a position.
type liveStop struct {
	ID    string  `json:"id"`
	Price float64 `json:"price"`
}

// protectiveStops lists every resting order that would close pos.
func protectiveStops(resting []bingx.OpenOrder, pos *bingx.Position) []liveStop {
	var out []liveStop
	for _, o := range resting {
		if bracket.Protects(o, pos) {
			out = append(out, liveStop{ID: o.OrderID, Price: bracket.StopTrigger(o)})
		}
	}
	return out
}

// rIfStopped is the whole trade's R if the remainder exits at stop: the
// partial closes' locked R plus the remainder's share at that price.
// Measured against the journal's stop, the R baseline.
func rIfStopped(t journal.Trade, stop float64) float64 {
	return journal.LockedR(t) + (1-journal.ClosedFrac(t))*journal.RealizedR(t, stop)
}

func (s *server) handleMoveStop(c *gin.Context) {
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
	stop, err := optionalPrice(c.PostForm("stop"))
	if err != nil || stop <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("ms.err.price")})
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
			// Without the resting list the old stop cannot be found to cancel,
			// and "move" would silently become "add". Refuse instead.
			c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
			return
		}
		current = protectiveStops(resting, pos)
	}

	plan := protect.BuildPlan(pos, mark, stop, 0)
	usdt := (stop - plan.Entry) * plan.Qty
	if plan.Side == "short" {
		usdt = -usdt
	}
	out := gin.H{
		"id": t.ID, "symbol": t.Symbol, "side": t.Side,
		"qty": plan.Qty, "entry": plan.Entry, "mark": plan.Mark,
		"current": current, "stop": stop, "baseline": t.Stop,
		"r_if_stopped": rIfStopped(t, stop), "usdt_at_stop": usdt,
		"faults": protectFaults(tr, plan.FaultMsgs), "ok": plan.OK(),
	}
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

	old := make([]string, 0, len(current))
	for _, st := range current {
		old = append(old, st.ID)
	}
	res := protect.MoveStop(ctx, s.client, plan, old)
	out["stage"] = "sent"
	out["new_stop_id"] = res.NewStopID
	out["cancelled"] = res.Cancelled
	out["errors"] = res.Errors
	if res.NewStopID != "" {
		s.recordStopOrderID(t.ID, res.NewStopID)
	}
	if res.NewStopID == "" {
		c.JSON(http.StatusBadGateway, out)
		return
	}
	c.JSON(http.StatusOK, out)
}

// recordStopOrderID points the row's stop_order_id at the new order. Read,
// set one field by ID, write — under sweepMu, so it cannot interleave with
// the background sweep's own read-merge-write.
func (s *server) recordStopOrderID(id int, orderID string) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	trades, err := journal.ReadAll("")
	if err != nil {
		return
	}
	for i := range trades {
		if trades[i].ID == id {
			trades[i].StopOrderID = orderID
			_ = journal.WriteAll("", trades)
			return
		}
	}
}

// liveStopsFor reads, per symbol, the resting protective stops of each active
// trade's position. Used by the /today card to show the exchange's stop next
// to the R baseline. Best effort: a symbol whose read fails is just absent.
//
// The same reads also yield the resting take-profits (tps), so the card can
// show the final target the exchange holds without a second round trip.
func (s *server) liveStopsFor(ctx context.Context, trades []journal.Trade) (out, tps map[int][]liveStop) {
	out, tps = map[int][]liveStop{}, map[int][]liveStop{}
	if s.client == nil || s.client.APIKey == "" {
		return out, tps
	}
	poss, err := s.client.AllPositions(ctx)
	if err != nil {
		return out, tps
	}
	orders := map[string][]bingx.OpenOrder{}
	for _, t := range trades {
		if !t.IsActive() {
			continue
		}
		sym, err := resolveWebSymbol(t.Symbol)
		if err != nil {
			continue
		}
		var pos *bingx.Position
		for i := range poss {
			if poss[i].Symbol == sym && strings.EqualFold(poss[i].Side, t.Side) {
				pos = &poss[i]
				break
			}
		}
		if pos == nil {
			continue
		}
		rest, ok := orders[string(sym)]
		if !ok {
			if rest, err = s.client.OpenOrders(ctx, sym); err != nil {
				continue
			}
			orders[string(sym)] = rest
		}
		out[t.ID] = protectiveStops(rest, pos)
		tps[t.ID] = takeProfits(rest, pos)
	}
	return out, tps
}
