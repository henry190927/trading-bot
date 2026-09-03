// Command gate runs an A/B result through the ship gate.
//
// The gate used to be prose in a harness comment, applied by hand at the end
// of a run — which is how SUI 1h shipped while losing money in every window,
// and how a -0.119 R/trade arm got described as "winning". A verdict you have
// to remember to compute is a verdict that gets skipped when it is 1am.
//
// Input is one line per window, "days netR trades [winRate]", on stdin or via
// --arm / --base. Everything is explicit: there is no default baseline, and an
// arm with no baseline is judged on absolute grounds only, which is correct
// for a brand-new strategy and wrong for a variant.
//
// Usage:
//
//	gate --arm "60 -2.44 9, 90 -2.44 15, 120 -0.69 20" \
//	     --base "60 -6.10 34, 90 -8.13 49, 120 -18.05 64" \
//	     --name "SUI 1h struct-momentum"
//
//	# or piped, one window per line, arm and baseline split by a blank line
//	printf '60 3.65 14\n90 2.48 18\n120 8.47 23\n' | gate --name "SOL 1h sm"
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"myFirstGo/trading-bot/shipgate"
)

func main() {
	name := flag.String("name", "arm", "label for the arm under test")
	armS := flag.String("arm", "", `arm windows: "days netR trades[, ...]"`)
	baseS := flag.String("base", "", `baseline windows, same format; omit for an absolute-only judgement`)
	minTrades := flag.Int("min-trades", 0, "override the sample-size floor (0 = package default)")
	minRPT := flag.Float64("min-r-per-trade", 0, "override the absolute median R/trade floor")
	flag.Parse()

	c := shipgate.Default()
	if *minTrades > 0 {
		c.MinTradesPerWindow = *minTrades
	}
	// A negative floor is a deliberate choice ("allow a small loser"), so it
	// must be settable, which means testing against the flag's presence rather
	// than its value.
	if isSet("min-r-per-trade") {
		c.MinRPerTradeMedian = *minRPT
	}

	arm := shipgate.Arm{Name: *name}
	var err error
	if strings.TrimSpace(*armS) != "" {
		arm.Windows, err = parse(*armS)
	} else {
		arm.Windows, err = parseStdin()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "arm: %v\n", err)
		os.Exit(2)
	}
	if len(arm.Windows) == 0 {
		fmt.Fprintln(os.Stderr, "no windows given; see --help")
		os.Exit(2)
	}

	var base *shipgate.Arm
	if strings.TrimSpace(*baseS) != "" {
		ws, perr := parse(*baseS)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "base: %v\n", perr)
			os.Exit(2)
		}
		base = &shipgate.Arm{Name: "baseline", Windows: ws}
	}

	v := shipgate.Evaluate(arm, base, c)
	fmt.Println(v)
	for _, w := range arm.Windows {
		fmt.Printf("    %4dd  netR %+8.2f  n=%-5d  R/trade %+0.3f\n", w.Days, w.NetR, w.Trades, w.RPerTrade())
	}
	if base == nil {
		fmt.Println("    (no baseline given — judged on absolute grounds only)")
	}
	// Non-zero exit on FAIL so this can gate a script.
	if v.Result == shipgate.Fail {
		os.Exit(1)
	}
	if v.Result == shipgate.Undecided {
		os.Exit(3)
	}
}

func isSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func parse(s string) ([]shipgate.Window, error) {
	var out []shipgate.Window
	for _, chunk := range strings.Split(s, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		w, err := parseOne(chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func parseStdin() ([]shipgate.Window, error) {
	var out []shipgate.Window
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		w, err := parseOne(line)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, sc.Err()
}

func parseOne(s string) (shipgate.Window, error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return shipgate.Window{}, fmt.Errorf("want %q, got %q", "days netR trades [winRate]", s)
	}
	days, err := strconv.Atoi(f[0])
	if err != nil {
		return shipgate.Window{}, fmt.Errorf("days %q: %w", f[0], err)
	}
	netR, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return shipgate.Window{}, fmt.Errorf("netR %q: %w", f[1], err)
	}
	trades, err := strconv.Atoi(f[2])
	if err != nil {
		return shipgate.Window{}, fmt.Errorf("trades %q: %w", f[2], err)
	}
	w := shipgate.Window{Days: days, NetR: netR, Trades: trades}
	if len(f) > 3 {
		if wr, e := strconv.ParseFloat(strings.TrimSuffix(f[3], "%"), 64); e == nil {
			w.WinRate = wr
		}
	}
	return w, nil
}
