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
	"github.com/henry190927/trading-bot/market"
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	end := time.Now()
	start := end.Add(-time.Duration(hours) * time.Hour)

	tpe := time.FixedZone("Asia/Taipei", 8*3600)
	rows := []gin.H{}
	problems := []string{}

	// WHICH symbols to ask about comes from the ACCOUNT, not from a list the
	// UI happens to know. Iterating uiSymbols made two real trades invisible
	// inside three days — XRP on 2026-09-18 and AKE on 2026-09-19 — and
	// neither was a missing entry so much as the wrong direction of
	// enumeration: a hardcoded roster is a guess about the past, and this
	// endpoint exists precisely for the trades nobody remembered to expect.
	// Same inversion exchangeOrphans applies to positions.
	//
	// ?symbol= still wins, for pulling one instrument deliberately.
	var want []market.Symbol
	discovery := "account income ledger"
	if q := strings.TrimSpace(c.Query("symbol")); q != "" {
		discovery = "explicit ?symbol="
		for _, short := range strings.Split(strings.ToUpper(q), ",") {
			sym, err := resolveWebSymbol(strings.TrimSpace(short))
			if err != nil {
				problems = append(problems, short+": "+err.Error())
				continue
			}
			want = append(want, sym)
		}
	} else if syms, err := s.client.TradedSymbols(ctx, start, end); err == nil {
		want = syms
	} else {
		// Falling back is right — an unreadable ledger must not render as "no
		// trades" — but the caller has to know the list went back to being a
		// guess, because that is exactly when a symbol goes missing again.
		discovery = "FALLBACK to uiSymbols — ledger unreadable"
		problems = append(problems, "income ledger: "+err.Error())
		for _, short := range uiSymbols {
			if sym, rerr := resolveWebSymbol(short); rerr == nil {
				want = append(want, sym)
			}
		}
	}

	for _, sym := range want {
		// A contract with no short name is still a contract that traded; the
		// raw code labels it rather than dropping the row.
		short := market.Short(sym)
		if short == "" {
			short = string(sym)
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
	labels := make([]string, 0, len(want))
	for _, sym := range want {
		if sh := market.Short(sym); sh != "" {
			labels = append(labels, sh)
		} else {
			labels = append(labels, string(sym))
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"fills": rows, "n": len(rows),
		"discovery":   discovery,
		"queried":     labels,
		"windowHours": hours,
		"fromTPE":     start.In(tpe).Format("2006-01-02 15:04"),
		"toTPE":       end.In(tpe).Format("2006-01-02 15:04"),
		"problems":    problems,
	})
}
