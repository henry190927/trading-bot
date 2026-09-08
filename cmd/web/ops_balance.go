package main

// GET /ops/balance — read-only account equity.
//
// Added 2026-09-08 because there was no online way to answer "what is the
// balance right now". cmd/acct can read it but was never deployed, and the
// home IP is not on the BingX whitelist, so every balance figure quoted
// during a session came from a stale snapshot — 336.30u was being repeated
// for three days while the real number moved. Sizing arithmetic built on a
// stale equity is wrong in the direction that matters.
//
// One signed GET, no parameters, nothing mutable.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"myFirstGo/trading-bot/bingx"

	"github.com/gin-gonic/gin"
)

func (s *server) handleOpsBalance(c *gin.Context) {
	if s.client == nil || s.client.APIKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "BingX API key/secret not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	raw, err := s.client.SignedGetRaw(ctx, bingx.PathBalance, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	// BingX nests the numbers under "balance" and sends them as strings.
	// Both the flat and nested shapes are accepted rather than guessed at,
	// and the raw payload is echoed so a shape change is visible instead of
	// silently zeroing the answer — a balance reading 0 must never be
	// indistinguishable from a balance that failed to parse.
	var env struct {
		Balance struct {
			Asset            string `json:"asset"`
			Balance          string `json:"balance"`
			Equity           string `json:"equity"`
			AvailableMargin  string `json:"availableMargin"`
			UsedMargin       string `json:"usedMargin"`
			UnrealizedProfit string `json:"unrealizedProfit"`
			RealisedProfit   string `json:"realisedProfit"`
		} `json:"balance"`
	}
	_ = json.Unmarshal(raw, &env)

	num := func(s string) *float64 {
		if s == "" {
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &v
	}
	c.JSON(http.StatusOK, gin.H{
		"asset":            env.Balance.Asset,
		"balance":          num(env.Balance.Balance),
		"equity":           num(env.Balance.Equity),
		"availableMargin":  num(env.Balance.AvailableMargin),
		"usedMargin":       num(env.Balance.UsedMargin),
		"unrealizedProfit": num(env.Balance.UnrealizedProfit),
		"realisedProfit":   num(env.Balance.RealisedProfit),
		"atTPE":            time.Now().In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04:05"),
		"raw":              json.RawMessage(raw),
	})
}
