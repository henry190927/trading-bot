// Command pivotbt backtests the "pivot-pullback" trend-following trigger
// (docs/pivot_pullback_design.md): 4h HH-HL / LH-LL regime + 4h impulse
// 樞紐區, 1h micro-BOS entry after a dip into the zone, stop on the 4h leg
// origin, TP on the 4h 1:1 measured move.
//
// The trigger is autostrat.PivotPullbackTrigger — the same pure function a
// daemon wrapper would call — so this measures the rule, not a copy of it.
//
// NO LOOK-AHEAD: fires on 1h bar i read cs[:i+1] and only the 4h bars whose
// CloseTime <= cs[i].OpenTime (enforced inside the trigger). Fires are
// generated only on bars whose OpenTime is inside a window; their outcome may
// be resolved with bars after the window end, so a trade belongs to the
// window/bucket it was OPENED in and a 30d bucket does not truncate its last
// trades to "open".
//
// Scoring: autotrade.EvaluateFire → autotrade.WithFee → autotrade.DedupFires
// (expiry 6, cooldown 6 bars) = one position per (symbol, rule) at a time.
// -no-dedup scores every fire instead (the signal, not the book); the no-dedup
// R/trade column is printed either way.
//
// Windows are the cross product of -days and -end-offset. With -gate, the
// windows with end-offset 0 and days > 30 are the nested gate windows and the
// -days 30 windows are the disjoint buckets; both go to shipgate.Evaluate.
//
//	go run ./cmd/pivotbt -symbols BTC,ETH -days 60,90,120,30 -end-offset 0,30,60,90,120,150 -fee-bps 6,10 -arm all -gate
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
)

type window struct{ days, off int }

// baseTF labels the fires (PaperFire.TF); set from -tf.
var baseTF = "1h"

type series struct {
	short   string
	cs, htf []market.Candle
}

// stats is one (symbol or pool, window) result.
type stats struct {
	positions, tp, stop, open int
	netR, longR, shortR       float64
	payoffs                   []float64
	ndN                       int
	ndR                       float64
}

func (s *stats) add(o stats) {
	s.positions += o.positions
	s.tp += o.tp
	s.stop += o.stop
	s.open += o.open
	s.netR += o.netR
	s.longR += o.longR
	s.shortR += o.shortR
	s.payoffs = append(s.payoffs, o.payoffs...)
	s.ndN += o.ndN
	s.ndR += o.ndR
}

func (s stats) n() int { return s.tp + s.stop }

func (s stats) rpt() float64 {
	if s.n() == 0 {
		return 0
	}
	return s.netR / float64(s.n())
}

func (s stats) sd() float64 {
	n := len(s.payoffs)
	if n < 2 {
		return 0
	}
	m := 0.0
	for _, v := range s.payoffs {
		m += v
	}
	m /= float64(n)
	ss := 0.0
	for _, v := range s.payoffs {
		ss += (v - m) * (v - m)
	}
	return math.Sqrt(ss / float64(n-1))
}

func (s stats) wr() float64 {
	if s.n() == 0 {
		return 0
	}
	return 100 * float64(s.tp) / float64(s.n())
}

