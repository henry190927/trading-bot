// Command earnings-fetch pulls the earnings calendar for the US-stock synthetic
// symbols under evaluation and writes /opt/trading/earnings.json (the file the
// earnings package loads for the blackout gate). Run daily via cron on the VPS,
// or once locally to seed a fixture for the backtest A/B.
//
// Reads FINNHUB_KEY from the environment (systemd EnvironmentFile on the VPS;
// `set -a; . .env` locally). The key is never logged. On the VPS, chown the
// output back to ubuntu:ubuntu after running (the web/monitor need write access).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"myFirstGo/trading-bot/earnings/finnhub"
)

func main() {
	log.SetFlags(0)
	var (
		out     = flag.String("out", envOr("EARNINGS_FILE", "/opt/trading/earnings.json"), "output earnings.json path")
		symsCSV = flag.String("symbols", envOr("EARNINGS_SYMBOLS", "SNDK,NVDA"), "comma-separated tickers")
		fromDays = flag.Int("from-days", 420, "history start = today - N days (wide for backtest, narrow for cron)")
		toDays   = flag.Int("to-days", 120, "horizon end = today + N days")
	)
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("FINNHUB_KEY"))
	if token == "" {
		log.Fatal("FINNHUB_KEY not set (systemd EnvironmentFile on VPS; `set -a; . .env` locally)")
	}

	now := time.Now().UTC()
	from := now.AddDate(0, 0, -*fromDays).Format("2006-01-02")
	to := now.AddDate(0, 0, *toDays).Format("2006-01-02")

	tickers := splitTrim(*symsCSV)
	if len(tickers) == 0 {
		log.Fatal("no symbols to fetch")
	}

	c := finnhub.NewClient(token)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var all []finnhub.Event
	var okN, failN int
	for _, tk := range tickers {
		evts, err := c.FetchSymbol(ctx, tk, from, to)
		if err != nil {
			log.Printf("WARN fetch %s: %v", tk, err) // never logs the token
			failN++
			continue
		}
		all = append(all, evts...)
		okN++
		log.Printf("fetched %-6s %d events", tk, len(evts))
	}

	// Total failure → keep the existing file (stale beats empty = "no blackout ever").
	if okN == 0 {
		log.Fatalf("all %d symbols failed; leaving %s untouched", failN, *out)
	}

	data, err := finnhub.BuildFile(all, now)
	if err != nil {
		log.Fatalf("build file: %v", err)
	}
	if err := finnhub.WriteFileAtomic(*out, data); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s: %d events from %d/%d symbols (%s..%s)", *out, len(all), okN, okN+failN, from, to)
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
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, strings.ToUpper(s))
		}
	}
	return out
}
