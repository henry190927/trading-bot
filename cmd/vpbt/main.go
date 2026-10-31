// Command vpbt backtests the volume-profile "mountains" idea (docs/vpbt_design.md):
// price rests on a node and travels fast through the low-volume gap to the
// next one.
//
//	Arm A (edge hold, limit): price leaves the next node and closes in the gap
//	  → rest a limit in the near half of this node (-entry-frac), stop past
//	  its far foot, target the node price just left. This is how the user
//	  trades it by hand (BTC 10/21: node 81,250–81,900, limit 81,700).
//	Arm A2 (approach, limit): like A, but the limit is placed only once a close
//	  first lands within -approach-atr ATR of the node's near edge, or inside
//	  the node without breaking its far foot (an hourly bar often skips the
//	  band) — price has
//	  already shown it is heading for the node (user, 11/01: "react after
//	  price shows its direction"), instead of right after it leaves the
//	  other node.
//	Arm B (breakout): the previous close was inside a node, this bar closes
//	  beyond its edge into the gap → enter toward the next node.
//
// NO LOOK-AHEAD: the profile for signal bar i is built from cs[i-lookback:i],
// the bars BEFORE it, and every test reads only closed bars. Entries are at
// the signal bar's close (Market), scored by autotrade.DedupFires +
// autotrade.EvaluateFire like the other *bt harnesses.
//
// Fees: EvaluateFire's NetR is fee-free (and so are the other harnesses).
// -fee-bp charges a round trip per filled trade in R: fee×entry/risk. Set
// -fee-bp 0 to compare with snrbt/sweepbt/rangebt numbers like for like.
//
// Output is one row per symbol plus an aggregate line; feed the per-window
// aggregates to cmd/gate.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

type params struct {
	arm         string  // "A" | "B"
	target      string  // "foot" (near side of the next node) | "poc" (next node's POC)
	side        string  // long | short | auto
	lookback    int     // bars in the profile
	bins        int     // profile bins
	bufATR      float64 // stop buffer beyond the stop level, in ATR(14)
	minR        float64 // skip fires whose target/risk is below this
	cooldown    bool    // stay quiet on a side until the last fire's target or stop is reached
	approachATR float64 // arm A2: the band beyond the near edge that counts as "approaching"
	trendVeto   bool    // skip longs in a structural downtrend and shorts in an uptrend
	entryFrac   float64 // arm A: limit sits this far from the far foot (0.7 = upper 30% for a long)
	opts        indicator.NodeOpts
}