func main() {
	symbols := flag.String("symbols", "BTC,ETH,SOL,XRP,SUI,HYPE,NEAR,LINK,XAG", "comma-separated short names")
	tfStr := flag.String("tf", "1h", "base (entry) timeframe")
	htfStr := flag.String("htf", "4h", "regime / zone timeframe")
	daysS := flag.String("days", "60,90,120", "window lengths in days (comma list)")
	offS := flag.String("end-offset", "0", "window ends N days before now (comma list); -days 30 -end-offset 0,30,60 = disjoint buckets")
	feeS := flag.String("fee-bps", "6", "round-trip fee in bps of notional via autotrade.WithFee (comma list)")
	noDedup := flag.Bool("no-dedup", false, "score every fire instead of one position per rule at a time")
	warmup := flag.Bool("warmup", true, "fetch the trigger's lookback before each window (live parity); false = cold start at the window edge")
	armS := flag.String("arm", "P0", "P0 (micro-BOS, leg-origin stop) | V1 (reclaim trigger) | V2 (pullback-low stop) | all")
	side := flag.String("side", "auto", "long | short | auto")
	gate := flag.Bool("gate", false, "run shipgate on the nested (end-offset 0, days>30) windows with the 30d windows as buckets")
	verbose := flag.Bool("v", false, "print every (symbol, window) row")
	trace := flag.Bool("trace", false, "print every position of the longest window")
	flag.Parse()

	days, offs, fees := ints(*daysS), ints(*offS), floats(*feeS)
	var wins []window
	for _, d := range days {
		for _, o := range offs {
			if d > 30 && o != 0 {
				continue // nested windows end now; only buckets shift
			}
			wins = append(wins, window{d, o})
		}
	}
	sort.Slice(wins, func(a, b int) bool {
		if wins[a].days != wins[b].days {
			return wins[a].days > wins[b].days
		}
		return wins[a].off < wins[b].off
	})
	arms := []string{strings.ToUpper(*armS)}
	if arms[0] == "ALL" {
		arms = []string{"P0", "V1", "V2"}
	}

	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	tf, htf := market.Timeframe(*tfStr), market.Timeframe(*htfStr)
	baseTF = *tfStr
	barDur, htfDur := tfDuration(tf), tfDuration(htf)
	now := time.Now().UTC().Truncate(time.Hour)
	maxSpan := 0
	for _, w := range wins {
		maxSpan = max(maxSpan, w.days+w.off)
	}
	p0 := autostrat.DefaultPivotPullbackParams()
	lead := time.Duration(p0.HTFLookback+10) * htfDur
	fetchStart := now.AddDate(0, 0, -maxSpan).Add(-lead)

	var all []series
	for _, tok := range strings.Split(*symbols, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		sym, err := market.ResolveErr(tok)
		if err != nil {
			log.Print(err)
			continue
		}
		cs, err := fetch(client, sym, tf, fetchStart, now)
		if err != nil {
			log.Printf("%s %s: %v", tok, tf, err)
			continue
		}
		hc, err := fetch(client, sym, htf, fetchStart, now)
		if err != nil {
			log.Printf("%s %s: %v", tok, htf, err)
			continue
		}
		log.Printf("%s: %d %s bars from %s, %d %s bars", tok, len(cs), tf, cs[0].OpenTime.Format("2006-01-02"), len(hc), htf)
		all = append(all, series{tok, cs, hc})
	}

	for _, arm := range arms {
		p := p0
		switch arm {
		case "V1":
			p.Trigger = "reclaim"
		case "V2":
			p.Stop = "pullback"
		}
		for _, fee := range fees {
			fmt.Printf("\n=== pivotbt %s · %s/%s · fee %.0fbps · dedup %v · warmup %v · side %s · now %s ===\n",
				arm, tf, htf, fee, !*noDedup, *warmup, *side, now.Format("2006-01-02 15:04"))
			pool := map[window]*stats{}
			for _, w := range wins {
				pool[w] = &stats{}
			}
			type symRes struct {
				short string
				by    map[window]stats
			}
			var res []symRes
			for _, s := range all {
				by := map[window]stats{}
				var full []autotrade.PaperFire
				if *warmup {
					full = genFires(s.cs, s.htf, 0, s.short, arm, *side, p)
				}
				for _, w := range wins {
					end := now.AddDate(0, 0, -w.off)
					start := end.AddDate(0, 0, -w.days)
					fires := full
					if !*warmup {
						// Cold start: the trigger sees nothing before the window.
						k := sort.Search(len(s.cs), func(j int) bool { return !s.cs[j].OpenTime.Before(start) })
						h := sort.Search(len(s.htf), func(j int) bool { return !s.htf[j].OpenTime.Before(start) })
						fires = genFires(s.cs[k:], s.htf[h:], 0, s.short, arm, *side, p)
					}
					var in []autotrade.PaperFire
					for _, f := range fires {
						ot := f.Time.Add(-barDur + time.Millisecond)
						if !ot.Before(start) && ot.Before(end) {
							in = append(in, f)
						}
					}
					st := score(in, s.cs, fee, barDur, *noDedup)
					by[w] = st
					pool[w].add(st)
					if *trace && w == wins[0] {
						traceFires(in, s.cs, fee, barDur, *noDedup)
					}
				}
				res = append(res, symRes{s.short, by})
			}

			if *verbose {
				fmt.Printf("%-5s %8s %5s %4s %4s %4s %6s %8s %8s %6s %6s %7s %7s %7s\n",
					"sym", "window", "pos", "tp", "stop", "open", "WR%", "netR", "R/t", "SD", "SE", "trd/d", "ndR/t", "L/S")
				for _, r := range res {
					for _, w := range wins {
						printRow(r.short, w, r.by[w])
					}
				}
				for _, w := range wins {
					printRow("POOL", w, *pool[w])
				}
			}

			// Summary: one line per symbol.
			var nested, buckets []window
			for _, w := range wins {
				if w.days == 30 {
					buckets = append(buckets, w)
				} else if w.off == 0 {
					nested = append(nested, w)
				}
			}
			sort.Slice(nested, func(a, b int) bool { return nested[a].days < nested[b].days })
			fmt.Printf("%-5s | %-26s | %-12s | %-44s | %s\n", "sym", "nested netR (n)", "bucket sum", "buckets off0..", "verdict")
			line := func(name string, get func(window) stats) {
				var ns, bs []string
				var arm shipgate.Arm
				arm.Name = name
				bsum := 0.0
				for _, w := range nested {
					st := get(w)
					ns = append(ns, fmt.Sprintf("%+.1f(%d)", st.netR, st.n()))
					arm.Windows = append(arm.Windows, shipgate.Window{Days: w.days, NetR: st.netR, Trades: st.n(), WinRate: st.wr()})
				}
				for _, w := range buckets {
					st := get(w)
					bs = append(bs, fmt.Sprintf("%+.1f", st.netR))
					bsum += st.netR
					arm.Buckets = append(arm.Buckets, shipgate.Window{Days: w.off, NetR: st.netR, Trades: st.n(), WinRate: st.wr()})
				}
				verdict := ""
				if *gate && len(nested) > 0 {
					v := shipgate.Evaluate(arm, nil, shipgate.Default())
					verdict = string(v.Result)
					if len(v.Reasons) > 0 {
						verdict += " — " + strings.Join(v.Reasons, "; ")
					}
				}
				fmt.Printf("%-5s | %-26s | %+12.2f | %-44s | %s\n", name, strings.Join(ns, " "), bsum, strings.Join(bs, " "), verdict)
			}
			for _, r := range res {
				by := r.by
				line(r.short, func(w window) stats { return by[w] })
			}
			line("POOL", func(w window) stats { return *pool[w] })

			// Detail on the longest nested window and on each bucket, pooled.
			if *verbose {
				continue // the POOL rows were printed above
			}
			fmt.Printf("pooled detail:\n")
			for _, w := range wins {
				printRow("POOL", w, *pool[w])
			}
		}
	}
}

