// Command fundamental-scan scores the exchange's US-stock synthetic universe
// (BingX NCSK*2USD-USDT) into fundamentals.json for the /fundamentals board —
// consumer B's auto-discovery. Run daily via cron on the VPS. Reads FINNHUB_KEY
// from the environment (never logged); rate-limited to respect the free tier.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/earnings/finnhub"
	"github.com/henry190927/trading-bot/fundamental"
)

const bingxContractsURL = "https://open-api.bingx.com/openApi/swap/v2/quote/contracts"

func main() {
	log.SetFlags(0)
	var (
		out     = flag.String("out", envOr("FUNDAMENTALS_FILE", "/opt/trading/fundamentals.json"), "output path")
		symsCSV = flag.String("symbols", "", "comma-separated tickers (overrides the NCSK universe scan)")
		limit   = flag.Int("limit", 0, "cap number of symbols (0 = all; for testing)")
		sleepMS = flag.Int("sleep-ms", 1100, "delay between Finnhub calls (~54/min at 1100)")
		minCap  = flag.Float64("min-market-cap", 1000, "skip symbols below this market cap (Finnhub units = USD millions; 1000 = $1B); 0 disables")
	)
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("FINNHUB_KEY"))
	if token == "" {
		log.Fatal("FINNHUB_KEY not set")
	}

	var tickers []string
	if *symsCSV != "" {
		tickers = splitTrim(*symsCSV)
	} else {
		u, err := ncskUniverse()
		if err != nil {
			log.Fatalf("universe: %v", err)
		}
		tickers = u
	}
	if *limit > 0 && *limit < len(tickers) {
		tickers = tickers[:*limit]
	}
	log.Printf("scanning %d tickers (sleep %dms)", len(tickers), *sleepMS)

	// Previous scan's labels → PrevLabel on each new entry for downgrade
	// detection (the fundamental sell signal). Missing/first-run → empty.
	prevLabel := loadPrevLabels(*out)

	c := finnhub.NewClient(token)
	var entries []fundamental.BoardEntry
	var scored, skipped, tooSmall int
	for i, tk := range tickers {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		m, err := c.FetchMetrics(ctx, tk)
		cancel()
		if err != nil {
			skipped++
		} else if m.MarketCap == 0 && m.PE == 0 && m.PS == 0 && m.RevGrowthYoY == 0 && m.NetMargin == 0 {
			// No fundamentals (private co like ANTHROPIC/OPENAI, delisted, or an
			// ETF Finnhub doesn't cover) — drop rather than list as "unknown".
			skipped++
		} else if *minCap > 0 && m.MarketCap > 0 && m.MarketCap < *minCap {
			// Micro-cap: pure-metric scoring is noise here (distorted P/E,
			// triple-digit growth off a tiny base). Drop below the floor.
			tooSmall++
		} else {
			r := fundamental.Score(*m)
			entries = append(entries, fundamental.BoardEntry{
				Rating: r, PrevLabel: prevLabel[r.Symbol], PE: m.PE, PS: m.PS,
				RevGrowthYoY: m.RevGrowthYoY, NetMargin: m.NetMargin, DebtToEquity: m.DebtToEquity,
			})
			scored++
		}
		if i < len(tickers)-1 {
			time.Sleep(time.Duration(*sleepMS) * time.Millisecond)
		}
		if (i+1)%50 == 0 {
			log.Printf("  ...%d/%d (scored %d, skipped %d)", i+1, len(tickers), scored, skipped)
		}
	}

	// Sort: buy → hold → avoid → unknown, then quality desc.
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := fundamental.LabelRank(entries[i].Label), fundamental.LabelRank(entries[j].Label)
		if ri != rj {
			return ri < rj
		}
		return entries[i].Quality > entries[j].Quality
	})

	board := fundamental.Board{
		UpdatedUTC: time.Now().UTC().Format(time.RFC3339),
		Entries:    entries,
	}
	data, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	if scored == 0 {
		log.Fatalf("scored 0 symbols; leaving %s untouched", *out)
	}
	if err := writeAtomic(*out, data); err != nil {
		log.Fatalf("write: %v", err)
	}
	cnt := board.Counts()
	log.Printf("wrote %s: %d scored, %d skipped, %d below $%.0fM cap | buy=%d hold=%d avoid=%d unknown=%d",
		*out, scored, skipped, tooSmall, *minCap, cnt["buy"], cnt["hold"], cnt["avoid"], cnt["unknown"])
}

// ncskUniverse fetches BingX contracts and returns the bare tickers of the
// NCSK*2USD-USDT stock synthetics.
func ncskUniverse() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bingxContractsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			Symbol string `json:"symbol"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	const pre, suf = "NCSK", "2USD-USDT"
	var out []string
	for _, c := range body.Data {
		s := c.Symbol
		if strings.HasPrefix(s, pre) && strings.HasSuffix(s, suf) && len(s) > len(pre)+len(suf) {
			out = append(out, s[len(pre):len(s)-len(suf)])
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadPrevLabels reads the existing board (if any) into symbol→label for
// downgrade detection on the next scan. Best-effort: missing/unreadable → empty.
func loadPrevLabels(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var b fundamental.Board
	if json.Unmarshal(data, &b) != nil {
		return out
	}
	for _, e := range b.Entries {
		out[e.Symbol] = e.Label
	}
	return out
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func splitTrim(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if s := strings.TrimSpace(strings.ToUpper(p)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
