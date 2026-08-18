package main

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/earnings"
	"myFirstGo/trading-bot/earnings/finnhub"
	"myFirstGo/trading-bot/fundamental"
)

// Consumer B surface: /fundamentals — a standalone spot buy/hold/avoid board.
// Fundamentals move slowly, so ratings are cached per-ticker for 6h to respect
// Finnhub's free-tier RPM and keep the page snappy.

const fundCacheTTL = 6 * time.Hour

type fundCacheEntry struct {
	rating  fundamental.Rating
	metrics *finnhub.Metrics
	at      time.Time
	err     string
}

var (
	fundMu    sync.Mutex
	fundCache = map[string]fundCacheEntry{}
)

// fundamentalSymbols is the ticker list for the board (bare tickers, not
// synthetics). Override via FUNDAMENTAL_SYMBOLS.
func fundamentalSymbols() []string {
	def := "NVDA,SNDK,MU,TSLA,KO"
	if v := strings.TrimSpace(os.Getenv("FUNDAMENTAL_SYMBOLS")); v != "" {
		def = v
	}
	var out []string
	for _, p := range strings.Split(def, ",") {
		if s := strings.TrimSpace(strings.ToUpper(p)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type fundRow struct {
	Symbol       string
	Rating       fundamental.Rating
	Metrics      *finnhub.Metrics
	NextEarnings *earnings.Event
	DaysToER     int
	Err          string
}

// ratingRank orders the board: buy, then hold, then unknown/avoid last.
func ratingRank(label string) int {
	switch label {
	case "buy":
		return 0
	case "hold":
		return 1
	case "unknown":
		return 3
	default: // avoid
		return 2
	}
}

func (s *server) handleFundamentalsPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	token := strings.TrimSpace(os.Getenv("FINNHUB_KEY"))
	syms := fundamentalSymbols()
	now := time.Now().UTC()

	rows := make([]fundRow, 0, len(syms))
	client := finnhub.NewClient(token)
	for _, tk := range syms {
		row := fundRow{Symbol: tk}
		if token == "" {
			row.Err = "FINNHUB_KEY not set"
			rows = append(rows, row)
			continue
		}
		// cache
		fundMu.Lock()
		ent, ok := fundCache[tk]
		fresh := ok && now.Sub(ent.at) < fundCacheTTL
		fundMu.Unlock()
		if !fresh {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			m, err := client.FetchMetrics(ctx, tk)
			cancel()
			ent = fundCacheEntry{at: now}
			if err != nil {
				ent.err = "fetch failed"
			} else {
				ent.metrics = m
				ent.rating = fundamental.Score(*m)
			}
			fundMu.Lock()
			fundCache[tk] = ent
			fundMu.Unlock()
		}
		row.Rating = ent.rating
		row.Metrics = ent.metrics
		row.Err = ent.err
		// next earnings (consumer B awareness) from the shared earnings calendar
		if ev := earnings.Default().NextUpcoming(tk, now, 120*24*time.Hour); ev != nil {
			row.NextEarnings = ev
			row.DaysToER = int(ev.DatetimeUTC.Sub(now).Hours() / 24)
		}
		rows = append(rows, row)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := ratingRank(rows[i].Rating.Label), ratingRank(rows[j].Rating.Label)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Rating.Quality > rows[j].Rating.Quality
	})

	c.HTML(http.StatusOK, "fundamentals.html", gin.H{
		"Rows":    rows,
		"Updated": now.Format("2006-01-02 15:04 UTC"),
		"NoKey":   token == "",
	})
}
