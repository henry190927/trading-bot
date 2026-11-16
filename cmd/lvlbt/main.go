// Command lvlbt backtests the "level-reclaim" rule (docs/level_reclaim_design.md):
// a 15m/1h CLOSE holds or reclaims a session level (D/W/Asia/London/NY open,
// prior-day and session highs/lows), then the first 1m displacement bar in
// that direction is a market entry, stop beyond the 1m extreme since the
// event bar, TP 2R.
//
// Everything is derived from ONE 1m series per symbol: HTF bars are
// UTC-aligned aggregates of closed 1m bars, levels are read from 1m bars that
// closed before the level is used. So a fire at 1m bar b depends only on bars
// <= b (TestNoLookahead poisons every later bar and requires the same fires).
//
// Scoring: autotrade.EvaluateFire on the 1m series → autotrade.WithFee →
// autotrade.DedupFires (one position per symbol, cooldown 0, no timeout —
// the paper executor's semantics). Windows/buckets/gate as cmd/pivotbt.
//
//	go run ./cmd/lvlbt -symbols BTC,ETH -days 60,90,120,30 -end-offset 0,30,60,90,120,150 -fee-bps 10,6 -arm all -gate -cache <dir>
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

const strategyName = "level-reclaim"

// Params is §3 of the design doc. Fixed before results; the flags exist only
// for the pre-registered arms (HTF, K, level set).
type Params struct {
	HTF        time.Duration // 15m or 1h
	K          int           // 1 = event bar is the signal; 2 = one more close on the same side
	OpensOnly  bool          // "opens" level set vs "all"
	ScanBars   int           // 1m bars after the signal to look for the action bar
	BodyATR    float64       // action-bar body >= BodyATR * ATR20(1m)
	CloseFrac  float64       // close in the outer CloseFrac of the bar's range
	StopBufATR float64       // stop buffer beyond the 1m extreme
	MinStopPct float64       // skip below (fees)
	MaxStopPct float64       // skip above
	RMult      float64       // TP in R
	MergePct   float64       // levels closer than this are one level
}

func DefaultParams() Params {
	return Params{HTF: 15 * time.Minute, K: 1, ScanBars: 60, BodyATR: 1.0, CloseFrac: 0.30,
		StopBufATR: 0.1, MinStopPct: 0.15, MaxStopPct: 2.0, RMult: 2.0, MergePct: 0.05}
}

// Level ids in priority order: when two coincide (MergePct) the earlier wins.
var levelOrder = []string{"DO", "WO", "AO", "LO", "NYO", "PDH", "PDL", "AH", "AL", "LH", "LL", "NYH", "NYL"}

var isOpen = map[string]bool{"DO": true, "WO": true, "AO": true, "LO": true, "NYO": true}

// Level is one instance: a price usable for HTF bars inside [From, To).
type Level struct {
	ID       string
	Price    float64
	From, To time.Time
	key      string // unique per instance (id + From)
}

var et = mustLoc("America/New_York")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		log.Fatalf("tz %s: %v", name, err)
	}
	return l
}

// idxAtOrAfter returns the first bar with OpenTime >= t (len(cs) if none).
func idxAtOrAfter(cs []market.Candle, t time.Time) int {
	return sort.Search(len(cs), func(i int) bool { return !cs[i].OpenTime.Before(t) })
}

// openAt is the open of the bar starting exactly at t (5 min tolerance for a
// missing bar); ok=false if the series does not cover t.
func openAt(cs []market.Candle, t time.Time) (float64, bool) {
	i := idxAtOrAfter(cs, t)
	if i >= len(cs) || cs[i].OpenTime.Sub(t) > 5*time.Minute || i == 0 {
		return 0, false
	}
	return cs[i].Open, true
}

