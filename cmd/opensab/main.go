// Command opensab A/B-tests session-open FILTERS on the shipped sweep-reject
// edge, at fire-generation time, and puts each arm through shipgate.
//
// WHY THIS EXISTS, and why cmd/openbt is not enough. openbt generates every
// fire, dedups, and THEN buckets the resulting positions by open-alignment.
// That is a post-hoc subgroup split, and under one-position-per-rule dedup a
// subgroup's R does not transfer to a rule that trades only that subgroup:
// removing a fire frees the slot and resets the cooldown, so different LATER
// fires become positions. The "mixed" bucket reading +29 to +38R across
// windows is therefore NOT an estimate of what a mixed-only rule would have
// earned — it is an estimate of how the mixed subset of an unfiltered book
// behaved, which is a different quantity.
//
// So: apply the filter INSIDE fire generation, dedup each arm independently,
// and compare. Arms:
//
//	baseline    every sweep-reject fire (the shipped rule)
//	mixed       only fires with entry BETWEEN the daily and weekly open
//	dw-aligned  only fires with entry beyond BOTH opens in the trade's direction
//	mo-aligned  only fires with entry beyond the MONTHLY open in that direction
//
// mo-aligned is the Phase-2 "opens as a scoring factor" question, measured
// rather than argued.
//
// FAIRNESS NOTE: a fire is eligible for EVERY arm only when the opens it
// needs are computable from the window (ComputeOpens returns 0 when the slice
// starts after the boundary — true for early bars, and for the monthly open
// through most of a 60d window). Baseline applies the same eligibility test,
// so the arms are compared over an identical candidate set rather than
// baseline silently getting extra trades the filters could never have seen.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
	"github.com/henry190927/trading-bot/signal"
)

func alignRight(arr []float64, n int) []float64 {
	if len(arr) == n {
		return arr
	}
	out := make([]float64, n)
	off := n - len(arr)
	for i := range out {
		if i < off {
			if len(arr) > 0 {
				out[i] = arr[0]
			}
		} else {
			out[i] = arr[i-off]
		}
	}
	return out
}

// eligible reports whether every open this study needs is resolvable at bar i.
// Applied to all arms including baseline — see FAIRNESS NOTE.
func eligible(o signal.Opens) bool {
	return o.Daily != 0 && o.Weekly != 0 && o.Monthly != 0
}

// keep is an arm's filter: given the entry price, side and the opens at fire
// time, may this fire be emitted?
type keep func(px float64, long bool, o signal.Opens) bool

var arms = []struct {
	name string
	f    keep
}{
	{"baseline", func(float64, bool, signal.Opens) bool { return true }},
	{"mixed", func(px float64, _ bool, o signal.Opens) bool {
		lo, hi := o.Daily, o.Weekly
		if lo > hi {
			lo, hi = hi, lo
		}
		return px > lo && px < hi
	}},
	{"dw-aligned", func(px float64, long bool, o signal.Opens) bool {
		above := px > o.Daily && px > o.Weekly
		below := px < o.Daily && px < o.Weekly
		return (long && above) || (!long && below)
	}},
	{"mo-aligned", func(px float64, long bool, o signal.Opens) bool {
		return (long && px > o.Monthly) || (!long && px < o.Monthly)
	}},
}

// genFires mirrors cmd/sweepbt's shipped sweet spot, with the arm's filter
// applied at emit time.
func genFires(cs []market.Candle, atr []float64, short string, k keep) []autotrade.PaperFire {
	const tolFrac, bufATR, rMult = 0.0015, 0.15, 2.0
	var out []autotrade.PaperFire
	for i := 60; i < len(cs); i++ {
		pools := signal.FindLiquidity(cs[:i], 2, 20, tolFrac)
		bar := cs[i]
		a := atr[i]
		o := signal.ComputeOpens(cs[:i+1], bar.CloseTime)
		if !eligible(o) {
			continue
		}
		for _, p := range pools {
			if p.Kind == signal.EQH && bar.High > p.Hi && bar.Close < p.Lo {
				stop := bar.High + bufATR*a
				if risk := stop - bar.Close; risk > 0 && k(bar.Close, false, o) {
					out = append(out, autotrade.PaperFire{Time: bar.CloseTime, Symbol: short, TF: "1h",
						Strategy: "sweep-reject", Side: "short", Market: true, Entry: bar.Close,
						Stop: stop, TP: bar.Close - rMult*risk})
				}
				break
			}
			if p.Kind == signal.EQL && bar.Low < p.Lo && bar.Close > p.Hi {
				stop := bar.Low - bufATR*a
				if risk := bar.Close - stop; risk > 0 && k(bar.Close, true, o) {
					out = append(out, autotrade.PaperFire{Time: bar.CloseTime, Symbol: short, TF: "1h",
						Strategy: "sweep-reject", Side: "long", Market: true, Entry: bar.Close,
						Stop: stop, TP: bar.Close + rMult*risk})
				}
				break
			}
		}
	}
	return out
}

type tally struct {
	n, tp int
	netR  float64
}

func (t *tally) add(o autotrade.Outcome) {
	if o.Status != autotrade.OutTP && o.Status != autotrade.OutStop {
		return
	}
	t.n++
	if o.Status == autotrade.OutTP {
		t.tp++
	}
	t.netR += o.NetR
}

