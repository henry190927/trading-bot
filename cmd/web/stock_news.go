package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/earnings/finnhub"
)

// Per-symbol company-news, fetched live from Finnhub on demand (option A) and
// memoized 1h so browsing the board doesn't burn the free-tier rate limit.
type stockNewsCacheEntry struct {
	at   time.Time
	resp gin.H
}

var (
	stockNewsMu    sync.Mutex
	stockNewsCache = map[string]stockNewsCacheEntry{}
)

const stockNewsTTL = time.Hour

// handleStockNews — GET /api/stock/news?symbol=NVDA
// Returns recent company-news headlines for a US ticker (plain ticker as shown
// on the /fundamentals board). Live-fetched, 1h-cached. Never 500s on a fetch
// error — returns an empty list + an error string so the UI degrades cleanly.
func (s *server) handleStockNews(c *gin.Context) {
	sym := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	if sym == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol required"})
		return
	}

	stockNewsMu.Lock()
	if e, ok := stockNewsCache[sym]; ok && time.Since(e.at) < stockNewsTTL {
		resp := e.resp
		stockNewsMu.Unlock()
		c.JSON(http.StatusOK, resp)
		return
	}
	stockNewsMu.Unlock()

	token := strings.TrimSpace(os.Getenv("FINNHUB_KEY"))
	if token == "" {
		c.JSON(http.StatusOK, gin.H{"symbol": sym, "news": []any{}, "error": "FINNHUB_KEY not set"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	from, to := finnhub.NewsWindow(time.Now().UTC(), 14) // last 2 weeks
	items, err := finnhub.NewClient(token).FetchNews(ctx, sym, from, to, 12)
	if err != nil {
		// Don't cache errors — let the next click retry.
		c.JSON(http.StatusOK, gin.H{"symbol": sym, "news": []any{}, "error": err.Error()})
		return
	}

	resp := gin.H{"symbol": sym, "news": items}
	stockNewsMu.Lock()
	stockNewsCache[sym] = stockNewsCacheEntry{at: time.Now(), resp: resp}
	stockNewsMu.Unlock()
	c.JSON(http.StatusOK, resp)
}