func main() {
	days := flag.Int("days", 90, "history window in days")
	endOff := flag.Int("end-offset", 0, "end the window N days before now; -days 30 -end-offset 30 is a DISJOINT bucket")
	tfStr := flag.String("tf", "1h", "timeframe for both the profile and the signal bars")
	symbols := flag.String("symbols", "BTC,ETH", "comma-separated short names (crypto only for now, user 11/01; e.g. add SOL,XRP,SUI as exploratory)")
	arm := flag.String("arm", "A", "A (limit after leaving the other node) | A2 (limit once approaching this node) | B (breakout) | both (A and B) | all")
	target := flag.String("target", "foot", "foot (near side of the next node) | poc (next node's POC)")
	side := flag.String("side", "auto", "long | short | auto")
	lookback := flag.Int("lookback", 200, "closed bars in each profile")
	bins := flag.Int("bins", 80, "profile bins (engine uses 80)")
	bufATR := flag.Float64("buf-atr", 0.25, "stop buffer in ATR(14)")
	cooldown := flag.Bool("cooldown", true, "design §3 cooldown: after a fire, stay quiet on that side until a close reaches its target or stop. The live daemon has NO such cooldown (only one position per rule), so use -cooldown=false to measure what the vp-node rule does")
	approachATR := flag.Float64("approach-atr", 1.0, "arm A2: a close within this many ATR beyond the node's near edge counts as approaching it")
	trend := flag.String("trend", "off", "off | veto: AnalyzeStructure on the last 250 closed bars, no long in a downtrend, no short in an uptrend (the range-edge / NFE rule)")
	entryFrac := flag.Float64("entry-frac", 0.7, "arm A limit position inside the node, from the far foot (BTC 10/21 example: 0.69)")
	minR := flag.Float64("min-r", 1.5, "skip fires whose reward:risk is below this")
	feeBp := flag.Float64("fee-bp", 6, "round-trip fee charged per filled trade, basis points of entry (0 = like the other harnesses)")
	trace := flag.Bool("trace", false, "print every position: node, bracket, outcome, and for a no-fill how close price came to the limit")
	showFunnel := flag.Bool("funnel", false, "print how many edge events were dropped for target side or reward:risk (arms A and B; A2 runs the shared autostrat code and is not counted)")
	expiry := flag.Int("expiry", 6, "EvaluateFire expiry bars (same 6 the other harnesses use)")
	flag.Parse()

	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf := market.Timeframe(*tfStr)
	barDur := tfDuration(tf)
	end := time.Now().UTC().AddDate(0, 0, -*endOff)
	start := end.AddDate(0, 0, -*days)

	arms := []string{strings.ToUpper(*arm)}
	switch arms[0] {
	case "BOTH":
		arms = []string{"A", "B"}
	case "ALL":
		arms = []string{"A", "A2", "B"}
	}

	type series struct {
		short string
		cs    []market.Candle
		atr   []float64
	}
	var all []series
	for _, tok := range strings.Split(*symbols, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		sym, err := market.ResolveErr(tok)
		if err != nil {
			log.Print(err)
			continue
		}
		// Fetch lookback extra bars before the window so the first signal
		// bar already has a full profile behind it.
		// BingX answers bursts with code 100410 ("try again later"); back off
		// and retry rather than silently dropping the symbol from the sample.
		var cs []market.Candle
		for try := 1; try <= 5; try++ {
			cs, err = client.KlinesRange(context.Background(), sym, tf, start.Add(-time.Duration(*lookback)*barDur), end)
			if err == nil || !strings.Contains(err.Error(), "100410") {
				break
			}
			time.Sleep(time.Duration(try*3) * time.Second)
		}
		if err != nil || len(cs) < *lookback+30 {
			log.Printf("%s: klines %v (len %d)", tok, err, len(cs))
			continue
		}
		all = append(all, series{tok, cs, alignRight(indicator.ATR(cs, 14), len(cs))})
	}

	for _, a := range arms {
		p := params{
			arm: a, target: *target, side: *side, lookback: *lookback, bins: *bins,
			bufATR: *bufATR, minR: *minR, entryFrac: *entryFrac, trendVeto: *trend == "veto", approachATR: *approachATR, cooldown: *cooldown, opts: indicator.DefaultNodeOpts(),
		}
		fmt.Printf("\n=== vpbt arm %s · %dd ending -%dd · %s · lookback %d · target %s · side %s · entry-frac %.2f (A) · buf %.2fATR · minR %.1f · fee %.0fbp · expiry %d · trend %s ===\n",
			a, *days, *endOff, tf, *lookback, *target, *side, *entryFrac, *bufATR, *minR, *feeBp, *expiry, *trend)
		fmt.Printf("%-5s %6s %6s %5s %5s %7s %9s %8s %8s\n", "sym", "pos", "fill%", "tp", "stop", "win%", "netR", "R/trade", "trd/day")

		var aggR float64
		var aggN int
		for _, s := range all {
			fires := genFires(s.cs, s.atr, s.short, start, p)
			positions := autotrade.DedupFires(fires, *expiry, *expiry, barDur, func(f autotrade.PaperFire) autotrade.Outcome {
				return autotrade.EvaluateFire(f, s.cs, *expiry)
			})
			var netR float64
			var tp, stop, filled int
			for _, pos := range positions {
				f, oc := pos.Fire, pos.Outcome
				fee := feeR(f, *feeBp)
				switch oc.Status {
				case autotrade.OutTP:
					tp, filled = tp+1, filled+1
					netR += oc.NetR - fee
				case autotrade.OutStop:
					stop, filled = stop+1, filled+1
					netR += oc.NetR - fee
				case autotrade.OutOpen:
					filled++
				}
			}
			win, fp, rpt, tpd := 0.0, 0.0, 0.0, 0.0
			if tp+stop > 0 {
				win = float64(tp) / float64(tp+stop) * 100
			}
			if len(positions) > 0 {
				fp = float64(filled) / float64(len(positions)) * 100
				rpt = netR / float64(len(positions))
				tpd = float64(len(positions)) / float64(*days)
			}
			fmt.Printf("%-5s %6d %5.0f%% %5d %5d %6.0f%% %+9.2f %+8.3f %8.2f\n",
				s.short, len(positions), fp, tp, stop, win, netR, rpt, tpd)
			if *trace {
				for _, pos := range positions {
					printTrace(pos, s.cs, *expiry)
				}
			}
			aggR += netR
			aggN += len(positions)
		}
		aggRPT, aggTPD := 0.0, 0.0
		if aggN > 0 {
			aggRPT = aggR / float64(aggN)
			aggTPD = float64(aggN) / float64(*days)
		}
		if *showFunnel {
			avg := 0.0
			if funnel.lowR > 0 {
				avg = funnel.rSum / float64(funnel.lowR)
			}
			fmt.Printf("    funnel: %d edge events · %d target on the wrong side · %d below minR (their avg %.2fR) · %d fired\n",
				funnel.events, funnel.badSide, funnel.lowR, avg, funnel.events-funnel.badSide-funnel.lowR)
			funnel.events, funnel.badSide, funnel.lowR, funnel.rSum = 0, 0, 0, 0
		}
		fmt.Printf("--- aggregate: %d positions, netR %+.2f, R/trade %+.3f, trd/day %.2f ---\n", aggN, aggR, aggRPT, aggTPD)
	}
}