// hl is the high/low of bars with OpenTime in [t0, t1); ok=false unless the
// series covers the whole span (a partial session is not a level).
func hl(cs []market.Candle, t0, t1 time.Time) (float64, float64, bool) {
	i := idxAtOrAfter(cs, t0)
	if i == 0 || i >= len(cs) || cs[i].OpenTime.Sub(t0) > 5*time.Minute {
		return 0, 0, false
	}
	j := idxAtOrAfter(cs, t1)
	if j >= len(cs) || j <= i {
		return 0, 0, false
	}
	h, l := cs[i].High, cs[i].Low
	for k := i + 1; k < j; k++ {
		h = math.Max(h, cs[k].High)
		l = math.Min(l, cs[k].Low)
	}
	return h, l, true
}

// BuildLevels lists every level instance §3.1 defines over the series.
func BuildLevels(cs []market.Candle) []Level {
	if len(cs) == 0 {
		return nil
	}
	var out []Level
	add := func(id string, p float64, from, to time.Time) {
		out = append(out, Level{ID: id, Price: p, From: from, To: to, key: id + "@" + from.Format(time.RFC3339)})
	}
	first := cs[0].OpenTime.UTC()
	last := cs[len(cs)-1].CloseTime.UTC()
	d0 := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)
	for d := d0; d.Before(last); d = d.AddDate(0, 0, 1) {
		next := d.AddDate(0, 0, 1)
		if p, ok := openAt(cs, d); ok {
			add("DO", p, d, next)
		}
		if d.Weekday() == time.Monday {
			if p, ok := openAt(cs, d); ok {
				add("WO", p, d, d.AddDate(0, 0, 7))
			}
		}
		if h, l, ok := hl(cs, d.AddDate(0, 0, -1), d); ok {
			add("PDH", h, d, next)
			add("PDL", l, d, next)
		}
	}
	// ET sessions, DST-aware: iterate ET calendar dates.
	fe := first.In(et)
	e0 := time.Date(fe.Year(), fe.Month(), fe.Day(), 0, 0, 0, 0, et)
	for e := e0.AddDate(0, 0, -1); e.Before(last); e = e.AddDate(0, 0, 1) {
		at := func(h, m int) time.Time { return time.Date(e.Year(), e.Month(), e.Day(), h, m, 0, 0, et).UTC() }
		// Asia: 20:00 ET of date e → 00:00 ET of e+1.
		aStart := at(20, 0)
		aEnd := time.Date(e.Year(), e.Month(), e.Day()+1, 0, 0, 0, 0, et).UTC()
		if p, ok := openAt(cs, aStart); ok {
			add("AO", p, aStart, aStart.Add(24*time.Hour))
		}
		if h, l, ok := hl(cs, aStart, aEnd); ok {
			add("AH", h, aEnd, aStart.Add(24*time.Hour))
			add("AL", l, aEnd, aStart.Add(24*time.Hour))
		}
		lStart, lEnd := at(2, 0), at(5, 0)
		if p, ok := openAt(cs, lStart); ok {
			add("LO", p, lStart, lStart.Add(24*time.Hour))
		}
		if h, l, ok := hl(cs, lStart, lEnd); ok {
			add("LH", h, lEnd, lStart.Add(24*time.Hour))
			add("LL", l, lEnd, lStart.Add(24*time.Hour))
		}
		nStart, nEnd := at(9, 30), at(11, 0)
		if p, ok := openAt(cs, nStart); ok {
			add("NYO", p, nStart, nStart.Add(24*time.Hour))
		}
		if h, l, ok := hl(cs, nStart, nEnd); ok {
			add("NYH", h, nEnd, nStart.Add(24*time.Hour))
			add("NYL", l, nEnd, nStart.Add(24*time.Hour))
		}
	}
	return out
}

// htfBar is a UTC-aligned aggregate of 1m bars [I0, I1].
type htfBar struct {
	Open, High, Low, Close float64
	OpenTime, CloseTime    time.Time
	I0, I1                 int
}

// Aggregate builds complete HTF bars only: a bucket whose last 1m bar does not
// close at the bucket end (still forming, or a gap at the end) is dropped.
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
		if !cs[j].OpenTime.Add(time.Minute).Before(end) { // last 1m bar is the bucket's final minute
			out = append(out, b)
		}
		i = j + 1
	}
	return out
}

