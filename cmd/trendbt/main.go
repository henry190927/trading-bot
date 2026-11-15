// Command trendbt backtests the HTF trend follower (Donchian breakout entry,
// opposite-channel trailing exit, no target) — docs/htf_trend_design.md.
//
// Scoring is autostrat.SimulateHTFTrend, not autotrade.EvaluateFire: the
// strategy has no take-profit, so a fixed stop/TP scorer cannot represent it.
//
// ONE continuous simulation per symbol over all available history (BingX
// public klines, paged back to the listing date), one position at a time.
// Each trade belongs to the window containing its ENTRY bar, so -days /
// -end-offset select trades out of that single run: nested windows are
// prefixes of it and disjoint buckets partition it exactly. A trade that runs
// past a window's end is scored to its real exit; one still open at the last
// closed bar is marked to that close (and pays the fee) and counted as open.
//
// Public klines need no API key; this command never reads .env.
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
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

type symData struct {
	short  string
	cs     []market.Candle
	trades []autostrat.TrendTrade
}

func main() {
	symbols := flag.String("symbols", "BTC,ETH,SOL,XRP,SUI,HYPE,NEAR,LINK,XAG", "comma-separated short names or raw contract codes")
	tfStr := flag.String("tf", "4h", "timeframe (pre-registered: 4h = variant A, 1d = variant B)")
	days := flag.Int("days", 120, "window length in days (trades attributed by entry bar)")
	endOff := flag.Int("end-offset", 0, "end the window N days before now; -days 60 -end-offset 60 is a disjoint bucket")
	feeBps := flag.Float64("fee-bps", 10, "round-trip fee in bps of notional (autotrade.WithFee conversion). 10 = taker in + stop-market out")
	warmup := flag.Int("warmup", 0, "bars at the start of each symbol's history on which no signal is taken (0 = max(N, ATR period)+1, the minimum for warm indicators)")
	side := flag.String("side", "auto", "long | short | auto")
	n := flag.Int("n", 0, "entry channel bars (0 = pre-registered 20)")
	m := flag.Int("m", 0, "trail channel bars (0 = pre-registered 10)")
	k := flag.Float64("k", 0, "initial stop in ATR (0 = pre-registered 3.0)")
	report := flag.Bool("report", false, "full report: nested -windows, disjoint -bucket-days buckets back through history, calendar periods, gate input lines")
	windowsS := flag.String("windows", "120,240,360", "nested windows for -report")
	bucketDays := flag.Int("bucket-days", 60, "disjoint bucket length for -report")
	showTrades := flag.Bool("trades", false, "print every trade in the window")
	cacheDir := flag.String("cache", "", "directory to cache klines in (JSON per symbol+tf; reused if younger than -cache-ttl)")
	cacheTTL := flag.Duration("cache-ttl", 6*time.Hour, "max cache age")
	since := flag.String("since", "2019-01-01", "earliest date to page back to; with -compare-log, the date the compare starts from (history is then paged from 2019-01-01)")
	compareLog := flag.String("compare-log", "", "paper log (autotrade_paper.jsonl) to match fire by fire against the backtest since -since (compare.go)")
	blockedLog := flag.String("blocked-log", "", "with -compare-log: the blocked-candidate log that explains missing signals")
	replayDays := flag.Int("replay-days", 0, "offline replay: emulate the paper executor over the last N days and score it through the live resolver and the panel path (compare.go)")
	replayOut := flag.String("replay-out", "", "with -replay-days: write the synthetic paper log here")
	riskPct := flag.Float64("risk-pct", 0.25, "with -replay-days: the rules' risk_pct written on synthetic fires")
	bookPct := flag.Float64("book-pct", 1.5, "with -replay-days: the book's risk_per_trade_pct the panel normalises to")
	flag.Parse()

	var compareSince time.Time
	if *compareLog != "" {
		t, err := time.Parse("2006-01-02", *since)
		if err != nil || *since == "2019-01-01" {
			log.Fatalf("-compare-log needs -since YYYY-MM-DD (the forward test's start)")
		}
		compareSince, *since = t, "2019-01-01"
	}
	if (*compareLog != "" || *replayDays > 0) && strategyForTF(market.Timeframe(*tfStr)) == "" {
		log.Fatalf("-tf %s has no htf-trend strategy (4h = htf-trend, 1d = htf-trend-1d)", *tfStr)
	}

	p := autostrat.DefaultHTFTrendParams()
	if *n > 0 {
		p.EntryN = *n
	}
	if *m > 0 {
		p.TrailM = *m
	}
	if *k > 0 {
		p.StopATR = *k
	}
	if p != autostrat.DefaultHTFTrendParams() {
		fmt.Println("!!! OFF the pre-registered parameters — exploratory, not a verdict")
	}
	tf := market.Timeframe(*tfStr)
	wantLong, wantShort := *side != "short", *side != "long"
	wu := *warmup
	if wu <= 0 {
		wu = max(p.EntryN, p.ATRPeriod) + 1
	}
	from, err := time.Parse("2006-01-02", *since)
	if err != nil {
		log.Fatalf("-since: %v", err)
	}

	client := bingx.New("", "")
	var data []symData
	for _, tok := range strings.Split(*symbols, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		sym, short := resolve(tok)
		cs, err := fetch(client, sym, tf, from, *cacheDir, *cacheTTL)
		if err != nil || len(cs) < wu+5 {
			log.Printf("%s: klines %v (len %d) — skipped", short, err, len(cs))
			continue
		}
		atr := indicator.ATR(cs, p.ATRPeriod)
		tr := autostrat.SimulateHTFTrend(cs, atr, wu, wantLong, wantShort, p, *feeBps)
		data = append(data, symData{short: short, cs: cs, trades: tr})
	}
	if len(data) == 0 {
		log.Fatal("no data")
	}

	if *compareLog != "" || *replayDays > 0 {
		if p != autostrat.DefaultHTFTrendParams() || *side != "auto" {
			log.Fatal("-compare-log / -replay-days compare against the executor, which runs the pre-registered parameters on both sides")
		}
		if *compareLog != "" {
			os.Exit(runCompareLog(data, tf, *compareLog, *blockedLog, compareSince))
		}
		os.Exit(runReplay(data, tf, *replayDays, *riskPct, *bookPct, *replayOut))
	}

	now := time.Now().UTC()
	fmt.Printf("=== htf-trend · %s · N %d / M %d / K %.1f×ATR%d · fee %.1fbp · side %s · warmup %d bars ===\n",
		tf, p.EntryN, p.TrailM, p.StopATR, p.ATRPeriod, *feeBps, *side, wu)
	for _, d := range data {
		fmt.Printf("    %-5s history %s → %s (%d bars), first signal bar %s\n", d.short,
			d.cs[0].OpenTime.Format("2006-01-02"), d.cs[len(d.cs)-1].CloseTime.Format("2006-01-02"), len(d.cs),
			d.cs[min(wu, len(d.cs)-1)].CloseTime.Format("2006-01-02"))
	}

	if !*report {
		end := now.AddDate(0, 0, -*endOff)
		printWindow(data, end.AddDate(0, 0, -*days), end, *days, *showTrades)
		return
	}

	// Nested windows.
	var wins []int
	for _, s := range strings.Split(*windowsS, ",") {
		var w int
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &w); err == nil && w > 0 {
			wins = append(wins, w)
		}
	}
	var gateArm []string
	for _, w := range wins {
		end := now.AddDate(0, 0, -*endOff)
		st := printWindow(data, end.AddDate(0, 0, -w), end, w, *showTrades)
		gateArm = append(gateArm, fmt.Sprintf("%d %.2f %d", w, st.net, st.n))
		// Jitter readings (end shifted 1 and 2 days), for cmd/gate's sign-robustness test.
		for j := 1; j <= 2; j++ {
			e := end.AddDate(0, 0, -j)
			js := stats(collect(data, e.AddDate(0, 0, -w), e, ""), w)
			gateArm = append(gateArm, fmt.Sprintf("%d %.2f %d", w, js.net, js.n))
		}
	}

	// Disjoint buckets, back as far as any symbol can signal.
	earliest := now
	for _, d := range data {
		if t := d.cs[min(wu, len(d.cs)-1)].CloseTime; t.Before(earliest) {
			earliest = t
		}
	}
	fmt.Printf("\n--- disjoint %dd buckets, pooled (trades by entry bar) ---\n", *bucketDays)
	fmt.Printf("%-23s %5s %9s %8s %5s  %s\n", "bucket (end-offset)", "n", "netR", "R/trade", "WR", "per-symbol netR")
	var gateBuckets []string
	var bnet []float64
	for off := *endOff; ; off += *bucketDays {
		end := now.AddDate(0, 0, -off)
		start := end.AddDate(0, 0, -*bucketDays)
		if end.Before(earliest) {
			break
		}
		all := collect(data, start, end, "")
		st := stats(all, *bucketDays)
		var per []string
		for _, d := range data {
			ts := stats(collect([]symData{d}, start, end, ""), *bucketDays)
			if ts.n > 0 {
				per = append(per, fmt.Sprintf("%s %+.1f/%d", d.short, ts.net, ts.n))
			}
		}
		fmt.Printf("%s..%s (%4d) %5d %+9.2f %+8.3f %4.0f%%  %s\n", start.Format("06-01-02"), end.Format("06-01-02"), off,
			st.n, st.net, st.rpt, st.wr, strings.Join(per, " "))
		if st.n == 0 {
			continue // empty leading bucket: no evidence either way, keep it out of the gate
		}
		gateBuckets = append(gateBuckets, fmt.Sprintf("%d %.2f %d", off, st.net, st.n))
		bnet = append(bnet, st.net)
	}
	// Buckets as the unit of observation: cross-symbol trades are correlated
	// (one crypto move fires most of the book at once), so the per-trade SE
	// overstates the evidence. The bucket-level t treats each period as one draw.
	if len(bnet) > 2 {
		var sum, v float64
		pos := 0
		for _, x := range bnet {
			sum += x
			if x > 0 {
				pos++
			}
		}
		mean := sum / float64(len(bnet))
		for _, x := range bnet {
			v += (x - mean) * (x - mean)
		}
		sd := math.Sqrt(v / float64(len(bnet)-1))
		fmt.Printf("bucket-level: %d buckets, %d positive, mean %+.2fR sd %.2f, t = %.2f\n", len(bnet), pos, mean, sd, mean/(sd/math.Sqrt(float64(len(bnet)))))
		if k := min(6, len(bnet)); k > 0 {
			var s, best float64
			best = math.Inf(-1)
			for _, x := range bnet[:k] {
				s += x
				best = math.Max(best, x)
			}
			fmt.Printf("latest %d buckets: total %+.2fR, without the best %+.2fR\n", k, s, s-best)
		}
	}

	// Calendar periods, marked to market (includes trades entered earlier).
	fmt.Printf("\n--- calendar periods, R earned mark-to-market (open trades at period-end close, fees at entry) ---\n")
	periods := []struct {
		name   string
		p0, p1 time.Time
	}{
		{"Sep–Oct 2026 uptrend", date(2026, 9, 1), date(2026, 11, 1)},
		{"Nov 2026 drop", date(2026, 11, 1), now},
	}
	for _, pr := range periods {
		var tot float64
		var per []string
		for _, d := range data {
			r := periodR(d, pr.p0, pr.p1)
			tot += r
			per = append(per, fmt.Sprintf("%s %+.2f", d.short, r))
		}
		fmt.Printf("%-22s pooled %+7.2fR   %s\n", pr.name, tot, strings.Join(per, " "))
	}

	fmt.Printf("\n--- cmd/gate input (pooled) ---\n")
	fmt.Printf("gate --name \"htf-trend %s %.0fbp pooled\" --arm \"%s\" --buckets \"%s\"\n",
		tf, *feeBps, strings.Join(gateArm, ", "), strings.Join(gateBuckets, ", "))
}

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

