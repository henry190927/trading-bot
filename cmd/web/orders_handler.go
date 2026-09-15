package main

// GET /ops/orders — every RESTING order on the exchange, with the stop and
// take-profit bundled onto it.
//
// The gap this closes: /ops/verify walks OPEN POSITIONS and checks each one
// has a stop. A resting entry has no position yet, so a limit order sitting
// on the book — protected or naked — appeared on no surface at all. On
// 2026-09-15 confirming one meant cross-compiling cmd/acct and scp'ing it to
// the VPS mid-session.
//
// That is the worse half of the same failure /ops/verify exists for. A naked
// POSITION is visible and alarming; a resting entry with no bundled stop is
// invisible, and becomes a naked position by itself, at whatever hour it
// fills.
import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/market"
)

func (s *server) handleOpsOrders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if s.client == nil || s.client.APIKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	// One call: this endpoint IGNORES its symbol parameter and returns the
	// whole book (see bingx.OpenOrders). Asking per-symbol would be N signed
	// calls for N copies of the same answer.
	all, err := s.client.OpenOrders(ctx, market.Symbol(""))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read open orders: " + err.Error()})
		return
	}

	rows := make([]gin.H, 0, len(all))
	naked := 0
	for _, o := range all {
		// reduce-only rows ARE the protection for something else; only an
		// order that can OPEN a position needs a stop of its own.
		opening := !o.ReduceOnly
		isNaked := opening && o.BundledStop <= 0
		if isNaked {
			naked++
		}
		rows = append(rows, gin.H{
			"symbol": o.Symbol, "orderId": o.OrderID, "type": o.Type,
			"side": o.Side, "positionSide": o.PositionSide,
			"price": o.Price, "stopPrice": o.StopPrice, "qty": o.Quantity,
			"reduceOnly":     o.ReduceOnly,
			"bundledStop":    o.BundledStop,
			"bundledTP":      o.BundledTP,
			"opensAPosition": opening,
			"naked":          isNaked,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := rows[i]["symbol"].(string)
		b, _ := rows[j]["symbol"].(string)
		if a != b {
			return a < b
		}
		pa, _ := rows[i]["price"].(float64)
		pb, _ := rows[j]["price"].(float64)
		return pa > pb
	})

	c.JSON(http.StatusOK, gin.H{
		"count": len(rows), "naked": naked, "orders": rows,
		"atTPE": time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04:05"),
		"note":  "naked = an order that can OPEN a position and carries no bundled stop; it becomes an unprotected position the moment it fills",
	})
}