// GenFires runs §3 over one symbol's 1m series and returns fires oldest first.
func GenFires(cs []market.Candle, short string, p Params) []autotrade.PaperFire {
	levels := BuildLevels(cs)
	sort.Slice(levels, func(a, b int) bool { return levels[a].From.Before(levels[b].From) })
	bars := Aggregate(cs, p.HTF)
	atr := indicator.ATR(cs, 20)
	rank := map[string]int{}
	for i, id := range levelOrder {
		rank[id] = i
	}
	used := map[string]bool{} // level key + side
	var fires []autotrade.PaperFire

	for j := 2; j < len(bars); j++ {
		b, prev := bars[j], bars[j-1]
		// Active levels: formed before the bar opened and still live at its close.
		var act []Level
		for _, l := range levels {
			if l.From.After(bars[j-p.K+1].OpenTime) {
				break
			}
			if p.OpensOnly && !isOpen[l.ID] {
				continue
			}
			if b.CloseTime.After(l.To) {
				continue
			}
			act = append(act, l)
		}
		sort.SliceStable(act, func(a, c int) bool { return rank[act[a].ID] < rank[act[c].ID] })
		var kept []Level
	merge:
		for _, l := range act {
			for _, k := range kept {
				if math.Abs(l.Price-k.Price)/k.Price*100 < p.MergePct {
					continue merge
				}
			}
			kept = append(kept, l)
		}

		for _, l := range kept {
			// event bar e = j (K=1) or j-1 (K=2), judged against its own previous close.
			e, ePrev := b, prev
			if p.K == 2 {
				e, ePrev = prev, bars[j-2]
			}
			var side string
			switch {
			case e.Close > l.Price && (ePrev.Close < l.Price || e.Low < l.Price):
				side = "long"
			case e.Close < l.Price && (ePrev.Close > l.Price || e.High > l.Price):
				side = "short"
			default:
				continue
			}
			if p.K == 2 && ((side == "long" && b.Close <= l.Price) || (side == "short" && b.Close >= l.Price)) {
				continue
			}
			uk := l.key + "|" + side
			if used[uk] {
				continue
			}
			f, ok := action(cs, atr, b.I1, e.I0, l, side, short, p)
			used[uk] = true // the signal is spent whether or not the 1m action came
			if ok {
				fires = append(fires, f)
			}
		}
	}
	sort.SliceStable(fires, func(a, b int) bool { return fires[a].Time.Before(fires[b].Time) })
	return fires
}

// action scans the 1m bars after the signal (last 1m index sig) for §3.3's
// displacement bar; ext0 is the first 1m bar of the event HTF bar (stop range).
func action(cs []market.Candle, atr []float64, sig, ext0 int, l Level, side, short string, p Params) (autotrade.PaperFire, bool) {
	long := side == "long"
	for i := sig + 1; i <= sig+p.ScanBars && i < len(cs); i++ {
		c := cs[i]
		if (long && c.Close < l.Price) || (!long && c.Close > l.Price) {
			return autotrade.PaperFire{}, false // closed back through the level: cancelled
		}
		a := atr[i-1]
		rng := c.High - c.Low
		if a <= 0 || rng <= 0 {
			continue
		}
		body := c.Close - c.Open
		if !long {
			body = -body
		}
		if body < p.BodyATR*a {
			continue
		}
		if long && (c.Close-c.Low)/rng < 1-p.CloseFrac {
			continue
		}
		if !long && (c.High-c.Close)/rng < 1-p.CloseFrac {
			continue
		}
		// Trigger. Stop beyond the extreme since the event bar opened.
		ext := cs[ext0].Low
		if !long {
			ext = cs[ext0].High
		}
		for k := ext0; k <= i; k++ {
			if long {
				ext = math.Min(ext, cs[k].Low)
			} else {
				ext = math.Max(ext, cs[k].High)
			}
		}
		entry := c.Close
		var stop, tp float64
		if long {
			stop = ext - p.StopBufATR*a
			tp = entry + p.RMult*(entry-stop)
		} else {
			stop = ext + p.StopBufATR*a
			tp = entry - p.RMult*(stop-entry)
		}
		pct := math.Abs(entry-stop) / entry * 100
		if pct < p.MinStopPct || pct > p.MaxStopPct {
			return autotrade.PaperFire{}, false
		}
		return autotrade.PaperFire{
			Time: c.CloseTime, Symbol: short, TF: "1m", Strategy: strategyName, Side: side, Market: true,
			Entry: entry, Stop: stop, TP: tp,
			Why: fmt.Sprintf("%s %.2f stop %.2f%%", l.ID, l.Price, pct),
		}, true
	}
	return autotrade.PaperFire{}, false
}

