package main

import (
	"context"
	"encoding/json"
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
// Two modes:
//   - PRECOMPUTED (preferred): reads fundamentals.json written daily by
//     cmd/fundamental-scan over the whole BingX NCSK universe (~400 symbols).
//     Shows the auto-discovered buy/hold candidates + counts.
//   - ON-DEMAND fallback (no scan file): fetches a small FUNDAMENTAL_SYMBOLS
//     list live with a 6h cache.

const fundCacheTTL = 6 * time.Hour

func fundamentalsPath() string {
	if p := strings.TrimSpace(os.Getenv("FUNDAMENTALS_FILE")); p != "" {
		return p
	}
	return "/opt/trading/fundamentals.json"
}

type fundCacheEntry struct {
	row fundRow
	at  time.Time
}

var (
	fundMu    sync.Mutex
	fundCache = map[string]fundCacheEntry{}
)

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

// fundRow is a flat display row usable from either the precomputed board or an
// on-demand fetch.
type fundRow struct {
	Symbol       string
	Label        string
	Quality      float64
	Valuation    float64
	Growth       float64
	Profitability float64
	BalanceSheet float64
	Confidence   string
	Note         string
	PE           float64
	PS           float64
	RevGrowthYoY float64
	NetMargin    float64
	DebtToEquity float64
	NextEarnings *earnings.Event
	DaysToER     int
	Err          string
}

func firstNote(ns []string) string {
	if len(ns) > 0 {
		return ns[0]
	}
	return ""
}

func rowFromRating(r fundamental.Rating, pe, ps, revG, netM, de float64) fundRow {
	return fundRow{
		Symbol: r.Symbol, Label: r.Label, Quality: r.Quality, Valuation: r.Valuation,
		Growth: r.Growth, Profitability: r.Profitability, BalanceSheet: r.BalanceSheet,
		Confidence: r.Confidence, Note: firstNote(r.Notes),
		PE: pe, PS: ps, RevGrowthYoY: revG, NetMargin: netM, DebtToEquity: de,
	}
}

func attachNextER(row *fundRow, now time.Time) {
	if ev := earnings.Default().NextUpcoming(row.Symbol, now, 120*24*time.Hour); ev != nil {
		row.NextEarnings = ev
		row.DaysToER = int(ev.DatetimeUTC.Sub(now).Hours() / 24)
	}
}

func ratingRank(label string) int {
	switch label {
	case "buy":
		return 0
	case "hold":
		return 1
	case "unknown":
		return 3
	default:
		return 2
	}
}

func (s *server) handleFundamentalsPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	now := time.Now().UTC()

	// PRECOMPUTED mode — read the daily scan if present.
	if data, err := os.ReadFile(fundamentalsPath()); err == nil {
		var board fundamental.Board
		if json.Unmarshal(data, &board) == nil && len(board.Entries) > 0 {
			rows := make([]fundRow, 0, len(board.Entries))
			for _, e := range board.Entries {
				if e.Label != "buy" && e.Label != "hold" {
					continue // hide avoid/unknown from the board; counts still shown
				}
				row := rowFromRating(e.Rating, e.PE, e.PS, e.RevGrowthYoY, e.NetMargin, e.DebtToEquity)
				attachNextER(&row, now)
				rows = append(rows, row)
			}
			cnt := board.Counts()
			c.HTML(http.StatusOK, "fundamentals.html", gin.H{
				"Rows": rows, "Updated": board.UpdatedUTC, "NoKey": false,
				"Scanned": len(board.Entries), "Mode": "universe",
				"CountBuy": cnt["buy"], "CountHold": cnt["hold"],
				"CountAvoid": cnt["avoid"], "CountUnknown": cnt["unknown"],
			})
			return
		}
	}

	// ON-DEMAND fallback — small list, live fetch, 6h cache.
	token := strings.TrimSpace(os.Getenv("FINNHUB_KEY"))
	syms := fundamentalSymbols()
	client := finnhub.NewClient(token)
	rows := make([]fundRow, 0, len(syms))
	for _, tk := range syms {
		if token == "" {
			rows = append(rows, fundRow{Symbol: tk, Err: "FINNHUB_KEY not set"})
			continue
		}
		fundMu.Lock()
		ent, ok := fundCache[tk]
		fresh := ok && now.Sub(ent.at) < fundCacheTTL
		fundMu.Unlock()
		if !fresh {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			m, err := client.FetchMetrics(ctx, tk)
			cancel()
			var row fundRow
			if err != nil {
				row = fundRow{Symbol: tk, Err: "fetch failed"}
			} else {
				row = rowFromRating(fundamental.Score(*m), m.PE, m.PS, m.RevGrowthYoY, m.NetMargin, m.DebtToEquity)
			}
			ent = fundCacheEntry{row: row, at: now}
			fundMu.Lock()
			fundCache[tk] = ent
			fundMu.Unlock()
		}
		row := ent.row
		attachNextER(&row, now)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := ratingRank(rows[i].Label), ratingRank(rows[j].Label)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Quality > rows[j].Quality
	})
	c.HTML(http.StatusOK, "fundamentals.html", gin.H{
		"Rows": rows, "Updated": now.Format("2006-01-02 15:04 UTC"),
		"NoKey": token == "", "Mode": "ondemand",
	})
}