func printWindow(data []symData, start, end time.Time, days int, showTrades bool) winStats {
	fmt.Printf("\n--- window %dd: %s → %s (trades by entry bar) ---\n", days, start.Format("2006-01-02"), end.Format("2006-01-02"))
	fmt.Printf("%-6s %4s %4s %9s %8s %5s %7s %7s %6s %6s %6s %6s %6s %7s %8s\n",
		"sym", "n", "open", "netR", "R/trade", "WR", "avgWin", "avgLos", "SD", "SE", "MDD", "trd/mo", "stop%", "medLev", "ex-best")
	row := func(name string, s winStats) {
		fmt.Printf("%-6s %4d %4d %+9.2f %+8.3f %4.0f%% %+7.2f %+7.2f %6.2f %6.3f %6.2f %6.2f %6.2f %4d/%-3d %+8.2f\n",
			name, s.n, s.open, s.net, s.rpt, s.wr, s.avgWin, s.avgLoss, s.sd, s.se, s.mdd, s.perMonth, s.medStopPct, s.medLev, s.minLev, s.netExBest)
	}
	for _, d := range data {
		ts := collect([]symData{d}, start, end, "")
		if len(ts) == 0 {
			fmt.Printf("%-6s %4d\n", d.short, 0)
			continue
		}
		row(d.short, stats(ts, days))
		if showTrades {
			for _, r := range ts {
				t := r.t
				fmt.Printf("        %s %-5s entry %.4f stop %.4f (%.2f%%, ≤%dx) → %s exit %.4f  %+.2fR gross %+.2fR%s  %d bars\n",
					t.EntryTime.Format("2006-01-02 15:04"), t.Side, t.Entry, t.InitStop, t.StopPct, t.MaxLev,
					t.ExitTime.Format("01-02 15:04"), t.Exit, t.NetR, t.GrossR, map[bool]string{true: " OPEN", false: ""}[t.Open], t.BarsHeld)
			}
		}
	}
	all := collect(data, start, end, "")
	st := stats(all, days)
	row("POOLED", st)
	fmt.Printf("       max concurrent open positions (pooled): %d\n", maxConcurrent(all))
	lo, hi := st.rpt-1.96*st.se, st.rpt+1.96*st.se
	need := 0.0
	if st.rpt > 0 {
		need = math.Pow(1.96*st.sd/st.rpt, 2)
	}
	fmt.Printf("       95%% CI on R/trade [%+.3f, %+.3f]; n needed for the CI to exclude 0 at this mean+SD ≈ %.0f\n", lo, hi, need)
	return st
}

