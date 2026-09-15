// Command contracts lists the BingX perpetual universe, with a filter for the
// US-stock synthetics.
//
// Exists because the synthetic contract codes are NOT guessable — SNDK is
// "NCSKSNDK2USD-USDT" and gold is "NCCOGOLD2USD-USDT", so the prefix varies by
// asset class and the ticker is embedded mid-string. Adding a symbol by writing
// the code you expect is how you get a silently-dead entry that resolves to
// nothing. Same principle as cmd/acct printing raw payloads: read it off the
// wire, don't infer it from a pattern.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/market"
)

func main() {
	filter := flag.String("filter", "", "case-insensitive substring to match against the symbol code")
	stocks := flag.Bool("stocks", false, "only the equity-style synthetics (codes carrying 2USD)")
	all := flag.Bool("all", false, "print every contract")
	vet := flag.String("vet", "", "comma-separated SHORT tickers to vet as candidates: resolves each to its NCSK code, then reports mark price and 90d 1h bar count. Bar count is the liquidity/history screen — a contract can exist and still have no tradeable history.")
	flag.Parse()

	if *vet != "" {
		vetCandidates(*vet)
		return
	}

	config.LoadDotEnv()
	c := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The contracts endpoint is public, but SignedGetRaw is the only raw
	// accessor available and signing a public GET is harmless.
	raw, err := c.SignedGetRaw(ctx, bingx.PathContract, url.Values{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "contracts: %v\n", err)
		os.Exit(1)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}

	type rec struct {
		symbol   string
		status   any
		qtyPrec  any
		pricePre any
		minQty   any
		maxLev   any
	}
	var out []rec
	for _, r := range rows {
		sym, _ := r["symbol"].(string)
		if sym == "" {
			continue
		}
		if *filter != "" && !strings.Contains(strings.ToUpper(sym), strings.ToUpper(*filter)) {
			continue
		}
		if *stocks && !strings.Contains(sym, "2USD") {
			continue
		}
		if !*all && *filter == "" && !*stocks {
			continue
		}
		out = append(out, rec{sym, r["status"], r["quantityPrecision"], r["pricePrecision"], r["minTradeNum"], r["maxLongLeverage"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].symbol < out[j].symbol })
	fmt.Printf("total contracts: %d   shown: %d\n", len(rows), len(out))
	fmt.Printf("%-34s %6s %8s %8s %10s %6s\n", "symbol", "status", "qtyPrec", "pxPrec", "minQty", "maxLev")
	for _, r := range out {
		fmt.Printf("%-34s %6v %8v %8v %10v %6v\n", r.symbol, r.status, r.qtyPrec, r.pricePre, r.minQty, r.maxLev)
	}
}

// vetCandidates resolves each short ticker to its synthetic code and reports
// what actually matters before adding a symbol: does it price, and does it have
// enough history to backtest.
//
// Existence in the contract list is NOT sufficient — 464 stock synthetics are
// listed and many are thin. A symbol with 200 bars in 90 days cannot be A/B'd,
// and adding it would produce confident numbers from nothing.
func vetCandidates(csv string) {
	config.LoadDotEnv()
	c := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	end := time.Now().UTC()
	start := end.AddDate(0, 0, -90)
	fmt.Printf("%-6s %-24s %12s %8s %10s %9s\n", "ticker", "code", "mark", "bars90d", "cover%", "medRange%")
	for _, t := range strings.Split(csv, ",") {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		code := market.Symbol("NCSK" + t + "2USD-USDT")
		mark := 0.0
		if fr, err := c.FundingRate(ctx, code); err == nil {
			mark = fr.MarkPrice
		}
		cs, err := c.KlinesRange(ctx, code, market.Timeframe("1h"), start, end)
		if err != nil {
			fmt.Printf("%-6s %-24s %12s %8s %10s %9s  (klines: %v)\n", t, code, "—", "—", "—", "—", err)
			continue
		}
		// 90d of hourly bars is 2160. Coverage below ~90% means gaps, which
		// for an equity synthetic usually means it only trades US hours.
		cover := float64(len(cs)) / 2160.0 * 100
		var ranges []float64
		for _, k := range cs {
			if k.Low > 0 {
				ranges = append(ranges, (k.High-k.Low)/k.Low*100)
			}
		}
		sort.Float64s(ranges)
		med := 0.0
		if len(ranges) > 0 {
			med = ranges[len(ranges)/2]
		}
		// A recent LISTING and a series full of holes both show up as low
		// coverage but mean opposite things: the first is backtestable on a
		// shorter window, the second is not backtestable at all.
		if len(cs) > 1 {
			firstT := cs[0].OpenTime.Format("01-02")
			lastT := cs[len(cs)-1].OpenTime.Format("01-02")
			gaps, maxGap := 0, 0.0
			span := cs[len(cs)-1].OpenTime.Sub(cs[0].OpenTime).Hours()
			for i := 1; i < len(cs); i++ {
				d := cs[i].OpenTime.Sub(cs[i-1].OpenTime).Hours()
				if d > 1.5 {
					gaps++
					if d > maxGap {
						maxGap = d
					}
				}
			}
			// Coverage WITHIN the listed span, which is the number that says
			// whether the series is continuous.
			inSpan := float64(len(cs)) / (span + 1) * 100
			fmt.Printf("%-6s %-24s %12.4f %8d %9.0f%% %8.3f%%  %s..%s span%.0fd inSpan%.0f%% gaps=%d max=%.0fh\n",
				t, code, mark, len(cs), cover, med, firstT, lastT, span/24, inSpan, gaps, maxGap)
			continue
		}
		fmt.Printf("%-6s %-24s %12.4f %8d %9.0f%% %8.3f%%\n", t, code, mark, len(cs), cover, med)
	}
}
