package main

// POST /api/trade/:id/add-tp    — add a partial take-profit to a live position.
// POST /api/trade/:id/cancel-tp — cancel one resting take-profit.
//
// Step (b2) of docs/position_orders_design.md. Neither writes the journal:
// what rests on the exchange is read from the exchange, and a partial close
// that fills is recorded by the sweep (exits.go).
//
// The size is a share of the ORIGINAL position (the user's decision), so
// "30%" means the same thing before and after other legs have filled, and
// the leg's R weight is exactly that share. The order is a reduce-only
// LIMIT at the price (decision: limit, for the maker fee).

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/protect"
)

// addTPSize turns "pct of the original position" into an order quantity,
// floored to the lot, and checks it leaves part of the live position open —
// a take-profit for all of it is a final target, which move-tp handles.
func addTPSize(entryQty, livePos, pct float64, lotDecimals int) (qty float64, fault string) {
	if pct <= 0 || pct >= 100 {
		return 0, "atp.err.pct"
	}
	if entryQty <= 0 {
		return 0, "atp.err.noEntryQty"
	}
	qty = floorTo(entryQty*pct/100, lotDecimals)
	if qty <= 0 {
		return 0, "atp.err.tooSmall"
	}
	if qty >= livePos {
		return qty, "atp.err.wholePos"
	}
	return qty, ""
}

func (s *server) handleAddTP(c *gin.Context) {
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
	price, err := optionalPrice(c.PostForm("tp"))
	if err != nil || price <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("mt.err.price")})
		return
	}
	pct, _ := strconv.ParseFloat(strings.TrimSpace(c.PostForm("pct")), 64)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read positions: " + err.Error()})
		return
	}
	if pos == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("atp.err.noPos")})
		return
	}
	mark, err := s.markFor(ctx, sym)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read mark price: " + err.Error()})
		return
	}
	// The entry order's own fill is the true denominator; the planned size
	// is the fallback (entryQtyFrom).
	end := time.Now()
	start := t.OpenedAt.Add(-time.Minute)
	if earliest := end.Add(-exchangeHistoryMax); start.Before(earliest) {
		start = earliest
	}
	fills, _ := s.client.OrderHistory(ctx, sym, start, end)
	entryQty := entryQtyFrom(t, fills)
	resting, err := s.client.OpenOrders(ctx, sym)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
		return
	}
	current := takeProfits(resting, pos)

	qty, fault := addTPSize(entryQty, pos.Quantity, pct, lotPrec(t.Symbol))
	plan := protect.BuildPlan(pos, mark, 0, price)
	plan.Qty = qty
	usdt := (price - plan.Entry) * qty
	if plan.Side == "short" {
		usdt = -usdt
	}
	restingQty := 0.0
	for _, tp := range current {
		restingQty += tp.Qty
	}
	out := gin.H{
		"id": t.ID, "symbol": t.Symbol, "side": t.Side,
		"tp": price, "pct": pct, "qty": qty, "entry_qty": entryQty, "live_qty": pos.Quantity,
		"mark": mark, "current": current, "resting_tp_qty": restingQty,
		// The leg's own R, weighted by its share: what it locks if it fills.
		"r_locked": pct / 100 * journal.RealizedR(t, price), "usdt": usdt,
		"faults": protectFaults(tr, plan.FaultMsgs), "ok": plan.OK() && fault == "",
	}
	if fault != "" {
		out["faults"] = append(out["faults"].([]string), tr(fault))
	}
	if !plan.OK() || fault != "" {
		out["stage"] = "refused"
		c.JSON(http.StatusBadRequest, out)
		return
	}
	if c.PostForm("confirm") != "1" {
		out["stage"] = "preview"
		c.JSON(http.StatusOK, out)
		return
	}
	res := protect.Apply(ctx, s.client, plan)
	out["stage"] = "sent"
	out["order_id"] = res.TPOrderID
	out["errors"] = res.Errors
	if res.TPOrderID == "" {
		c.JSON(http.StatusBadGateway, out)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *server) handleCancelTP(c *gin.Context) {
	if s.client == nil || s.client.APIKey == "" || s.client.APISecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	tr := s.translator(langOf(c))
	t, _, _, err := s.loadTradeByID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	sym, err := resolveWebSymbol(t.Symbol)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := strings.TrimSpace(c.PostForm("order_id"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	pos, err := s.client.FindOpenPosition(ctx, sym, t.Side)
	if err != nil || pos == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("atp.err.noPos")})
		return
	}
	resting, err := s.client.OpenOrders(ctx, sym)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
		return
	}
	// Only a take-profit of THIS position may be cancelled here — never a
	// stop, never another trade's order, whatever id the request carries.
	found := false
	for _, tp := range takeProfits(resting, pos) {
		found = found || tp.ID == id
	}
	if id == "" || !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": tr("atp.err.notTP")})
		return
	}
	if err := s.client.CancelOrder(ctx, sym, id); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "cancel: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"cancelled": id})
}
