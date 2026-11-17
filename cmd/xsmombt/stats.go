package main

// Stats helpers copied from cmd/trendbt (same definitions, so the numbers are
// comparable with docs/htf_trend_entry_variants_design.md §7).

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
)

type symData struct {
	short  string
	cs     []market.Candle
	trades []autostrat.TrendTrade
}

var nestedDays = []int{60, 90, 120}

const bucketLen, nBuckets = 30, 6

type tradeRef struct {
	sym string
	t   autostrat.TrendTrade
}

// collect returns the trades whose entry bar closed in (start, end].
func collect(data []symData, start, end time.Time, only string) []tradeRef {
	var out []tradeRef
	for _, d := range data {
		if only != "" && d.short != only {
			continue
		}
		for _, t := range d.trades {
			if t.EntryTime.After(start) && !t.EntryTime.After(end) {
				out = append(out, tradeRef{d.short, t})
			}
		}
	}
	return out
}

type winStats struct {
	n, wins, open           int
	net, rpt, wr            float64
	avgWin, avgLoss, sd, se float64
	mdd, perMonth           float64
	medStopPct              float64
	medLev, minLev          int
	best                    float64
	netExBest               float64
}

func stats(ts []tradeRef, days int) winStats {
	var s winStats
	s.n = len(ts)
	if s.n == 0 {
		return s
	}
	var sumW, sumL float64
	var nL int
	var stops []float64
	var levs []int
	s.minLev = math.MaxInt
	s.best = math.Inf(-1)
	for _, r := range ts {
		t := r.t
		s.net += t.NetR
		if t.NetR > 0 {
			s.wins++
			sumW += t.NetR
		} else {
			nL++
			sumL += t.NetR
		}
		if t.Open {
			s.open++
		}
		s.best = math.Max(s.best, t.NetR)
		stops = append(stops, t.StopPct)
		levs = append(levs, t.MaxLev)
		s.minLev = min(s.minLev, t.MaxLev)
	}
	s.rpt = s.net / float64(s.n)
	s.wr = float64(s.wins) / float64(s.n) * 100
	if s.wins > 0 {
		s.avgWin = sumW / float64(s.wins)
	}
	if nL > 0 {
		s.avgLoss = sumL / float64(nL)
	}
	if s.n > 1 {
		var v float64
		for _, r := range ts {
			v += (r.t.NetR - s.rpt) * (r.t.NetR - s.rpt)
		}
		s.sd = math.Sqrt(v / float64(s.n-1))
		s.se = s.sd / math.Sqrt(float64(s.n))
	}
	// Max drawdown on the closed-trade equity curve, in exit order (open
	// trades last, at their mark).
	ord := append([]tradeRef(nil), ts...)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].t.ExitTime.Before(ord[j].t.ExitTime) })
	var eq, peak float64
	for _, r := range ord {
		eq += r.t.NetR
		peak = math.Max(peak, eq)
		s.mdd = math.Max(s.mdd, peak-eq)
	}
	s.perMonth = float64(s.n) / (float64(days) / 30.44)
	sort.Float64s(stops)
	sort.Ints(levs)
	s.medStopPct = stops[len(stops)/2]
	s.medLev = levs[len(levs)/2]
	s.netExBest = s.net - s.best
	return s
}

// fullStats is every trade of the arm, with its span in days (first entry →
// now) for trades/day.
func fullStats(data []symData, now time.Time) (winStats, float64) {
	all := collect(data, time.Time{}, now, "")
	if len(all) == 0 {
		return winStats{}, 1
	}
	first := now
	for _, r := range all {
		if r.t.EntryTime.Before(first) {
			first = r.t.EntryTime
		}
	}
	span := now.Sub(first).Hours() / 24
	return stats(all, int(span)), span
}

// bucketNet is pooled netR per disjoint `days` bucket back from now until the
// bucket ends before `first`, newest first.
func bucketNet(data []symData, now time.Time, days int, first time.Time) []float64 {
	var out []float64
	for off := 0; ; off += days {
		end := now.AddDate(0, 0, -off)
		if end.Before(first) {
			break
		}
		out = append(out, stats(collect(data, end.AddDate(0, 0, -days), end, ""), days).net)
	}
	return out
}

func addSeries(a, b []float64) []float64 {
	n := min(len(a), len(b))
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] + b[i]
	}
	return out
}

func meanSD(x []float64) (float64, float64) {
	if len(x) == 0 {
		return 0, 0
	}
	var s float64
	for _, v := range x {
		s += v
	}
	m := s / float64(len(x))
	if len(x) < 2 {
		return m, 0
	}
	var v float64
	for _, y := range x {
		v += (y - m) * (y - m)
	}
	return m, math.Sqrt(v / float64(len(x)-1))
}

func dropBest(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	best := math.Inf(-1)
	for _, v := range x {
		s += v
		best = math.Max(best, v)
	}
	return s - best
}

