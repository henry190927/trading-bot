// protect — attach a reduce-only stop and/or take-profit to an ALREADY-OPEN
// BingX position, at explicitly given prices.
//
// Why this exists rather than the journal's place_stop / place_tp1 checkboxes:
// those drive off the journal's single Stop field, which is the ANALYSIS stop
// (it defines R and must stay at the level the risk was taken against). A
// profit-locking stop is a different number — 2026-09-03, entry 77,640 with a
// stop trailed to 77,900 — and forcing both into one field would either
// destroy the R baseline or place the wrong order.
//
// Size always comes from the live position, never from a flag: a reduce-only
// order sized by hand is how you end up with a partially-protected position.
//
// Dry by default. --confirm is required to send anything.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/market"
)

func main() {
	symShort := flag.String("symbol", "", "short symbol, e.g. BTC / SNDK")
	stop := flag.Float64("stop", 0, "reduce-only STOP_MARKET trigger price (0 = skip)")
	tp := flag.Float64("tp", 0, "reduce-only LIMIT take-profit price (0 = skip)")
	confirm := flag.Bool("confirm", false, "actually send the orders")
	flag.Parse()

	if *symShort == "" || (*stop == 0 && *tp == 0) {
		fmt.Fprintln(os.Stderr, "usage: protect -symbol BTC -stop 77900 -tp 78900 [-confirm]")
		os.Exit(2)
	}
	sym, ok := shortToSym(*symShort)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown symbol %q\n", *symShort)
		os.Exit(2)
	}

	config.LoadDotEnv()
	c := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	poss, err := c.OpenPositions(ctx, sym)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read positions: %v\n", err)
		os.Exit(1)
	}
	var pos *bingx.Position
	for i := range poss {
		if poss[i].Quantity > 0 {
			pos = &poss[i]
			break
		}
	}
	if pos == nil {
		fmt.Fprintf(os.Stderr, "no open position for %s — nothing to protect\n", *symShort)
		os.Exit(1)
	}

	fr, err := c.FundingRate(ctx, sym)
	if err != nil || fr.MarkPrice <= 0 {
		fmt.Fprintf(os.Stderr, "read mark price: %v\n", err)
		os.Exit(1)
	}
	mark := fr.MarkPrice
	hedge := pos.PositionSide == "LONG" || pos.PositionSide == "SHORT"

	fmt.Printf("position  %s %s qty %g entry %.4f | mark %.4f | lev %dx | posSide %s (hedge=%v)\n",
		*symShort, pos.Side, pos.Quantity, pos.EntryPrice, mark, pos.Leverage, pos.PositionSide, hedge)

	// Sanity is checked against the MARK, not the entry. A stop above entry on
	// a long is a legitimate profit-lock; a stop above the MARK is an order
	// that fires the instant it lands and closes at market. Those are entirely
	// different mistakes and only the second one is a mistake.
	if *stop > 0 {
		if pos.Side == "long" && *stop >= mark {
			fmt.Fprintf(os.Stderr, "REFUSED stop %.4f >= mark %.4f on a long — would trigger immediately\n", *stop, mark)
			os.Exit(1)
		}
		if pos.Side == "short" && *stop <= mark {
			fmt.Fprintf(os.Stderr, "REFUSED stop %.4f <= mark %.4f on a short — would trigger immediately\n", *stop, mark)
			os.Exit(1)
		}
		lock := ""
		if (pos.Side == "long" && *stop > pos.EntryPrice) || (pos.Side == "short" && *stop < pos.EntryPrice) {
			locked := (*stop - pos.EntryPrice) * pos.Quantity
			if pos.Side == "short" {
				locked = -locked
			}
			lock = fmt.Sprintf("  [locks in ~%+.2f USDT]", locked)
		}
		fmt.Printf("  STOP_MARKET reduce-only  qty %g  trigger %.4f%s\n", pos.Quantity, *stop, lock)
	}
	if *tp > 0 {
		if pos.Side == "long" && *tp <= mark {
			fmt.Fprintf(os.Stderr, "REFUSED tp %.4f <= mark %.4f on a long — would fill immediately at market-ish\n", *tp, mark)
			os.Exit(1)
		}
		if pos.Side == "short" && *tp >= mark {
			fmt.Fprintf(os.Stderr, "REFUSED tp %.4f >= mark %.4f on a short — would fill immediately\n", *tp, mark)
			os.Exit(1)
		}
		gain := (*tp - pos.EntryPrice) * pos.Quantity
		if pos.Side == "short" {
			gain = -gain
		}
		fmt.Printf("  LIMIT reduce-only        qty %g  price   %.4f  [~%+.2f USDT if filled]\n", pos.Quantity, *tp, gain)
	}

	if !*confirm {
		fmt.Println("\nDRY RUN — nothing sent. Re-run with -confirm to place.")
		return
	}

	if *stop > 0 {
		res, err := c.PlaceStopMarket(ctx, sym, pos.Side, pos.Quantity, *stop, hedge)
		if err != nil {
			fmt.Fprintf(os.Stderr, "place stop: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("SENT stop  orderId=%s\n", res.OrderID)
	}
	if *tp > 0 {
		res, err := c.PlaceReduceOnlyLimit(ctx, sym, pos.Side, pos.Quantity, *tp, hedge)
		if err != nil {
			fmt.Fprintf(os.Stderr, "place tp: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("SENT tp    orderId=%s\n", res.OrderID)
	}
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