func printRow(name string, w window, st stats) {
	se := 0.0
	if n := len(st.payoffs); n > 1 {
		se = st.sd() / math.Sqrt(float64(n))
	}
	nd := 0.0
	if st.ndN > 0 {
		nd = st.ndR / float64(st.ndN)
	}
	fmt.Printf("  %-5s %4dd-%-3d pos %4d tp %3d stop %3d open %2d WR %5.1f%% netR %+8.2f R/t %+6.3f SD %5.2f SE %5.3f trd/d %5.2f ndR/t %+6.3f (n %d) L %+.1f S %+.1f\n",
		name, w.days, w.off, st.positions, st.tp, st.stop, st.open, st.wr(), st.netR, st.rpt(), st.sd(), se,
		float64(st.positions)/float64(w.days), nd, st.ndN, st.longR, st.shortR)
}

// genFires walks every closed base bar from `from` and returns the trigger's
// fires, oldest first.
func genFires(cs, htf []market.Candle, from int, short, arm, side string, p autostrat.PivotPullbackParams) []autotrade.PaperFire {
	wantLong, wantShort := side != "short", side != "long"
	var out []autotrade.PaperFire
	for i := max(from, 1); i < len(cs); i++ {
		t := autostrat.PivotPullbackTrigger(cs, htf, i, wantLong, wantShort, p)
		if !t.Fire {
			continue
		}
		out = append(out, autotrade.PaperFire{
			Time: cs[i].CloseTime, Symbol: short, TF: baseTF, Strategy: "pivot-pullback-" + strings.ToLower(arm),
			Side: t.Side, Market: t.Market, Entry: t.Entry, Stop: t.Stop, TP: t.TP, Why: t.Why,
		})
	}
	return out
}

