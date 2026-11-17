package main

// Entry variants of htf-trend (docs/htf_trend_entry_variants_design.md): the
// stop and the trail are the baseline's (3×ATR20 initial stop, WalkTrail with
// the 10-bar opposite channel, no target); only the entry differs.
//
//   - close: autostrat.SimulateHTFTrend (the baseline on paper).
//   - touch: a resting stop-entry at the prior N-bar channel, filled intrabar.
//   - tsmom: the sign of the N-bar return flips → enter at that bar's close.
//
// -variants-run is the pre-registered run of §5–6 in one command.

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
)

// trendTrade is one position entered on bar i at `entry`, walked by the
// baseline's trail (capTrade = runTrail's arithmetic, pinned equal to
// SimulateHTFTrend by TestSimulateCapped and TestTSMOMWalkIsBaselineWalk).
func trendTrade(cs []market.Candle, i int, side string, entry, stop float64, m int, feeBps float64) autostrat.TrendTrade {
	return capTrade(cs, i, autostrat.Trigger{Side: side, Entry: entry, Stop: stop}, m, feeBps)
}

// donchian is the max High / min Low of cs[i-n .. i-1] (the bars before i).
func donchian(cs []market.Candle, i, n int) (hi, lo float64) {
	hi, lo = math.Inf(-1), math.Inf(1)
	for _, c := range cs[i-n : i] {
		hi = math.Max(hi, c.High)
		lo = math.Min(lo, c.Low)
	}
	return hi, lo
}

// simulateTouch: design §3.2. Before bar i opens (flat, i >= first, i >= last
// exit + 1) stop-entries rest at the prior N-bar channel edges; ATR is
// atr[i-1] (known before bar i). Fill = the level, or the open on a gap
// through it. Both edges touched in one bar: the level nearer the open filled
// first, an exact tie is skipped. If the entry bar also reaches the initial
// stop the trade is a stop on that bar (conservative). Returns the trades and
// the number of both-touched bars.
func simulateTouch(cs []market.Candle, atr []float64, first int, wantLong, wantShort bool, p autostrat.HTFTrendParams, feeBps float64) ([]autostrat.TrendTrade, int) {
	var out []autostrat.TrendTrade
	both := 0
	if len(atr) != len(cs) || p.EntryN < 1 || p.StopATR <= 0 {
		return nil, 0
	}
	for i := max(first, p.EntryN, 1); i < len(cs); i++ {
		a := atr[i-1]
		if a <= 0 {
			continue
		}
		hi, lo := donchian(cs, i, p.EntryN)
		b := cs[i]
		longHit := wantLong && b.High >= hi
		shortHit := wantShort && b.Low <= lo
		var side string
		switch {
		case longHit && shortHit:
			both++
			dl, ds := math.Max(0, hi-b.Open), math.Max(0, b.Open-lo)
			switch {
			case dl < ds:
				side = "long"
			case ds < dl:
				side = "short"
			default:
				continue
			}
		case longHit:
			side = "long"
		case shortHit:
			side = "short"
		default:
			continue
		}
		d := p.StopATR * a
		var t autostrat.TrendTrade
		if side == "long" {
			entry := math.Max(hi, b.Open)
			stop := entry - d
			if b.Low <= stop {
				t = sameBarStop(cs, i, side, entry, stop, feeBps)
			} else {
				t = trendTrade(cs, i, side, entry, stop, p.TrailM, feeBps)
			}
		} else {
			entry := math.Min(lo, b.Open)
			stop := entry + d
			if b.High >= stop {
				t = sameBarStop(cs, i, side, entry, stop, feeBps)
			} else {
				t = trendTrade(cs, i, side, entry, stop, p.TrailM, feeBps)
			}
		}
		out = append(out, t)
		if t.Open {
			break
		}
		i = t.ExitIdx
	}
	return out, both
}

// sameBarStop is a touch fill whose own bar also reached the initial stop:
// −1R gross at the stop price, exit on the entry bar.
func sameBarStop(cs []market.Candle, i int, side string, entry, stop, feeBps float64) autostrat.TrendTrade {
	t := trendTrade(cs, i, side, entry, stop, 1, feeBps) // for Risk/Fee/StopPct/MaxLev only
	t.ExitIdx, t.ExitTime, t.Exit, t.Open, t.BarsHeld = i, cs[i].CloseTime, stop, false, 0
	t.GrossR = -1
	t.NetR = t.GrossR - t.FeeR
	return t
}