// genFires walks closed signal bars inside the window and emits a fire where
// the arm's event happens at a node edge with a next node to aim at.
//
// Cooldown (design §3): after a fire, the same side of the same arm stays
// quiet until a close reaches that fire's target or its stop, i.e. until
// price has left the node's neighbourhood. Nodes are rebuilt every bar, so
// "the same node" is tracked by this price outcome rather than by identity.
func genFires(cs []market.Candle, atr []float64, short string, start time.Time, p params) []autotrade.PaperFire {
	wantLong := p.side == "long" || p.side == "auto"
	wantShort := p.side == "short" || p.side == "auto"
	strategy := "vp-" + strings.ToLower(p.arm)

	var out []autotrade.PaperFire
	var coolLong, coolShort *autotrade.PaperFire // copies, not pointers into out
	for i := p.lookback; i < len(cs); i++ {
		bar, prev, a := cs[i], cs[i-1], atr[i]
		if coolLong != nil && (bar.Close >= coolLong.TP || bar.Close <= coolLong.Stop) {
			coolLong = nil
		}
		if coolShort != nil && (bar.Close <= coolShort.TP || bar.Close >= coolShort.Stop) {
			coolShort = nil
		}
		if bar.OpenTime.Before(start) || a <= 0 {
			continue
		}

		// Structure over the 250 closed bars up to and including this one,
		// computed only if an edge event asks for it.
		trendDone, trend := false, signal.StructNeutral
		trendNow := func() signal.TrendStructure {
			if !trendDone {
				trend, trendDone = signal.AnalyzeStructure(cs[max(0, i+1-250):i+1], 2).Trend, true
			}
			return trend
		}

		if p.arm == "A2" {
			// The live "vp-node" rule's own code, so this arm measures what
			// the daemon runs.
			wl := wantLong && coolLong == nil && !(p.trendVeto && trendNow() == signal.StructDowntrend)
			ws := wantShort && coolShort == nil && !(p.trendVeto && trendNow() == signal.StructUptrend)
			t := autostrat.VPNodeTrigger(cs, a, i, wl, ws, p.vpNode())
			if !t.Fire {
				continue
			}
			f := autotrade.PaperFire{
				Time: bar.CloseTime, Symbol: short, Strategy: strategy, Side: t.Side,
				Market: t.Market, Entry: t.Entry, Stop: t.Stop, TP: t.TP, Why: "arm A2 · " + t.Why,
			}
			out = append(out, f)
			if p.cooldown && t.Side == "long" {
				coolLong = &f
			} else if p.cooldown {
				coolShort = &f
			}
			continue
		}

		vp := indicator.BuildVolumeProfile(cs[i-p.lookback:i], p.bins, 1)
		nodes := indicator.FindNodes(vp, p.opts)
		if len(nodes) < 2 {
			continue
		}

		for k, n := range nodes {
			var above, below *indicator.Node
			if k+1 < len(nodes) {
				above = &nodes[k+1]
			}
			if k > 0 {
				below = &nodes[k-1]
			}

			if wantLong && coolLong == nil && above != nil {
				if entry, stop, ok := longSetup(p, n, *above, prev, bar, a); ok && !(p.trendVeto && trendNow() == signal.StructDowntrend) {
					tgt := above.Low
					if p.target == "poc" {
						tgt = above.POC
					}
					if f, ok := mkFire(p, bar, short, strategy, "long", entry, stop, tgt); ok {
						f.Why += fmt.Sprintf(" · node %s–%s → next %s–%s", px(n.Low), px(n.High), px(above.Low), px(above.High))
						out = append(out, f)
						if p.cooldown {
							coolLong = &f
						}
						break
					}
				}
			}
			if wantShort && coolShort == nil && below != nil {
				if entry, stop, ok := shortSetup(p, n, *below, prev, bar, a); ok && !(p.trendVeto && trendNow() == signal.StructUptrend) {
					tgt := below.High
					if p.target == "poc" {
						tgt = below.POC
					}
					if f, ok := mkFire(p, bar, short, strategy, "short", entry, stop, tgt); ok {
						f.Why += fmt.Sprintf(" · node %s–%s → next %s–%s", px(n.Low), px(n.High), px(below.Low), px(below.High))
						out = append(out, f)
						if p.cooldown {
							coolShort = &f
						}
						break
					}
				}
			}
		}
	}
	return out
}

