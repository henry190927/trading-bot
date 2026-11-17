// Command mp80bt backtests the Market Profile "80% rule"
// (docs/market_profile_80_design.md): the UTC day opens outside the prior
// day's 70% value area, re-enters it and is accepted (30m×2 or 1h×1 closes
// inside), then a market entry targets the opposite VA edge with the stop
// beyond the re-entry edge at the extreme since re-entry + 0.25×ATR14(30m).
//
// Everything is derived from ONE 1m series per symbol: the VA is built from
// the prior UTC day's closed 1m bars, HTF bars are UTC-aligned aggregates of
// closed 1m bars. A fire at time T depends only on bars that closed at or
// before T (TestNoLookahead poisons every later bar and requires the same
// fires).
//
// Scoring as cmd/lvlbt: autotrade.EvaluateFire on the 1m series (no timeout)
// → autotrade.WithFee → autotrade.DedupFires (one position per symbol).
//
//	go run ./cmd/mp80bt -symbols BTC,ETH -days 60,90,120,30 -end-offset 0,30,60,90,120,150 -fee-bps 10,6,0 -gate -cache <dir>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/config"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
)

const strategyName = "mp80"

// Params is §3 of the design doc. Only HTF/Need differ between the two
// pre-registered variants; everything else is fixed.
type Params struct {
	HTF        time.Duration // acceptance bar size (30m or 1h)
	Need       int           // consecutive inside closes required
	Bins       int           // volume-profile bins over the prior day's range
	MinDayBars int           // prior day must have at least this many 1m bars
	StopBufATR float64       // stop buffer in ATR14(30m)
	MinStopPct float64       // skip below this stop distance (%)
	MinRR      float64       // skip below this reward/risk
}

func DefaultParams() Params {
	return Params{HTF: 30 * time.Minute, Need: 2, Bins: 100, MinDayBars: 1380,
		StopBufATR: 0.25, MinStopPct: 0.5, MinRR: 1.0}
}

// VA is the prior day's value area.
type VA struct{ VAH, VAL, POC float64 }

// ValueArea builds the 70% value area of one day's 1m bars, volume spread
// uniformly over each bar's [low, high] (indicator.BuildVolumeProfile with
// BodyWeight 0).
func ValueArea(day []market.Candle, bins int) (VA, bool) {
	saved := indicator.BodyWeight
	indicator.BodyWeight = 0
	vp := indicator.BuildVolumeProfile(day, bins, 1)
	indicator.BodyWeight = saved
	if vp.VAH <= vp.VAL || vp.POC == 0 {
		return VA{}, false
	}
	return VA{VAH: vp.VAH, VAL: vp.VAL, POC: vp.POC}, true
}

// idxAtOrAfter returns the first bar with OpenTime >= t (len(cs) if none).
func idxAtOrAfter(cs []market.Candle, t time.Time) int {
	return sort.Search(len(cs), func(i int) bool { return !cs[i].OpenTime.Before(t) })
}

// openAt is the open of the bar starting at t (5 min tolerance).
func openAt(cs []market.Candle, t time.Time) (float64, bool) {
	i := idxAtOrAfter(cs, t)
	if i >= len(cs) || cs[i].OpenTime.Sub(t) > 5*time.Minute || i == 0 {
		return 0, false
	}
	return cs[i].Open, true
}

// htfBar is a UTC-aligned aggregate of 1m bars [I0, I1].
type htfBar struct {
	Open, High, Low, Close float64
	OpenTime, CloseTime    time.Time
	I0, I1                 int
}

// Aggregate builds complete HTF bars only (as cmd/lvlbt): a bucket whose last
// 1m bar is not the bucket's final minute is dropped.
func Aggregate(cs []market.Candle, d time.Duration) []htfBar {
	var out []htfBar
	for i := 0; i < len(cs); {
		start := cs[i].OpenTime.UTC().Truncate(d)
		end := start.Add(d)
		b := htfBar{Open: cs[i].Open, High: cs[i].High, Low: cs[i].Low, OpenTime: start, CloseTime: end, I0: i}
		j := i
		for j+1 < len(cs) && cs[j+1].OpenTime.Before(end) {
			j++
			b.High = math.Max(b.High, cs[j].High)
			b.Low = math.Min(b.Low, cs[j].Low)
		}
		b.Close, b.I1 = cs[j].Close, j
		if !cs[j].OpenTime.Add(time.Minute).Before(end) {
			out = append(out, b)
		}
		i = j + 1
	}
	return out
}