// tsmomSigns returns s[i] = sign(Close[i]/Close[i-n] − 1) for i >= n (an exact
// zero keeps s[i-1]); 0 where undefined.
func tsmomSigns(cs []market.Candle, n int) []int {
	s := make([]int, len(cs))
	if n < 1 {
		return s
	}
	for i := n; i < len(cs); i++ {
		switch r := cs[i].Close/cs[i-n].Close - 1; {
		case r > 0:
			s[i] = 1
		case r < 0:
			s[i] = -1
		default:
			s[i] = s[i-1]
		}
	}
	return s
}

// simulateTSMOM: design §3.3. While flat, a flip of the n-bar return sign at
// bar i enters that side at Close[i] with the baseline stop (3×atr[i]) and
// trail. Flips while in a position or on its exit bar are skipped (counted).
func simulateTSMOM(cs []market.Candle, atr []float64, first, n int, wantLong, wantShort bool, p autostrat.HTFTrendParams, feeBps float64) ([]autostrat.TrendTrade, int) {
	var out []autostrat.TrendTrade
	if len(atr) != len(cs) || n < 1 || p.StopATR <= 0 {
		return nil, 0
	}
	s := tsmomSigns(cs, n)
	skipped, free := 0, 0
	for i := max(first, n+1); i < len(cs); i++ {
		if s[i] == 0 || s[i-1] == 0 || s[i] == s[i-1] {
			continue
		}
		if i < free {
			skipped++
			continue
		}
		side := "long"
		if s[i] < 0 {
			side = "short"
		}
		if (side == "long" && !wantLong) || (side == "short" && !wantShort) || atr[i] <= 0 {
			continue
		}
		d := p.StopATR * atr[i]
		stop := cs[i].Close - d
		if side == "short" {
			stop = cs[i].Close + d
		}
		t := trendTrade(cs, i, side, cs[i].Close, stop, p.TrailM, feeBps)
		out = append(out, t)
		if t.Open {
			break
		}
		free = t.ExitIdx + 1
	}
	return out, skipped
}

// simulateEntry dispatches on -entry. "close" is SimulateHTFTrend itself.
func simulateEntry(entry string, tsN int, cs []market.Candle, atr []float64, first int, wantLong, wantShort bool, p autostrat.HTFTrendParams, feeBps float64) []autostrat.TrendTrade {
	switch entry {
	case "touch":
		t, _ := simulateTouch(cs, atr, first, wantLong, wantShort, p, feeBps)
		return t
	case "tsmom":
		t, _ := simulateTSMOM(cs, atr, first, tsN, wantLong, wantShort, p, feeBps)
		return t
	default:
		return autostrat.SimulateHTFTrend(cs, atr, first, wantLong, wantShort, p, feeBps)
	}
}

// ---- -variants-run ---------------------------------------------------------

type varArm struct {
	name  string // "C-4h", "T-1d", "M30-1d"
	tf    market.Timeframe
	data  []symData
	extra int // both-touched bars (T) or skipped flips (M)
}

type varSpec struct {
	name  string
	entry string
	n     int
}