// maxConcurrent is the most positions open at once across symbols (entry at
// the signal bar's close, exit at the exit bar's close).
func maxConcurrent(ts []tradeRef) int {
	type ev struct {
		t time.Time
		d int
	}
	var evs []ev
	for _, r := range ts {
		evs = append(evs, ev{r.t.EntryTime, 1}, ev{r.t.ExitTime, -1})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			return evs[i].d < evs[j].d // close before open at the same instant
		}
		return evs[i].t.Before(evs[j].t)
	})
	cur, mx := 0, 0
	for _, e := range evs {
		cur += e.d
		mx = max(mx, cur)
	}
	return mx
}

// periodR is the R one symbol earned inside [p0, p1), marked to market at
// the last closed bar of each boundary.
func periodR(d symData, p0, p1 time.Time) float64 {
	k0, k1 := lastClosed(d.cs, p0), lastClosed(d.cs, p1)
	var r float64
	for _, t := range d.trades {
		r += mtm(t, d.cs, k1) - mtm(t, d.cs, k0)
	}
	return r
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

func date(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC) }

func resolve(tok string) (market.Symbol, string) {
	sym, ok := market.Resolve(tok)
	if !ok {
		sym = market.Symbol(tok)
		if !strings.Contains(tok, "-USDT") && !strings.Contains(tok, "-USDC") {
			sym = market.Symbol(tok + "-USDT")
		}
	}
	short := market.Short(sym)
	if short == "" {
		short = string(sym)
	}
	return sym, short
}

// fetch pages all history from `from` to now, retrying BingX rate limits
// (code 100410), with an optional JSON cache.
func fetch(c *bingx.Client, sym market.Symbol, tf market.Timeframe, from time.Time, dir string, ttl time.Duration) ([]market.Candle, error) {
	var path string
	if dir != "" {
		path = filepath.Join(dir, fmt.Sprintf("%s_%s.json", strings.ReplaceAll(string(sym), "/", "_"), tf))
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < ttl {
			if b, err := os.ReadFile(path); err == nil {
				var cs []market.Candle
				if json.Unmarshal(b, &cs) == nil && len(cs) > 0 {
					return cs, nil
				}
			}
		}
	}
	var cs []market.Candle
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		cs, err = c.KlinesRange(context.Background(), sym, tf, from, time.Now().UTC())
		if err == nil || !strings.Contains(err.Error(), "100410") {
			break
		}
		wait := time.Duration(attempt+1) * 5 * time.Second
		log.Printf("%s: rate limited (100410), retry in %s", sym, wait)
		time.Sleep(wait)
	}
	if err != nil {
		return nil, err
	}
	if path != "" && len(cs) > 0 {
		if b, e := json.Marshal(cs); e == nil {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return cs, nil
}
