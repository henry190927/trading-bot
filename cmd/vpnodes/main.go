// Command vpnodes prints the volume-profile nodes FindNodes cuts from live
// closed candles, so the split can be checked by eye against a chart before
// any backtest is built on it.
//
//	go run ./cmd/vpnodes -symbols BTC,ETH -tf 1h -lookback 200
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func main() {
	symbols := flag.String("symbols", "BTC,ETH,XAU,XAG", "comma-separated short names")
	tfStr := flag.String("tf", "1h", "candle timeframe")
	lookback := flag.Int("lookback", 200, "closed candles in the profile")
	bins := flag.Int("bins", 80, "price bins in the profile (engine uses 80)")
	hist := flag.Bool("hist", false, "also print every bin as a bar, marking node bins")
	flag.Parse()

	client := bingx.New("", "") // klines are public
	tf := market.Timeframe(*tfStr)
	opts := indicator.DefaultNodeOpts()

	for _, short := range strings.Split(*symbols, ",") {
		sym, err := market.ResolveErr(short)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		cs, err := client.Klines(context.Background(), sym, tf, *lookback)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", short, err)
			continue
		}
		vp := indicator.BuildVolumeProfile(cs, *bins, 5)
		nodes := indicator.FindNodes(vp, opts)
		last := cs[len(cs)-1]
		utc8 := time.FixedZone("UTC+8", 8*3600)

		fmt.Printf("\n%s %s · %d bars %s → %s (UTC+8) · last close %s · profile POC %s\n",
			strings.ToUpper(short), tf, len(cs),
			cs[0].OpenTime.In(utc8).Format("01-02 15:04"), last.OpenTime.In(utc8).Format("01-02 15:04"),
			fmtP(last.Close), fmtP(vp.POC))
		fmt.Printf("  %-3s %12s %12s %12s %8s  %s\n", "#", "low", "high", "poc", "vol%", "")
		total := 0.0
		for _, b := range vp.Bins {
			total += b.Volume
		}
		for i, n := range nodes {
			where := ""
			switch {
			case last.Close >= n.Low && last.Close < n.High:
				where = "← price inside"
			case last.Close >= n.High:
				where = "below price"
			default:
				where = "above price"
			}
			fmt.Printf("  %-3d %12s %12s %12s %7.1f%%  %s\n", i+1, fmtP(n.Low), fmtP(n.High), fmtP(n.POC), 100*n.Volume/total, where)
		}
		if *hist {
			printHist(vp, nodes)
		}
	}
}

func printHist(vp indicator.VolumeProfile, nodes []indicator.Node) {
	maxV := 0.0
	for _, b := range vp.Bins {
		maxV = max(maxV, b.Volume)
	}
	for i := len(vp.Bins) - 1; i >= 0; i-- {
		b := vp.Bins[i]
		mark := " "
		for k, n := range nodes {
			if b.PriceMid >= n.Low && b.PriceMid < n.High {
				mark = fmt.Sprint(k + 1)
			}
		}
		bar := strings.Repeat("█", int(40*b.Volume/maxV+0.5))
		fmt.Printf("  %12s %s %s\n", fmtP(b.PriceMid), mark, bar)
	}
}

func fmtP(p float64) string {
	switch {
	case p >= 1000:
		return fmt.Sprintf("%.0f", p)
	case p >= 10:
		return fmt.Sprintf("%.2f", p)
	default:
		return fmt.Sprintf("%.4f", p)
	}
}
