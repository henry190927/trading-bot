package main

// POST /ops/cancel — cancel ONE resting order by id.
//
// The companion /ops/entry needed: BingX has no modify-order call, so changing
// a resting entry's price, stop or target means cancel-and-replace. Without
// this that meant leaving the tool for the exchange app, which is the friction
// /ops/entry exists to remove — half a round trip is not much better than a
// whole one.
//
// THE RAIL THAT MATTERS: this refuses to cancel a REDUCE-ONLY order unless
// asked to in so many words. A resting entry is safe to cancel — it removes
// exposure that does not exist yet. A resting stop is the opposite: cancelling
// one turns a protected position into a naked one in a single call, from a
// phone, with no position-side context on screen. Positions have gone naked
// here through silence; they must not also be able to go naked through a
// convenience route. reduce_only_ok=1 is the deliberate override, and the
// response names what was cancelled either way.
import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bingx"
)

func (s *server) handleOpsCancel(c *gin.Context) {
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
	orderID := strings.TrimSpace(c.PostForm("order_id"))
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id is required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	// Read the book first, so the response can say WHAT is being cancelled and
	// the reduce-only rail has something to check. An id alone carries no
	// indication of whether it is an entry or the stop protecting a position.
	ords, err := s.client.OpenOrders(ctx, sym)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
		return
	}
	var target *bingx.OpenOrder
	for i := range ords {
		if ords[i].OrderID == orderID {
			target = &ords[i]
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "no resting order with that id on " + symbolRaw,
			"note":  "it may have filled or been cancelled already — check /ops/orders",
		})
		return
	}

	out := gin.H{
		"symbol": symbolRaw, "order_id": orderID, "type": target.Type,
		"side": target.Side, "price": target.Price, "qty": target.Quantity,
		"reduce_only":  target.ReduceOnly,
		"bundled_stop": target.BundledStop, "bundled_tp": target.BundledTP,
	}
	if target.ReduceOnly && c.PostForm("reduce_only_ok") != "1" {
		out["stage"] = "refused"
		out["error"] = "that is a REDUCE-ONLY order — cancelling it can leave a live position " +
			"unprotected; re-submit with reduce_only_ok=1 if that is the intent"
		c.JSON(http.StatusBadRequest, out)
		return
	}
	if c.PostForm("confirm") != "1" {
		out["stage"] = "preview"
		out["note"] = "nothing cancelled — re-submit with confirm=1"
		c.JSON(http.StatusOK, out)
		return
	}

	if err := s.client.CancelOrder(ctx, sym, orderID); err != nil {
		out["stage"] = "failed"
		out["error"] = err.Error()
		c.JSON(http.StatusBadGateway, out)
		return
	}
	out["stage"] = "cancelled"
	// Cancelling an entry that carried a bundled stop removes the protection
	// with it. Say so rather than letting a clean 200 imply nothing else moved.
	if target.BundledStop > 0 {
		out["note"] = "its bundled stop and take-profit went with it — anything replacing this order needs its own"
	}
	out["next"] = "re-open /ops/orders to confirm the book"
	c.JSON(http.StatusOK, out)
}
