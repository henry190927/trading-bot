// Command price prints the current market price for all 4 symbols.
// Designed for sub-second response from iPhone Terminal# via the `tm` alias.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"myFirstGo/trading/ansi"
	"myFirstGo/trading/bingx"
	"myFirstGo/trading/config"
	"myFirstGo/trading/market"
)

type row struct {
	short  string
	price  float64
	prev   float64
	err    string
}

func main() {
	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows := make([]row, len(market.All()))
	var wg sync.WaitGroup
	for i, sym := range market.All() {
		wg.Add(1)
		go func(idx int, s market.Symbol) {
			defer wg.Done()
			r := row{short: shortSymbol(s)}
			// Two 1m candles: current + previous, to compute direction.
			candles, err := client.Klines(ctx, s, market.TF1m, 2)
			if err != nil {
				r.err = err.Error()
				rows[idx] = r
				return
			}
			if len(candles) < 1 {
				r.err = "no data"
				rows[idx] = r
				return
			}
			r.price = candles[len(candles)-1].Close
			if len(candles) >= 2 {
				r.prev = candles[len(candles)-2].Close
			}
			rows[idx] = r
		}(i, sym)
	}
	wg.Wait()

	for _, r := range rows {
		if r.err != "" {
			fmt.Printf("%-4s  %s\n", r.short, ansi.Wrap(r.err, ansi.Red))
			continue
		}
		var arrow, pctStr, code string
		if r.prev > 0 {
			pct := (r.price - r.prev) / r.prev * 100
			switch {
			case pct > 0:
				arrow, code = "▲", ansi.Green
			case pct < 0:
				arrow, code = "▼", ansi.Red
			default:
				arrow, code = "·", ansi.Dim
			}
			pctStr = fmt.Sprintf("%s %+.3f%%", arrow, pct)
		}
		fmt.Printf("%-4s  %14.4f  %s\n",
			r.short, r.price, ansi.Wrap(pctStr, code))
	}
}

func shortSymbol(s market.Symbol) string {
	switch s {
	case market.XAUUSDT:
		return "XAU"
	case market.XAGUSDT:
		return "XAG"
	case market.BTCUSDT:
		return "BTC"
	case market.ETHUSDT:
		return "ETH"
	}
	return string(s)
}