// bucketT is mean / (SD/√n) of per-bucket differences; +Inf when they are
// all identical and non-negative, -Inf when identical and negative.
func bucketT(d []float64) float64 {
	if len(d) < 2 {
		return 0
	}
	var s float64
	for _, x := range d {
		s += x
	}
	mean := s / float64(len(d))
	var v float64
	for _, x := range d {
		v += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(v / float64(len(d)-1))
	if sd == 0 {
		switch {
		case mean > 0:
			return math.Inf(1)
		case mean < 0:
			return math.Inf(-1)
		}
		return 0
	}
	return mean / (sd / math.Sqrt(float64(len(d))))
}

// dailyR is the pooled change in TrendTrade.MTMR per UTC day, for the days
// whose end is in (from, now], oldest first. The MTM at a day's end is taken
// at the last bar closed by then (fees at entry, as MTMR).
func dailyR(data []symData, from, now time.Time) []float64 {
	d0 := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	var ends []time.Time
	for e := d0.AddDate(0, 0, 1); !e.After(now); e = e.AddDate(0, 0, 1) {
		ends = append(ends, e)
	}
	out := make([]float64, len(ends))
	for _, d := range data {
		ks := make([]int, len(ends)+1)
		ks[0] = lastClosed(d.cs, d0)
		for x, e := range ends {
			ks[x+1] = lastClosed(d.cs, e)
		}
		for _, t := range d.trades {
			// Days before the entry bar and after the exit bar contribute 0.
			lo := sort.Search(len(ends), func(x int) bool { return ks[x+1] >= t.EntryIdx })
			prev := mtm(t, d.cs, ks[min(lo, len(ends))])
			for x := lo; x < len(ends); x++ {
				cur := mtm(t, d.cs, ks[x+1])
				out[x] += cur - prev
				prev = cur
				if ks[x+1] >= t.ExitIdx {
					break
				}
			}
		}
	}
	return out
}

func mtm(t autostrat.TrendTrade, cs []market.Candle, k int) float64 {
	if k < 0 {
		return 0
	}
	return t.MTMR(cs, k)
}

func lastClosed(cs []market.Candle, t time.Time) int {
	return sort.Search(len(cs), func(i int) bool { return cs[i].CloseTime.After(t) }) - 1
}

func mtmMDD(daily []float64) float64 {
	var eq, peak, mdd float64
	for _, r := range daily {
		eq += r
		peak = math.Max(peak, eq)
		mdd = math.Max(mdd, peak-eq)
	}
	return mdd
}

func tail(x []float64, n int) []float64 {
	if len(x) <= n {
		return x
	}
	return x[len(x)-n:]
}

func pearson(a, b []float64) float64 {
	n := min(len(a), len(b))
	if n < 3 {
		return math.NaN()
	}
	a, b = a[len(a)-n:], b[len(b)-n:]
	ma, sa := meanSD(a)
	mb, sb := meanSD(b)
	if sa == 0 || sb == 0 {
		return math.NaN()
	}
	var c float64
	for i := 0; i < n; i++ {
		c += (a[i] - ma) * (b[i] - mb)
	}
	return c / float64(n-1) / (sa * sb)
}

// gateArmOf builds the shipgate input (one symbol when only != ""): nested
// 60/90/120 ending now with end-jitter 1 and 2 days, and six disjoint 30d
// buckets at end-offset 0..150.
func gateArmOf(data []symData, only string, now time.Time) shipgate.Arm {
	a := shipgate.Arm{Name: only}
	for _, w := range nestedDays {
		for j := 0; j <= 2; j++ {
			e := now.AddDate(0, 0, -j)
			st := stats(collect(data, e.AddDate(0, 0, -w), e, only), w)
			a.Windows = append(a.Windows, shipgate.Window{Days: w, NetR: st.net, Trades: st.n, WinRate: st.wr})
		}
	}
	for k := 0; k < nBuckets; k++ {
		off := k * bucketLen
		e := now.AddDate(0, 0, -off)
		st := stats(collect(data, e.AddDate(0, 0, -bucketLen), e, only), bucketLen)
		a.Buckets = append(a.Buckets, shipgate.Window{Days: off, NetR: st.net, Trades: st.n, WinRate: st.wr})
	}
	return a
}

func ptr(a shipgate.Arm) *shipgate.Arm { return &a }

// printGateTable: per symbol and pooled, the nested windows (un-jittered
// reading), 120d R/trade, the six 30d buckets and the absolute gate verdict.
func printGateTable(data []symData, now time.Time) {
	fmt.Printf("%-6s | %-40s | %-8s %-5s %-6s %-6s | %-44s | %s\n", "sym", "nested 60/90/120 netR(n)", "120R/t", "WR", "SE", "MDD", "30d buckets off 0..150 netR/n", "verdict")
	var names []string
	for _, d := range data {
		names = append(names, d.short)
	}
	names = append(names, "")
	for _, sym := range names {
		ga := gateArmOf(data, sym, now)
		var ns, bs []string
		for x := 0; x < len(ga.Windows); x += 3 {
			w := ga.Windows[x]
			ns = append(ns, fmt.Sprintf("%d:%+.1f(%d)", w.Days, w.NetR, w.Trades))
		}
		for _, b := range ga.Buckets {
			bs = append(bs, fmt.Sprintf("%+.1f/%d", b.NetR, b.Trades))
		}
		st := stats(collect(data, now.AddDate(0, 0, -120), now, sym), 120)
		v := shipgate.Evaluate(ga, nil, shipgate.Default())
		name := sym
		if name == "" {
			name = "POOL"
		}
		verdict := string(v.Result)
		if len(v.Reasons) > 0 {
			verdict += " — " + strings.Join(v.Reasons, "; ")
		}
		fmt.Printf("%-6s | %-40s | %+8.3f %4.0f%% %6.3f %6.2f | %-44s | %s\n", name, strings.Join(ns, " "), st.rpt, st.wr, st.se, st.mdd, strings.Join(bs, " "), verdict)
	}
}
