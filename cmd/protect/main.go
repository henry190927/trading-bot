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
// The rules (size from the live position; sanity against the MARK, not the
// entry) live in package protect, shared with POST /ops/protect. This binary
// is now just a CLI front-end: it exists for the cases where a browser is not
// to hand, and it cannot drift from the button.
//
// Dry by default. --confirm is required to send anything.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/protect"
)

func main() {
	symShort := flag.String("symbol", "", "short symbol, e.g. BTC / ETH / SNDK / MSTR")
	side := flag.String("side", "", "position side to protect: long | short (blank = whichever is open)")
	stop := flag.Float64("stop", 0, "reduce-only STOP_MARKET trigger price (0 = skip)")
	tp := flag.Float64("tp", 0, "reduce-only LIMIT take-profit price (0 = skip)")
	qty := flag.Float64("qty", 0, "protect only this much of the position (0 = the whole thing)")
	confirm := flag.Bool("confirm", false, "actually send the orders")
	flag.Parse()

	if *symShort == "" || (*stop == 0 && *tp == 0) {
		fmt.Fprintln(os.Stderr, "usage: protect -symbol BTC -stop 77900 -tp 78900 [-side short] [-qty 0.0307] [-confirm]")
		os.Exit(2)
	}
	// Single source of truth for the symbol table, now market.Resolve rather
	// than zone.ShortToSym. The local map had gone stale first (it never
	// learned SPCX/MSTR/APP), and zone.ShortToSym then went stale the same way
	// against the five crypto alts — so protecting a SOL/LINK/SUI/HYPE/NEAR
	// position from the CLI was impossible while the web could do it. Two
	// tables, two chances to be missing the symbol you need at the moment you
	// need it. zone.ShortToSym stays where it is: it gates what a MANUAL zone
	// may name, which is a different question from what a position may be.
	sym, err := market.ResolveErr(*symShort)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	config.LoadDotEnv()
	c := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pos, err := findPosition(ctx, c, sym, *side)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read positions: %v\n", err)
		os.Exit(1)
	}

	fr, err := c.FundingRate(ctx, sym)
	if err != nil || fr.MarkPrice <= 0 {
		fmt.Fprintf(os.Stderr, "read mark price: %v\n", err)
		os.Exit(1)
	}

	// -qty exists for the scale-out case the web buttons cannot express: two
	// reduce-only targets at different prices, each over part of the position.
	// The web TP1/TP2 pair derives its split from a percentage and rounds it
	// itself; here the caller states the lot size outright, because the two
	// halves of an odd quantity are not equal and only the caller knows which
	// side the odd step belongs on. It never grows the order: a -qty above the
	// live position is a typo, not a request to sell what is not held.
	if *qty > 0 {
		if pos == nil {
			fmt.Fprintln(os.Stderr, "-qty given but there is no open position")
			os.Exit(1)
		}
		if *qty > pos.Quantity {
			fmt.Fprintf(os.Stderr, "-qty %g exceeds the live position %g\n", *qty, pos.Quantity)
			os.Exit(1)
		}
		part := *pos
		part.Quantity = *qty
		pos = &part
	}

	plan := protect.BuildPlan(pos, fr.MarkPrice, *stop, *tp)
	if !plan.OK() {
		for _, f := range plan.Faults {
			fmt.Fprintf(os.Stderr, "REFUSED %s\n", f)
		}
		os.Exit(1)
	}

	fmt.Printf("position  %s %s qty %g entry %.4f | mark %.4f | lev %dx | hedge=%v\n",
		*symShort, plan.Side, plan.Qty, plan.Entry, plan.Mark, plan.Leverage, plan.Hedge)
	if plan.Stop > 0 {
		lock := ""
		if plan.StopLocksUSDT != 0 {
			lock = fmt.Sprintf("  [locks in ~%+.2f USDT]", plan.StopLocksUSDT)
		}
		fmt.Printf("  STOP_MARKET reduce-only  qty %g  trigger %.4f%s\n", plan.Qty, plan.Stop, lock)
	}
	if plan.TP > 0 {
		fmt.Printf("  LIMIT reduce-only        qty %g  price   %.4f  [~%+.2f USDT if filled]\n",
			plan.Qty, plan.TP, plan.TPGainUSDT)
	}

	if !*confirm {
		fmt.Println("\nDRY RUN — nothing sent. Re-run with -confirm to place.")
		return
	}

	res := protect.Apply(ctx, c, plan)
	if res.StopOrderID != "" {
		fmt.Printf("SENT stop  orderId=%s\n", res.StopOrderID)
	}
	if res.TPOrderID != "" {
		fmt.Printf("SENT tp    orderId=%s\n", res.TPOrderID)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "%s\n", e)
	}
	if len(res.Errors) > 0 {
		// Exit non-zero even on a partial send, so a script cannot read
		// "stop landed, tp failed" as success.
		os.Exit(1)
	}
	fmt.Println("\nnow re-check /ops/verify — an orderId is not proof the exchange is holding it")
}

// findPosition returns the open position to protect. With -side given it asks
// for that leg (hedge accounts can hold both); without, it takes whichever
// non-zero position exists.
func findPosition(ctx context.Context, c *bingx.Client, sym market.Symbol, side string) (*bingx.Position, error) {
	if side != "" {
		return c.FindOpenPosition(ctx, sym, side)
	}
	poss, err := c.OpenPositions(ctx, sym)
	if err != nil {
		return nil, err
	}
	for i := range poss {
		if poss[i].Quantity > 0 {
			return &poss[i], nil
		}
	}
	return nil, nil // BuildPlan turns this into a readable fault
}