// ---- harness (windows / scoring / gate), as cmd/pivotbt ----

type window struct{ days, off int }

type stats struct {
	positions, tp, stop, open int
	netR, longR, shortR       float64
	payoffs                   []float64
	ndN                       int
	ndR                       float64
	byLevel                   map[string]float64
	byLevelN                  map[string]int
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
	if s.byLevel == nil {
		s.byLevel, s.byLevelN = map[string]float64{}, map[string]int{}
	}
	for k, v := range o.byLevel {
		s.byLevel[k] += v
		s.byLevelN[k] += o.byLevelN[k]
	}
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
	symbols := flag.String("symbols", "BTC,ETH", "comma-separated short names")
	daysS := flag.String("days", "60,90,120", "window lengths in days (comma list)")
	offS := flag.String("end-offset", "0", "window ends N days before now (comma list)")
	feeS := flag.String("fee-bps", "10,6", "round-trip fee in bps (comma list; the FIRST is the verdict fee)")
	noDedup := flag.Bool("no-dedup", false, "score every fire instead of one position per symbol")
	armS := flag.String("arm", "all", "all | e.g. 15m-K1-opens")
	gate := flag.Bool("gate", false, "run shipgate (nested windows + 30d buckets)")
	cacheDir := flag.String("cache", "", "directory to cache 1m klines (JSON per symbol)")
	verbose := flag.Bool("v", false, "print every (symbol, window) row")
	trace := flag.Bool("trace", false, "print every position of the longest window")
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
	var arms []arm
	for _, set := range []string{"opens", "all"} {
		for _, h := range []time.Duration{15 * time.Minute, time.Hour} {
			for _, k := range []int{1, 2} {
				p := DefaultParams()
				p.HTF, p.K, p.OpensOnly = h, k, set == "opens"
				name := fmt.Sprintf("%s-K%d-%s", strings.TrimSuffix(strings.TrimSuffix(h.String(), "0s"), "0m"), k, set)
				if *armS == "all" || *armS == name {
					arms = append(arms, arm{name, p})
				}
			}
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
	fetchStart := now.AddDate(0, 0, -maxSpan-9) // prior week + day before the first window

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

	for _, a := range arms {
		firesBy := map[string][]autotrade.PaperFire{}
		for _, s := range all {
			firesBy[s.short] = GenFires(s.cs, s.short, a.p)
		}
		for fi, fee := range fees {
			tag := ""
			if fi == 0 {
				tag = " (verdict fee)"
			}
			fmt.Printf("\n=== lvlbt %s · fee %.0fbps%s · dedup %v · now %s ===\n", a.name, fee, tag, !*noDedup, now.Format("2006-01-02 15:04"))
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
						ot := f.Time.Add(-time.Minute + time.Millisecond)
						if !ot.Before(start) && ot.Before(end) {
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
					bs = append(bs, fmt.Sprintf("%+.1f", st.netR))
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
				printRow("POOL", w, *pool[w])
				st := *pool[w]
				var ids []string
				for id := range st.byLevel {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				var parts []string
				for _, id := range ids {
					parts = append(parts, fmt.Sprintf("%s %+.1f(%d)", id, st.byLevel[id], st.byLevelN[id]))
				}
				fmt.Printf("  by level %dd (diagnostic only): %s\n", w.days, strings.Join(parts, " · "))
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
	st := stats{byLevel: map[string]float64{}, byLevelN: map[string]int{}}
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
		id := strings.Fields(pos.Fire.Why)[0]
		st.byLevel[id] += oc.NetR
		st.byLevelN[id]++
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
			if err == nil || !strings.Contains(err.Error(), "100410") {
				break
			}
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
