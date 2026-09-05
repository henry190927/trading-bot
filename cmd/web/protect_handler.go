package main

// POST /ops/protect — attach a reduce-only stop and/or take-profit to a live
// position from the phone. This is B2: the same thing cmd/protect does, minus
// the SSH.
//
// WHY IT MATTERS: on 2026-08-31 trade #60 went naked; on 2026-09-03 #61 and
// #62 sat naked for hours with +108u unrealised, because attaching a stop was
// a CLI operation and the trader was at work. On 2026-09-05 two resting limits
// were placed that BingX will fill into an UNPROTECTED position — a
// reduce-only stop cannot rest before a position exists, so the gap is
// structural, not an oversight.
//
// SAFETY, in order of how much each one matters:
//
//  1. Reduce-only ONLY. Both legs go through bingx.PlaceStopMarket /
//     PlaceReduceOnlyLimit, neither of which can open a position.
//  2. Size comes from the live position (package protect), never the form.
//  3. Two-step: without confirm=1 this returns a PREVIEW and sends nothing.
//  4. The plan is re-validated inside protect.Apply, so a stale preview in an
//     old browser tab cannot be replayed into a bad order.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"myFirstGo/trading-bot/protect"

	"github.com/gin-gonic/gin"
)

func (s *server) handleOpsProtect(c *gin.Context) {
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
	// Blank means "don't touch this leg", which is different from zero — a
	// user clearing the stop field wants the stop left alone, not a stop at 0.
	stop, err := optionalPrice(c.PostForm("stop"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stop: " + err.Error()})
		return
	}
	tp, err := optionalPrice(c.PostForm("tp"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tp: " + err.Error()})
		return
	}
	side := strings.ToLower(strings.TrimSpace(c.PostForm("side")))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	pos, err := s.client.FindOpenPosition(ctx, sym, side)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read positions: " + err.Error()})
		return
	}
	mark, err := s.markFor(ctx, sym)
	if err != nil {
		// Deliberately fatal rather than defaulted: every sanity rule in
		// package protect is relative to the mark, so a missing mark means
		// there is nothing to check the order against.
		c.JSON(http.StatusBadGateway, gin.H{"error": "read mark price: " + err.Error()})
		return
	}

	plan := protect.BuildPlan(pos, mark, stop, tp)
	out := gin.H{
		"symbol": symbolRaw, "side": plan.Side, "qty": plan.Qty,
		"entry": plan.Entry, "mark": plan.Mark, "hedge": plan.Hedge,
		"stop": plan.Stop, "tp": plan.TP,
		"stop_locks_usdt": plan.StopLocksUSDT, "tp_gain_usdt": plan.TPGainUSDT,
		"faults": plan.Faults, "ok": plan.OK(),
	}

	if !plan.OK() {
		out["stage"] = "refused"
		c.JSON(http.StatusBadRequest, out)
		return
	}
	if c.PostForm("confirm") != "1" {
		out["stage"] = "preview"
		out["note"] = "nothing sent — re-submit with confirm=1 to place"
		c.JSON(http.StatusOK, out)
		return
	}

	res := protect.Apply(ctx, s.client, plan)
	out["stage"] = "sent"
	out["stop_order_id"] = res.StopOrderID
	out["tp_order_id"] = res.TPOrderID
	out["errors"] = res.Errors
	// Always tell the caller to re-verify, including on a partial failure.
	// A bundled SL silently failing to attach is exactly how #60 went naked,
	// so an order id in the response is not proof the exchange is holding it.
	out["next"] = "re-open /ops/verify to confirm the exchange is actually holding these"
	if len(res.Errors) > 0 && !res.Sent() {
		c.JSON(http.StatusBadGateway, out)
		return
	}
	c.JSON(http.StatusOK, out)
}

// optionalPrice parses a price field where blank means "leave this leg alone".
// A negative or non-numeric value is an error rather than a silent zero,
// because a silent zero here is an unprotected position that reads as handled.
func optionalPrice(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, errNegativePrice
	}
	return v, nil
}

var errNegativePrice = errors.New("price must be >= 0")