func asCandles(bs []htfBar) []market.Candle {
	out := make([]market.Candle, len(bs))
	for i, b := range bs {
		out[i] = market.Candle{OpenTime: b.OpenTime, CloseTime: b.CloseTime, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close}
	}
	return out
}

// GenFires runs §3 over one symbol's 1m series and returns fires oldest first.
func GenFires(cs []market.Candle, short string, p Params) []autotrade.PaperFire {
	if len(cs) == 0 {
		return nil
	}
	bars := Aggregate(cs, p.HTF)
	b30 := Aggregate(cs, 30*time.Minute)
	atr30 := indicator.ATR(asCandles(b30), 14)
	var fires []autotrade.PaperFire

	first := cs[0].OpenTime.UTC()
	last := cs[len(cs)-1].CloseTime.UTC()
	d0 := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	for d := d0; d.Before(last); d = d.AddDate(0, 0, 1) {
		next := d.AddDate(0, 0, 1)
		i0, i1 := idxAtOrAfter(cs, d.AddDate(0, 0, -1)), idxAtOrAfter(cs, d)
		if i1-i0 < p.MinDayBars {
			continue
		}
		va, ok := ValueArea(cs[i0:i1], p.Bins)
		if !ok {
			continue
		}
		o, ok := openAt(cs, d)
		if !ok {
			continue
		}
		var side string
		switch {
		case o > va.VAH:
			side = "short"
		case o < va.VAL:
			side = "long"
		default:
			continue
		}
		long := side == "long"
		k0 := sort.Search(len(bars), func(k int) bool { return !bars[k].OpenTime.Before(d) })
		run, start := 0, -1
		for k := k0; k < len(bars) && !bars[k].CloseTime.After(next); k++ {
			c := bars[k].Close
			if (long && c > va.VAH) || (!long && c < va.VAL) {
				break // traversed without an accepted re-entry: day void
			}
			if !(c > va.VAL && c < va.VAH) {
				run = 0
				continue
			}
			if run == 0 || !bars[k].OpenTime.Equal(bars[k-1].CloseTime) {
				run, start = 1, k
			} else {
				run++
			}
			if run < p.Need {
				continue
			}
			if f, ok := entry(cs, bars, b30, atr30, start, k, va, side, short, p); ok {
				fires = append(fires, f)
			}
			break // first acceptance of the day only
		}
	}
	return fires
}

// entry builds §3.3's bracket at the close of accepting bar k; start is the
// re-entry bar.
func entry(cs []market.Candle, bars, b30 []htfBar, atr30 []float64, start, k int, va VA, side, short string, p Params) (autotrade.PaperFire, bool) {
	long := side == "long"
	t := bars[k].CloseTime
	// ATR14(30m) of the last complete 30m bar closing at or before t.
	j := sort.Search(len(b30), func(i int) bool { return b30[i].CloseTime.After(t) }) - 1
	if j < 0 || atr30[j] <= 0 {
		return autotrade.PaperFire{}, false
	}
	a := atr30[j]
	e := bars[k].Close
	var stop, tp float64
	if long {
		ext := va.VAL
		for i := bars[start].I0; i <= bars[k].I1; i++ {
			ext = math.Min(ext, cs[i].Low)
		}
		stop, tp = ext-p.StopBufATR*a, va.VAH
	} else {
		ext := va.VAH
		for i := bars[start].I0; i <= bars[k].I1; i++ {
			ext = math.Max(ext, cs[i].High)
		}
		stop, tp = ext+p.StopBufATR*a, va.VAL
	}
	risk := math.Abs(e - stop)
	pct := risk / e * 100
	rr := math.Abs(tp-e) / risk
	if pct < p.MinStopPct || rr < p.MinRR {
		return autotrade.PaperFire{}, false
	}
	return autotrade.PaperFire{
		Time: t, Symbol: short, TF: "1m", Strategy: strategyName, Side: side, Market: true,
		Entry: e, Stop: stop, TP: tp,
		Why: fmt.Sprintf("VAH %.2f VAL %.2f POC %.2f stop %.2f%% rr %.2f", va.VAH, va.VAL, va.POC, pct, rr),
	}, true
}

