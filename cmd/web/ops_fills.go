package main

// GET /ops/fills — read-only filled-order history from the exchange.
//
// Closing a trade by hand on the phone leaves the journal depending on
// recollection of the price. Two trades were closed over a weekend with a
// reversal in between; reconstructing that from memory is how an R baseline
// drifts from what actually happened. This asks the exchange instead.
//
// GET-only and every call underneath is a signed GET (bingx.SignedGetRaw),
// so this route cannot place, modify or cancel anything.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *server) handleOpsFills(c *gin.Context) {
	if s.client == nil || s.client.APIKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	hours := 48
	if v := strings.TrimSpace(c.Query("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*30 {
			hours = n
		}
	}
	// Default to every symbol the UI knows rather than requiring one: the
	// caller reconciling a journal usually wants "what did I actually do",
	// not one instrument at a time.
	var want []string
	if q := strings.TrimSpace(c.Query("symbol")); q != "" {
		want = strings.Split(strings.ToUpper(q), ",")
	} else {
		want = uiSymbols
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	end := time.Now()
	start := end.Add(-time.Duration(hours) * time.Hour)

	tpe := time.FixedZone("Asia/Taipei", 8*3600)
	rows := []gin.H{}
	problems := []string{}
	for _, short := range want {
		sym, err := resolveWebSymbol(strings.TrimSpace(short))
		if err != nil {
			problems = append(problems, short+": "+err.Error())
			continue
		}
		fills, err := s.client.OrderHistory(ctx, sym, start, end)
		if err != nil {
			// Reported, never swallowed: an empty result must be
			// distinguishable from an endpoint that refused.
			problems = append(problems, short+": "+err.Error())
			continue
		}
		for _, f := range fills {
			rows = append(rows, gin.H{
				"symbol": short, "orderId": f.OrderID,
				"side": f.Side, "positionSide": f.PositionSide, "type": f.Type,
				"avgPrice": f.AvgPrice, "price": f.Price, "qty": f.Quantity,
				"profitUSDT": f.ProfitUSDT, "feeUSDT": f.FeeUSDT,
				"status": f.Status, "source": f.Source,
				"timeTPE": f.Time.In(tpe).Format("2006-01-02 15:04:05"),
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"fills": rows, "n": len(rows),
		"windowHours": hours,
		"fromTPE":     start.In(tpe).Format("2006-01-02 15:04"),
		"toTPE":       end.In(tpe).Format("2006-01-02 15:04"),
		"problems":    problems,
	})
}
