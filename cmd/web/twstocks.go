package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/twse"
)

// /tw — Taiwan-stock spot fundamental board. TWSE open data is bulk (3 calls
// return the whole market) so this is on-demand with a 1h cache — no cron.

const twCacheTTL = time.Hour
const twMaxRows = 150 // cap the displayed buy+hold list (there are ~900)

var (
	twMu       sync.Mutex
	twCacheAt  time.Time
	twCacheRow []twse.Rated
	twCacheCnt map[string]int
)

func (s *server) handleTWStocksPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	now := time.Now()

	twMu.Lock()
	fresh := twCacheRow != nil && now.Sub(twCacheAt) < twCacheTTL
	twMu.Unlock()

	if !fresh {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		m, err := twse.FetchAll(ctx, nil)
		cancel()
		if err != nil {
			c.HTML(http.StatusOK, "tw.html", gin.H{"Err": "TWSE 資料抓取失敗，稍後再試"})
			return
		}
		rated := twse.RateAll(m)
		cnt := map[string]int{}
		for _, r := range rated {
			cnt[r.Label]++
		}
		twMu.Lock()
		twCacheRow, twCacheCnt, twCacheAt = rated, cnt, now
		twMu.Unlock()
	}

	twMu.Lock()
	rated, cnt, at := twCacheRow, twCacheCnt, twCacheAt
	twMu.Unlock()

	// Display: buy + hold with REAL quality data (HasRev && HasMargin), liquid
	// enough (TradeValue floor cuts illiquid micro-caps where the 2-factor
	// quality is noisiest), ex-financials (different statements). SORT BY
	// TRADE VALUE desc so the large, well-known, liquid names surface first
	// instead of Q100 small-caps burying them.
	const minTradeValue = 1e8 // NT$100M daily turnover
	rows := make([]twse.Rated, 0, len(rated))
	for _, r := range rated {
		if r.Label != "buy" && r.Label != "hold" {
			continue
		}
		if !r.HasRev || !r.HasMargin || r.TradeValue < minTradeValue {
			continue
		}
		if strings.Contains(r.Industry, "金融") || strings.Contains(r.Industry, "保險") {
			continue
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TradeValue > rows[j].TradeValue })
	if len(rows) > twMaxRows {
		rows = rows[:twMaxRows]
	}

	c.HTML(http.StatusOK, "tw.html", gin.H{
		"Rows": rows, "Shown": len(rows),
		"CountBuy": cnt["buy"], "CountHold": cnt["hold"],
		"CountAvoid": cnt["avoid"], "CountUnknown": cnt["unknown"],
		"Updated": at.In(time.FixedZone("Asia/Taipei", 8*3600)).Format("2006-01-02 15:04"),
	})
}