// longSetup reports whether bar triggers a long at node n (above is the
// next node up).
//
//	A: prev close inside `above`, this close in the gap (below above.Low,
//	   above n.High) → limit at n.Low + entryFrac×width, stop below n.Low.
//	B: prev close inside n, this close above n.High (into the gap), market.
//	   Stop at the node's midpoint — design §3 marks this "to discuss".
func longSetup(p params, n, above indicator.Node, prev, bar market.Candle, atr float64) (entry, stop float64, ok bool) {
	switch p.arm {
	case "A":
		if prev.Close >= above.Low && bar.Close < above.Low && bar.Close > n.High {
			return n.Low + p.entryFrac*(n.High-n.Low), n.Low - p.bufATR*atr, true
		}
	case "B":
		if prev.Close >= n.Low && prev.Close <= n.High && bar.Close > n.High {
			return bar.Close, (n.Low+n.High)/2 - p.bufATR*atr, true
		}
	}
	return 0, 0, false
}

// shortSetup mirrors longSetup at the node's lower edge (below is the next
// node down).
func shortSetup(p params, n, below indicator.Node, prev, bar market.Candle, atr float64) (entry, stop float64, ok bool) {
	switch p.arm {
	case "A":
		if prev.Close <= below.High && bar.Close > below.High && bar.Close < n.Low {
			return n.High - p.entryFrac*(n.High-n.Low), n.High + p.bufATR*atr, true
		}
	case "B":
		if prev.Close >= n.Low && prev.Close <= n.High && bar.Close < n.Low {
			return bar.Close, (n.Low+n.High)/2 + p.bufATR*atr, true
		}
	}
	return 0, 0, false
}

// vpNode maps the harness flags onto the live strategy's parameters.
func (p params) vpNode() autostrat.VPNodeParams {
	v := autostrat.DefaultVPNodeParams()
	v.Lookback, v.Bins, v.Opts = p.lookback, p.bins, p.opts
	v.EntryFrac, v.ApproachATR, v.BufATR, v.MinR = p.entryFrac, p.approachATR, p.bufATR, p.minR
	v.TargetPOC = p.target == "poc"
	return v
}