func score(fires []autotrade.PaperFire, cs []market.Candle, fee float64, barDur time.Duration, noDedup bool) stats {
	resolve := func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.WithFee(f, autotrade.EvaluateFire(f, cs, 6), fee)
	}
	var st stats
	// No-dedup R/trade: every fire on its own.
	for _, f := range fires {
		oc := resolve(f)
		if oc.Status == autotrade.OutTP || oc.Status == autotrade.OutStop {
			st.ndN++
			st.ndR += oc.NetR
		}
	}
	var positions []autotrade.Position
	if noDedup {
		for _, f := range fires {
			positions = append(positions, autotrade.Position{Fire: f, Outcome: resolve(f)})
		}
	} else {
		positions = autotrade.DedupFires(fires, 6, 6, barDur, resolve)
	}
	st.positions = len(positions)
	for _, pos := range positions {
		oc := pos.Outcome
		switch oc.Status {
		case autotrade.OutTP:
			st.tp++
		case autotrade.OutStop:
			st.stop++
		case autotrade.OutOpen:
			st.open++
			continue
		default:
			continue
		}
		st.netR += oc.NetR
		st.payoffs = append(st.payoffs, oc.NetR)
		if pos.Fire.Side == "long" {
			st.longR += oc.NetR
		} else {
			st.shortR += oc.NetR
		}
	}
	return st
}

func traceFires(fires []autotrade.PaperFire, cs []market.Candle, fee float64, barDur time.Duration, noDedup bool) {
	utc8 := time.FixedZone("UTC+8", 8*3600)
	resolve := func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.WithFee(f, autotrade.EvaluateFire(f, cs, 6), fee)
	}
	positions := autotrade.DedupFires(fires, 6, 6, barDur, resolve)
	if noDedup {
		positions = positions[:0]
		for _, f := range fires {
			positions = append(positions, autotrade.Position{Fire: f, Outcome: resolve(f)})
		}
	}
	for _, pos := range positions {
		f, oc := pos.Fire, pos.Outcome
		fmt.Printf("    %s %s %-5s entry %.4f stop %.4f tp %.4f → %-5s %+.2fR held %d · %s\n",
			f.Time.In(utc8).Format("01-02 15:04"), f.Symbol, f.Side, f.Entry, f.Stop, f.TP, oc.Status, oc.NetR, oc.BarsHeld, f.Why)
	}
}

// fetch is KlinesRange with a back-off on BingX's 100410 rate limit.
func fetch(client *bingx.Client, sym market.Symbol, tf market.Timeframe, start, end time.Time) ([]market.Candle, error) {
	var cs []market.Candle
	var err error
	for try := 1; try <= 6; try++ {
		cs, err = client.KlinesRange(context.Background(), sym, tf, start, end)
		if err == nil || !strings.Contains(err.Error(), "100410") {
			break
		}
		time.Sleep(time.Duration(try*5) * time.Second)
	}
	if err == nil && len(cs) < 50 {
		err = fmt.Errorf("only %d bars", len(cs))
	}
	return cs, err
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

func ints(s string) []int {
	var out []int
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		v, err := strconv.Atoi(t)
		if err != nil {
			log.Fatalf("bad int %q", t)
		}
		out = append(out, v)
	}
	return out
}

func floats(s string) []float64 {
	var out []float64
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		v, err := strconv.ParseFloat(t, 64)
		if err != nil {
			log.Fatalf("bad float %q", t)
		}
		out = append(out, v)
	}
	return out
}