func runVariants(load fetcher, symbols []string, fees []float64, corrStart time.Time) {
	p := autostrat.DefaultHTFTrendParams()
	wu := max(p.EntryN, p.ATRPeriod) + 1
	now := time.Now().UTC()
	plan := map[market.Timeframe][]varSpec{
		market.TF4h: {{"C", "close", 0}, {"T", "touch", 0}},
		market.TF1d: {{"C", "close", 0}, {"T", "touch", 0}, {"M30", "tsmom", 30}, {"M60", "tsmom", 60}},
	}
	type sym struct {
		short string
		cs    []market.Candle
		atr   []float64
	}
	bars := map[market.Timeframe][]sym{}
	for _, tf := range []market.Timeframe{market.TF4h, market.TF1d} {
		for _, tok := range symbols {
			tok = strings.ToUpper(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			s, short := resolve(tok)
			cs, err := load(s, tf)
			if err != nil || len(cs) < wu+70 {
				log.Fatalf("%s %s: klines %v (len %d)", short, tf, err, len(cs))
			}
			bars[tf] = append(bars[tf], sym{short, cs, indicator.ATR(cs, p.ATRPeriod)})
		}
	}
	fmt.Printf("=== htf-trend entry variants (docs/htf_trend_entry_variants_design.md) · now %s ===\n", now.Format("2006-01-02 15:04"))
	for _, tf := range []market.Timeframe{market.TF4h, market.TF1d} {
		for _, s := range bars[tf] {
			fmt.Printf("    %-3s %-5s %s → %s (%d bars)\n", tf, s.short, s.cs[0].OpenTime.Format("2006-01-02"),
				s.cs[len(s.cs)-1].CloseTime.Format("2006-01-02"), len(s.cs))
		}
	}

	for _, fee := range fees {
		fmt.Printf("\n\n################ fee %.0f bp %s ################\n", fee, map[bool]string{true: "(VERDICT)", false: "(reference)"}[fee == 10])
		arms := map[string]*varArm{}
		var order []string
		for _, tf := range []market.Timeframe{market.TF4h, market.TF1d} {
			for _, sp := range plan[tf] {
				a := &varArm{name: sp.name + "-" + string(tf), tf: tf}
				for _, s := range bars[tf] {
					var tr []autostrat.TrendTrade
					var x int
					switch sp.entry {
					case "touch":
						tr, x = simulateTouch(s.cs, s.atr, wu, true, true, p, fee)
					case "tsmom":
						tr, x = simulateTSMOM(s.cs, s.atr, wu, sp.n, true, true, p, fee)
					default:
						tr = autostrat.SimulateHTFTrend(s.cs, s.atr, wu, true, true, p, fee)
					}
					a.extra += x
					a.data = append(a.data, symData{short: s.short, cs: s.cs, trades: tr})
				}
				arms[a.name] = a
				order = append(order, a.name)
			}
		}

		// Full history.
		fmt.Printf("\n--- full history, pooled ---\n")
		fmt.Printf("%-8s %5s %9s %8s %5s %6s %6s %7s %7s %8s %9s %s\n", "arm", "n", "netR", "R/trade", "WR", "SD", "SE", "trd/day", "MDD", "MDDmtm", "60d t", "extra")
		for _, name := range order {
			a := arms[name]
			st, span := fullStats(a.data, now)
			b := bucketNet(a.data, now, 60, earliest(a.data, wu))
			bt, drop := bucketT(b), dropBest(b)
			dr := dailyR(a.data, corrStart, now)
			fmt.Printf("%-8s %5d %+9.2f %+8.4f %4.0f%% %6.2f %6.4f %7.3f %7.2f %8.2f %+9.2f %d   (span %.0fd; 60d buckets %d, drop-best %+.2f)\n",
				name, st.n, st.net, st.rpt, st.wr, st.sd, st.se, float64(st.n)/span, st.mdd, mtmMDD(dr), bt, a.extra, span, len(b), drop)
		}

		// Gate per TF: nested 60/90/120 (+jitter) and six 30d buckets.
		for _, name := range order {
			a := arms[name]
			base := arms["C-"+string(a.tf)]
			fmt.Printf("\n--- %s: nested windows + 30d buckets, shipgate.Default() ---\n", name)
			printGateTable(a, base, now)
		}

		// Correlation of daily R.
		fmt.Printf("\n--- daily R correlation (Pearson, pooled MTM per UTC day) ---\n")
		book := addSeries(dailyR(arms["C-4h"].data, corrStart, now), dailyR(arms["C-1d"].data, corrStart, now))
		refs := []struct {
			name string
			s    []float64
		}{
			{"C-4h", dailyR(arms["C-4h"].data, corrStart, now)},
			{"C-1d", dailyR(arms["C-1d"].data, corrStart, now)},
			{"C-book", book},
		}
		fmt.Printf("%-8s", "arm")
		for _, r := range refs {
			fmt.Printf("  %14s", "vs "+r.name)
		}
		fmt.Printf("   (full from %s | last 360d)\n", corrStart.Format("2006-01-02"))
		for _, name := range order {
			s := dailyR(arms[name].data, corrStart, now)
			fmt.Printf("%-8s", name)
			for _, r := range refs {
				fmt.Printf("  %+6.3f|%+6.3f", pearson(s, r.s), pearson(tail(s, 360), tail(r.s, 360)))
			}
			fmt.Println()
		}

		// Pre-registered decisions (§6).
		fmt.Printf("\n--- decisions (§6) ---\n")
		for _, tf := range []market.Timeframe{market.TF4h, market.TF1d} {
			t, c := arms["T-"+string(tf)], arms["C-"+string(tf)]
			v := shipgate.Evaluate(gateArmOf(t.data, "", now), ptr(gateArmOf(c.data, "", now)), shipgate.Default())
			ts, _ := fullStats(t.data, now)
			cst, _ := fullStats(c.data, now)
			first := earliest(append(append([]symData(nil), t.data...), c.data...), wu)
			diff := subSeries(bucketNet(t.data, now, 60, first), bucketNet(c.data, now, 60, first))
			dt := bucketT(diff)
			res := "UNDECIDED"
			switch {
			case v.Result == shipgate.Fail || ts.rpt <= cst.rpt:
				res = "FAIL"
			case v.Result == shipgate.Pass && dt >= 1:
				res = "ADOPT-candidate"
			}
			fmt.Printf("TOUCH %-3s gate vs C %-9s | full R/trade T %+.4f vs C %+.4f | 60d paired diff t %+.2f → %s\n", tf, v.Result, ts.rpt, cst.rpt, dt, res)
			if len(v.Reasons) > 0 {
				fmt.Printf("          gate: %s\n", strings.Join(v.Reasons, "; "))
			}
		}
		m30, m60 := arms["M30-1d"], arms["M60-1d"]
		v30 := shipgate.Evaluate(gateArmOf(m30.data, "", now), nil, shipgate.Default())
		v60 := shipgate.Evaluate(gateArmOf(m60.data, "", now), nil, shipgate.Default())
		vb := shipgate.Evaluate(gateArmOf(m30.data, "", now), ptr(gateArmOf(arms["C-1d"].data, "", now)), shipgate.Default())
		s30, _ := fullStats(m30.data, now)
		s60, _ := fullStats(m60.data, now)
		drop30 := dropBest(bucketNet(m30.data, now, 60, earliest(m30.data, wu)))
		corr := pearson(dailyR(m30.data, corrStart, now), book)
		res := "UNDECIDED"
		switch {
		case v30.Result == shipgate.Fail || s30.net <= 0 || drop30 <= 0:
			res = "FAIL"
		case v30.Result == shipgate.Pass && s60.rpt > 0 && v60.Result != shipgate.Fail:
			res = "PASS"
		case v30.Result == shipgate.Pass:
			res = "UNDECIDED (neighbour N=60 fails)"
		}
		label := "diversifying"
		if corr > 0.7 {
			label = "DUPLICATE"
		} else if corr >= 0.4 {
			label = "overlapping"
		}
		fmt.Printf("TSMOM-30 gate %s | full netR %+.2f drop-best-60d %+.2f | N60 gate %s R/trade %+.4f | corr vs C-book %+.3f (%s) → %s\n",
			v30.Result, s30.net, drop30, v60.Result, s60.rpt, corr, label, res)
		for _, r := range [][]string{v30.Reasons, v60.Reasons} {
			if len(r) > 0 {
				fmt.Printf("          gate: %s\n", strings.Join(r, "; "))
			}
		}
		fmt.Printf("          info: TSMOM-30 gate with C-1d as baseline → %s %s\n", vb.Result, strings.Join(vb.Reasons, "; "))
	}
}

func ptr(a shipgate.Arm) *shipgate.Arm { return &a }

var nestedDays = []int{60, 90, 120}

const bucketLen, nBuckets = 30, 6

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

func printGateTable(a, base *varArm, now time.Time) {
	fmt.Printf("%-6s | %-40s | %-8s %-7s %-6s %-6s | %-38s | %s\n", "sym", "nested 60/90/120 netR(n)", "120R/t", "WR", "SE", "MDD", "30d buckets off 0..150", "verdict")
	names := []string{}
	for _, d := range a.data {
		names = append(names, d.short)
	}
	names = append(names, "")
	for _, sym := range names {
		ga := gateArmOf(a.data, sym, now)
		var ns, bs []string
		for x := 0; x < len(ga.Windows); x += 3 { // every third is the un-jittered reading
			w := ga.Windows[x]
			ns = append(ns, fmt.Sprintf("%d:%+.1f(%d)", w.Days, w.NetR, w.Trades))
		}
		for _, b := range ga.Buckets {
			bs = append(bs, fmt.Sprintf("%+.1f/%d", b.NetR, b.Trades))
		}
		st := stats(collect(a.data, now.AddDate(0, 0, -120), now, sym), 120)
		var v shipgate.Verdict
		if base != nil && base != a {
			v = shipgate.Evaluate(ga, ptr(gateArmOf(base.data, sym, now)), shipgate.Default())
		} else {
			v = shipgate.Evaluate(ga, nil, shipgate.Default())
		}
		name := sym
		if name == "" {
			name = "POOL"
		}
		verdict := string(v.Result)
		if len(v.Reasons) > 0 {
			verdict += " — " + strings.Join(v.Reasons, "; ")
		}
		fmt.Printf("%-6s | %-40s | %+8.3f %4.0f%%  %6.3f %6.2f | %-38s | %s\n", name, strings.Join(ns, " "), st.rpt, st.wr, st.se, st.mdd, strings.Join(bs, " "), verdict)
	}
}

// earliest is the first signal bar close across symbols.
func earliest(data []symData, wu int) time.Time {
	e := time.Now().UTC()
	for _, d := range data {
		if t := d.cs[min(wu, len(d.cs)-1)].CloseTime; t.Before(e) {
			e = t
		}
	}
	return e
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

func subSeries(a, b []float64) []float64 {
	n := min(len(a), len(b))
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] - b[i]
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
