// acct — read-only dump of the live BingX futures account: open positions and
// wallet balance, straight from the exchange.
//
// Exists because the journal's risk numbers are only as good as the margin
// figures behind them, and under CROSS margin a per-position liquidation
// estimate is meaningless — liquidation depends on total account equity across
// every open position. The typed Position struct in package bingx models only
// what order placement needs, so this prints the RAW payload: the field names
// get read off the wire rather than guessed from documentation.
//
// Signed endpoints require the whitelisted VPS IP, so run it there.
// Prints numbers only — never credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/market"
)

func main() {
	raw := flag.Bool("raw", false, "print the raw JSON payloads verbatim")
	symbols := flag.String("symbols", "BTC,SNDK", "comma-separated short symbols to query")
	flag.Parse()

	config.LoadDotEnv()
	c := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── balance ───────────────────────────────────────────────────────────
	bal, err := c.SignedGetRaw(ctx, bingx.PathBalance, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "balance: %v\n", err)
	} else if *raw {
		fmt.Printf("BALANCE RAW: %s\n\n", bal)
	} else {
		printFlat("BALANCE", bal)
	}

	// ── positions ─────────────────────────────────────────────────────────
	for _, short := range splitCSV(*symbols) {
		sym, ok := shortToSym(short)
		if !ok {
			fmt.Printf("%s: unknown symbol\n", short)
			continue
		}
		q := url.Values{}
		q.Set("symbol", string(sym))
		p, err := c.SignedGetRaw(ctx, bingx.PathPositions, q)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s positions: %v\n", short, err)
			continue
		}
		if *raw {
			fmt.Printf("POSITIONS %s RAW: %s\n\n", short, p)
		} else {
			printFlat("POSITION "+short, p)
		}

		// Resting orders matter as much as the position: a stop that was never
		// accepted is the failure mode /ops/verify exists for, and a journal
		// entry needs the ACTUAL stop, not the one that was intended.
		o, err := c.SignedGetRaw(ctx, bingx.PathOpenOrders, q)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s orders: %v\n", short, err)
			continue
		}
		if *raw {
			fmt.Printf("ORDERS %s RAW: %s\n\n", short, o)
			continue
		}
		printFlat("ORDERS "+short, o)
	}
}

// printFlat renders whatever came back without needing a struct: an object
// prints its keys, an array prints each element. Zero and empty values are
// kept — a missing liquidationPrice is exactly the thing worth seeing.
func printFlat(label string, data json.RawMessage) {
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err == nil {
		if len(arr) == 0 {
			fmt.Printf("%s: (none)\n\n", label)
			return
		}
		for i, m := range arr {
			fmt.Printf("%s [%d]\n", label, i)
			printMap(m)
			fmt.Println()
		}
		return
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err == nil {
		fmt.Printf("%s\n", label)
		// Some endpoints nest the real object one level down (e.g. {"balance":{...}}).
		if len(m) == 1 {
			for _, v := range m {
				if inner, ok := v.(map[string]any); ok {
					printMap(inner)
					fmt.Println()
					return
				}
			}
		}
		printMap(m)
		fmt.Println()
		return
	}
	fmt.Printf("%s: %s\n\n", label, data)
}

func printMap(m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion order is random for maps; sort so successive runs are diffable.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		fmt.Printf("  %-22s %v\n", k, m[k])
	}
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func shortToSym(short string) (market.Symbol, bool) {
	m := map[string]market.Symbol{
		"BTC": market.BTCUSDT, "ETH": market.ETHUSDT,
		"XAU": market.XAUUSDT, "XAG": market.XAGUSDT,
		"SNDK": market.SNDKUSDT, "NVDA": market.NVDAUSDT,
	}
	s, ok := m[short]
	return s, ok
}