func (t tally) rpt() float64 {
	if t.n == 0 {
		return 0
	}
	return t.netR / float64(t.n)
}
func (t tally) wr() float64 {
	if t.n == 0 {
		return 0
	}
	return float64(t.tp) / float64(t.n) * 100
}

func main() {
	config.LoadDotEnv()
	// No flags: the three windows ARE the gate (shipgate.Default requires
	// MinWindows=3), so making them configurable would only let a caller
	// hand the gate fewer windows than it needs and get UNDECIDED.
	flag.Parse()

	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	syms := []struct {
		short string
		sym   market.Symbol
	}{
		{"BTC", market.BTCUSDT}, {"ETH", market.ETHUSDT}, {"SOL", market.SOLUSDT},
		{"SUI", market.SUIUSDT}, {"NEAR", market.NEARUSDT}, {"LINK", market.LINKUSDT},
	}
	days := []int{60, 90, 120}

	// [arm][window] aggregate, and [arm][symbol][window] per-symbol.
	agg := map[string]map[int]*tally{}
	per := map[string]map[string]map[int]*tally{}
	for _, a := range arms {
		agg[a.name] = map[int]*tally{}
		per[a.name] = map[string]map[int]*tally{}
		for _, d := range days {
			agg[a.name][d] = &tally{}
		}
		for _, s := range syms {
			per[a.name][s.short] = map[int]*tally{}
			for _, d := range days {
				per[a.name][s.short][d] = &tally{}
			}
		}
	}

	end := time.Now().UTC()
	for _, d := range days {
		start := end.AddDate(0, 0, -d)
		for _, s := range syms {
			cs, err := client.KlinesRange(context.Background(), s.sym, market.Timeframe("1h"), start, end)
			if err != nil || len(cs) < 200 {
				fmt.Fprintf(os.Stderr, "skip %s %dd: %v (bars=%d)\n", s.short, d, err, len(cs))
				continue
			}
			atr := alignRight(indicator.ATR(cs, 14), len(cs))
			for _, a := range arms {
				fires := genFires(cs, atr, s.short, a.f)
				positions := autotrade.DedupFires(fires, 6, 6, time.Hour, func(f autotrade.PaperFire) autotrade.Outcome {
					return autotrade.EvaluateFire(f, cs, 6)
				})
				for _, p := range positions {
					per[a.name][s.short][d].add(p.Outcome)
					agg[a.name][d].add(p.Outcome)
				}
			}
		}
	}

	fmt.Println("=== session-open FILTER A/B · sweep-reject 1h · filter applied at fire-generation ===")
	fmt.Println("(R/trd = netR/trades, trd/d = trades per day — the two numbers a bucket split cannot give)")
	for _, d := range days {
		fmt.Printf("\n──────── %dd ────────\n", d)
		fmt.Printf("%-11s %5s %6s %8s %8s %7s\n", "arm", "n", "win%", "netR", "R/trd", "trd/d")
		for _, a := range arms {
			t := agg[a.name][d]
			fmt.Printf("%-11s %5d %5.0f%% %+8.2f %+8.3f %7.2f\n",
				a.name, t.n, t.wr(), t.netR, t.rpt(), float64(t.n)/float64(d))
		}
	}

	fmt.Println("\n=== per-symbol R/trade by window ===")
	fmt.Printf("%-11s %-6s %9s %9s %9s\n", "arm", "sym", "60d", "90d", "120d")
	for _, a := range arms {
		for _, s := range syms {
			fmt.Printf("%-11s %-6s", a.name, s.short)
			for _, d := range days {
				t := per[a.name][s.short][d]
				fmt.Printf(" %+6.3f/%-2d", t.rpt(), t.n)
			}
			fmt.Println()
		}
	}

	fmt.Println("\n=== shipgate verdicts (arm vs baseline, aggregate windows) ===")
	mk := func(name string) shipgate.Arm {
		var ws []shipgate.Window
		for _, d := range days {
			t := agg[name][d]
			ws = append(ws, shipgate.Window{Days: d, NetR: t.netR, Trades: t.n, WinRate: t.wr()})
		}
		return shipgate.Arm{Name: name, Windows: ws}
	}
	base := mk("baseline")
	names := []string{}
	for _, a := range arms {
		if a.name != "baseline" {
			names = append(names, a.name)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		v := shipgate.Evaluate(mk(n), &base, shipgate.Default())
		fmt.Println(v.String())
	}

	// Per-symbol gate. The aggregate verdict can hide a filter that only
	// works where the baseline has no edge — and the structure veto set the
	// precedent of measuring per-symbol delta before baking an allowlist.
	// Same Criteria, so a PASS here is the same claim, just narrower.
	fmt.Println("\n=== shipgate verdicts PER SYMBOL (arm vs that symbol's baseline) ===")
	mkSym := func(name, sym string) shipgate.Arm {
		var ws []shipgate.Window
		for _, d := range days {
			t := per[name][sym][d]
			ws = append(ws, shipgate.Window{Days: d, NetR: t.netR, Trades: t.n, WinRate: t.wr()})
		}
		return shipgate.Arm{Name: name + "/" + sym, Windows: ws}
	}
	for _, n := range names {
		for _, s := range syms {
			b := mkSym("baseline", s.short)
			v := shipgate.Evaluate(mkSym(n, s.short), &b, shipgate.Default())
			if v.Result == shipgate.Pass {
				fmt.Println(v.String())
			}
		}
	}
	fmt.Println("(only PASS rows printed; everything else FAILed or was UNDECIDED)")
}
