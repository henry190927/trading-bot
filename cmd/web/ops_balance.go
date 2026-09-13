package main

// GET /ops/balance — read-only account equity and total exposure.
//
// Added 2026-09-08 because there was no online way to answer "what is the
// balance right now". cmd/acct can read it but was never deployed, and the
// home IP is not on the BingX whitelist, so every balance figure quoted during
// a session came from a stale snapshot — 336.30u was being repeated for three
// days while the real number moved. Sizing arithmetic built on a stale equity
// is wrong in the direction that matters.
//
// Extended the same day with the exposure block. Equity alone still cannot say
// whether a book is survivable: the pair that closed the account was 16,463u
// of notional, which is unremarkable next to a large balance and fatal next to
// 139.66u. Account leverage and its reciprocal, the kill distance, are the
// comparable figures, and until now neither existed anywhere in the system.
//
// Read-only throughout: two signed GETs, nothing mutable.

import (
	"context"
	"net/http"
	"time"

	"github.com/henry190927/trading-bot/risk"

	"github.com/gin-gonic/gin"
)

func (s *server) handleOpsBalance(c *gin.Context) {
	if s.client == nil || s.client.APIKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	// bingx.AccountBalance accepts both the nested and flat payload shapes and
	// keeps every field a pointer, so a balance reading 0 stays
	// distinguishable from one that failed to parse. The raw payload is echoed
	// for the same reason: a response-shape change must be visible rather than
	// silently zeroing the answer.
	b, err := s.client.AccountBalance(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	e := s.liveExposure(ctx)
	limits := riskLimits()
	// newNotional 0: this prices the book AS IT STANDS, so LevAfter is the
	// current leverage and the verdict describes reality rather than a
	// hypothetical order.
	v := risk.Check(limits, e, 0)

	legs := e.Legs
	if legs == nil {
		legs = []string{}
	}
	warnings := v.Warnings
	if warnings == nil {
		warnings = []string{}
	}

	c.JSON(http.StatusOK, gin.H{
		"asset":            b.Asset,
		"balance":          b.Balance,
		"equity":           b.Equity,
		"availableMargin":  b.AvailableMargin,
		"usedMargin":       b.UsedMargin,
		"unrealizedProfit": b.UnrealizedProfit,
		"realisedProfit":   b.RealisedProfit,
		"exposure": gin.H{
			"openCount":       e.OpenCount,
			"openNotionalU":   e.OpenNotional,
			"legs":            legs,
			"accountLeverage": v.LevAfter,
			"killDistancePct": v.KillDistancePct,
			"enforced":        v.Enforced,
			"warnings":        warnings,
		},
		"limits": gin.H{
			"maxAccountLev":   limits.MaxAccountLev,
			"warnAccountLev":  limits.WarnAccountLev,
			"maxNotionalUSDT": limits.MaxNotionalUSDT,
			"maxConcurrent":   limits.MaxConcurrent,
			"anySet":          limits.Enabled(),
		},
		"atTPE": time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04:05"),
		"raw":   b.Raw,
	})
}