// mkFire builds the fire, rejecting it when the target is on the wrong side
// of the entry or the reward:risk is below minR.
func mkFire(p params, bar market.Candle, short, strategy, side string, entry, stop, tgt float64) (autotrade.PaperFire, bool) {
	var risk, reward float64
	if side == "long" {
		risk, reward = entry-stop, tgt-entry
	} else {
		risk, reward = stop-entry, entry-tgt
	}
	funnel.events++
	switch {
	case risk <= 0 || reward <= 0:
		funnel.badSide++
		return autotrade.PaperFire{}, false
	case reward/risk < p.minR:
		funnel.lowR++
		funnel.rSum += reward / risk
		return autotrade.PaperFire{}, false
	}
	return autotrade.PaperFire{
		Time: bar.CloseTime, Symbol: short, Strategy: strategy, Side: side,
		Market: p.arm == "B", Entry: entry, Stop: stop, TP: tgt,
		Why: fmt.Sprintf("arm %s · %.2fR", p.arm, reward/risk),
	}, true
}

// printTrace prints one position on two lines. For a no-fill it adds the
// closest approach: how far the best price in the expiry window stayed from
// the limit, in % of the limit and in units of the bracket's risk.
func printTrace(pos autotrade.Position, cs []market.Candle, expiry int) {
	f, oc := pos.Fire, pos.Outcome
	utc8 := time.FixedZone("UTC+8", 8*3600)
	risk := f.Entry - f.Stop
	if f.Side == "short" {
		risk = f.Stop - f.Entry
	}
	fmt.Printf("    %s %-5s entry %s stop %s tp %s · %s\n",
		f.Time.In(utc8).Format("01-02 15:04"), f.Side, px(f.Entry), px(f.Stop), px(f.TP), f.Why)
	switch oc.Status {
	case autotrade.OutNoFill, autotrade.OutPending:
		best, seen := 0.0, 0
		for _, c := range cs {
			if !c.CloseTime.After(f.Time) {
				continue
			}
			if seen == expiry {
				break
			}
			if f.Side == "long" && (seen == 0 || c.Low < best) {
				best = c.Low
			}
			if f.Side == "short" && (seen == 0 || c.High > best) {
				best = c.High
			}
			seen++
		}
		gap := best - f.Entry
		if f.Side == "short" {
			gap = f.Entry - best
		}
		fmt.Printf("          %-7s closest %s · missed by %.2f%% (%.2f R)\n", oc.Status, px(best), 100*gap/f.Entry, gap/risk)
	default:
		fmt.Printf("          %-7s %+.2fR · filled after %d bars · exit %s after %d bars\n",
			oc.Status, oc.NetR, oc.BarsToFill, px(oc.ExitPrice), oc.BarsHeld)
	}
}

func px(p float64) string {
	switch {
	case p >= 1000:
		return fmt.Sprintf("%.0f", p)
	case p >= 10:
		return fmt.Sprintf("%.2f", p)
	default:
		return fmt.Sprintf("%.4f", p)
	}
}

// funnel counts why edge events did or did not become fires (-funnel).
var funnel struct {
	events, badSide, lowR int
	rSum                  float64
}

// feeR is a round-trip fee in R for one filled trade.
func feeR(f autotrade.PaperFire, bp float64) float64 {
	risk := f.Entry - f.Stop
	if f.Side == "short" {
		risk = f.Stop - f.Entry
	}
	if risk <= 0 || bp <= 0 {
		return 0
	}
	return bp / 1e4 * f.Entry / risk
}

func tfDuration(tf market.Timeframe) time.Duration {
	if tf == "1d" {
		return 24 * time.Hour
	}
	if d, err := time.ParseDuration(string(tf)); err == nil {
		return d
	}
	return time.Hour
}

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
			continue
		}
		out[i] = arr[i-off]
	}
	return out
}