// ---- harness (windows / scoring / gate), as cmd/lvlbt ----

type window struct{ days, off int }

type stats struct {
	positions, tp, stop, open int
	netR, longR, shortR       float64
	payoffs                   []float64
	stopPcts, feeRs           []float64
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
	s.stopPcts = append(s.stopPcts, o.stopPcts...)
	s.feeRs = append(s.feeRs, o.feeRs...)
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

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	if len(c)%2 == 1 {
		return c[len(c)/2]
	}
	return (c[len(c)/2-1] + c[len(c)/2]) / 2
}

func main() {
	symbols := flag.String("symbols", "BTC,ETH", "comma-separated short names")
	daysS := flag.String("days", "60,90,120", "window lengths in days (comma list)")
	offS := flag.String("end-offset", "0", "window ends N days before now (comma list)")
	feeS := flag.String("fee-bps", "10,6,0", "round-trip fee in bps (comma list; the FIRST is the verdict fee)")
	noDedup := flag.Bool("no-dedup", false, "score every fire instead of one position per symbol")
	armS := flag.String("arm", "all", "all | 30mx2 | 1hx1")
	gate := flag.Bool("gate", false, "run shipgate (nested windows + 30d buckets)")
	cacheDir := flag.String("cache", "", "directory to cache 1m klines (JSON per symbol)")
	verbose := flag.Bool("v", false, "print every (symbol, window) row")
	trace := flag.Bool("trace", false, "print every position of the longest window")
	fetchOnly := flag.Bool("fetch-only", false, "fetch/cache the 1m series and exit (no results)")
	flag.Parse()

	days, offs, fees := ints(*daysS), ints(*offS), floats(*feeS)
	var wins []window
	for _, d := range days {
		for _, o := range offs {
			if d > 30 && o != 0 {
				continue
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

	type arm struct {
		name string
		p    Params
	}
	a30 := DefaultParams()
	a1h := DefaultParams()
	a1h.HTF, a1h.Need = time.Hour, 1
	var arms []arm
	for _, a := range []arm{{"30mx2", a30}, {"1hx1", a1h}} {
		if *armS == "all" || *armS == a.name {
			arms = append(arms, a)
		}
	}
	if len(arms) == 0 {
		log.Fatalf("no arm matches %q", *armS)
	}

	config.LoadDotEnv()
	client := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	now := time.Now().UTC().Truncate(time.Hour)
	maxSpan := 0
	for _, w := range wins {
		maxSpan = max(maxSpan, w.days+w.off)
	}
	fetchStart := now.AddDate(0, 0, -maxSpan-9)

	type series struct {
		short string
		cs    []market.Candle
	}
	var all []series
	for _, tok := range strings.Split(*symbols, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		sym, err := market.ResolveErr(tok)
		if err != nil {
			log.Print(err)
			continue
		}
		cs, err := fetchCached(client, sym, fetchStart, now, *cacheDir)
		if err != nil {
			log.Printf("%s: %v", tok, err)
			continue
		}
		log.Printf("%s: %d 1m bars %s → %s", tok, len(cs), cs[0].OpenTime.Format("2006-01-02"), cs[len(cs)-1].OpenTime.Format("2006-01-02 15:04"))
		all = append(all, series{tok, cs})
	}
	if *fetchOnly {
		return
	}

	for _, a := range arms {
		firesBy := map[string][]autotrade.PaperFire{}
		for _, s := range all {
			firesBy[s.short] = GenFires(s.cs, s.short, a.p)
			log.Printf("%s %s: %d fires", a.name, s.short, len(firesBy[s.short]))
		}
		for fi, fee := range fees {
			tag := ""
			if fi == 0 {
				tag = " (verdict fee)"
			}
			fmt.Printf("\n=== mp80bt %s · fee %.0fbps%s · dedup %v · now %s ===\n", a.name, fee, tag, !*noDedup, now.Format("2006-01-02 15:04"))
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
				for _, w := range wins {
					end := now.AddDate(0, 0, -w.off)
					start := end.AddDate(0, 0, -w.days)
					var in []autotrade.PaperFire
					for _, f := range firesBy[s.short] {
						if !f.Time.Before(start) && f.Time.Before(end) {
							in = append(in, f)
						}
					}
					st := score(in, s.cs, fee, *noDedup)
					by[w] = st
					pool[w].add(st)
					if *trace && fi == 0 && w == wins[0] {
						traceFires(in, s.cs, fee, *noDedup)
					}
				}
				res = append(res, symRes{s.short, by})
			}
			if *verbose {
				for _, r := range res {
					for _, w := range wins {
						printRow(r.short, w, r.by[w])
					}
				}
			}
			var nested, buckets []window
			for _, w := range wins {
				if w.days == 30 {
					buckets = append(buckets, w)
				} else if w.off == 0 {
					nested = append(nested, w)
				}
			}
			sort.Slice(nested, func(x, y int) bool { return nested[x].days < nested[y].days })
			fmt.Printf("%-5s | %-30s | %-12s | %-44s | %s\n", "sym", "nested netR (n)", "bucket sum", "buckets off0..", "verdict")
			line := func(name string, get func(window) stats) {
				var ns, bs []string
				ga := shipgate.Arm{Name: name}
				bsum := 0.0
				for _, w := range nested {
					st := get(w)
					ns = append(ns, fmt.Sprintf("%+.1f(%d)", st.netR, st.n()))
					ga.Windows = append(ga.Windows, shipgate.Window{Days: w.days, NetR: st.netR, Trades: st.n(), WinRate: st.wr()})
				}
				for _, w := range buckets {
					st := get(w)
					bs = append(bs, fmt.Sprintf("%+.1f(%d)", st.netR, st.n()))
					bsum += st.netR
					ga.Buckets = append(ga.Buckets, shipgate.Window{Days: w.off, NetR: st.netR, Trades: st.n(), WinRate: st.wr()})
				}
				verdict := ""
				if *gate && len(nested) > 0 {
					v := shipgate.Evaluate(ga, nil, shipgate.Default())
					verdict = fmt.Sprintf("%s medR/t %+.3f", v.Result, v.MedRPT)
					if len(v.Reasons) > 0 {
						verdict += " — " + strings.Join(v.Reasons, "; ")
					}
				}
				fmt.Printf("%-5s | %-30s | %+12.2f | %-44s | %s\n", name, strings.Join(ns, " "), bsum, strings.Join(bs, " "), verdict)
			}
			for _, r := range res {
				by := r.by
				line(r.short, func(w window) stats { return by[w] })
			}
			line("POOL", func(w window) stats { return *pool[w] })
			if len(nested) > 0 {
				w := nested[len(nested)-1]
				for _, r := range res {
					printRow(r.short, w, r.by[w])
				}
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
	ms := median(st.stopPcts)
	feeAtMed := 0.0
	if ms > 0 {
		feeAtMed = 0.10 / ms
	}
	fmt.Printf("  %-5s %4dd-%-3d pos %4d tp %3d stop %3d open %2d WR %5.1f%% netR %+8.2f R/t %+6.3f SD %5.2f SE %5.3f trd/d %5.2f ndR/t %+6.3f (n %d) L %+.1f S %+.1f | med stop %.2f%% fee10bp@med %.3fR meanFeeR(this fee) %.3f\n",
		name, w.days, w.off, st.positions, st.tp, st.stop, st.open, st.wr(), st.netR, st.rpt(), st.sd(), se,
		float64(st.positions)/float64(w.days), nd, st.ndN, st.longR, st.shortR, ms, feeAtMed, mean(st.feeRs))
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// fwdFrom hands EvaluateFire the series from the fire's own bar on: it only
// reads bars that closed after f.Time, and copying 260k bars per fire is the
// whole runtime otherwise.
func fwdFrom(cs []market.Candle, f autotrade.PaperFire) []market.Candle {
	i := sort.Search(len(cs), func(i int) bool { return !cs[i].CloseTime.Before(f.Time) })
	return cs[min(i, len(cs)-1):]
}

func resolver(cs []market.Candle, fee float64) func(autotrade.PaperFire) autotrade.Outcome {
	return func(f autotrade.PaperFire) autotrade.Outcome {
		return autotrade.WithFee(f, autotrade.EvaluateFire(f, fwdFrom(cs, f), 0), fee)
	}
}

func positionsOf(fires []autotrade.PaperFire, cs []market.Candle, fee float64, noDedup bool) []autotrade.Position {
	resolve := resolver(cs, fee)
	if noDedup {
		var ps []autotrade.Position
		for _, f := range fires {
			ps = append(ps, autotrade.Position{Fire: f, Outcome: resolve(f)})
		}
		return ps
	}
	return autotrade.DedupFires(fires, 0, 0, time.Minute, resolve)
}

func score(fires []autotrade.PaperFire, cs []market.Candle, fee float64, noDedup bool) stats {
	st := stats{}
	resolve := resolver(cs, fee)
	for _, f := range fires {
		oc := resolve(f)
		if oc.Status == autotrade.OutTP || oc.Status == autotrade.OutStop {
			st.ndN++
			st.ndR += oc.NetR
		}
	}
	ps := positionsOf(fires, cs, fee, noDedup)
	st.positions = len(ps)
	for _, pos := range ps {
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
		f := pos.Fire
		risk := math.Abs(f.Entry - f.Stop)
		st.stopPcts = append(st.stopPcts, risk/f.Entry*100)
		fill := oc.FillPrice
		if fill == 0 {
			fill = f.Entry
		}
		st.feeRs = append(st.feeRs, fee/10000*fill/risk)
	}
	return st
}

func traceFires(fires []autotrade.PaperFire, cs []market.Candle, fee float64, noDedup bool) {
	utc8 := time.FixedZone("UTC+8", 8*3600)
	for _, pos := range positionsOf(fires, cs, fee, noDedup) {
		f, oc := pos.Fire, pos.Outcome
		fmt.Printf("    %s %s %-5s entry %.2f stop %.2f tp %.2f → %-5s %+.2fR held %dm · %s\n",
			f.Time.In(utc8).Format("01-02 15:04"), f.Symbol, f.Side, f.Entry, f.Stop, f.TP, oc.Status, oc.NetR, oc.BarsHeld, f.Why)
	}
}

// fetchCached pages 1m history (BingX caps 1440/request) with a 100410
// back-off, and caches it as JSON when -cache is set (reused if < 6h old).
func fetchCached(client *bingx.Client, sym market.Symbol, start, end time.Time, dir string) ([]market.Candle, error) {
	var path string
	if dir != "" {
		path = filepath.Join(dir, strings.ReplaceAll(string(sym), "/", "_")+"_1m.json")
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < 6*time.Hour {
			if b, err := os.ReadFile(path); err == nil {
				var cs []market.Candle
				if json.Unmarshal(b, &cs) == nil && len(cs) > 0 && !cs[0].OpenTime.After(start.Add(time.Hour)) {
					return cs, nil
				}
			}
		}
	}
	// 5-day chunks, each retried on its own: one 100410 halfway through ~190
	// pages otherwise throws the whole symbol away.
	var cs []market.Candle
	for c0 := start; c0.Before(end); c0 = c0.Add(5 * 24 * time.Hour) {
		c1 := c0.Add(5*24*time.Hour - time.Minute)
		if c1.After(end) {
			c1 = end
		}
		var part []market.Candle
		var err error
		for try := 1; try <= 8; try++ {
			part, err = client.KlinesRange(context.Background(), sym, market.Timeframe("1m"), c0, c1)
			if err == nil {
				break
			}
			// 100410 (rate limit) and network timeouts both get a per-chunk back-off.
			log.Printf("%s chunk %s try %d: %v", sym, c0.Format("2006-01-02"), try, err)
			time.Sleep(time.Duration(try*10) * time.Second)
		}
		if err != nil {
			return nil, err
		}
		for _, c := range part {
			if !c.OpenTime.Before(c0) && !c.OpenTime.After(c1) && (len(cs) == 0 || c.OpenTime.After(cs[len(cs)-1].OpenTime)) {
				cs = append(cs, c)
			}
		}
	}
	if len(cs) < 1000 {
		return nil, fmt.Errorf("only %d bars", len(cs))
	}
	if path != "" {
		if b, e := json.Marshal(cs); e == nil {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return cs, nil
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
