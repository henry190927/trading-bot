// Command xsmombt backtests cross-sectional momentum (weekly rotation) over
// the paper book's 8 symbols — docs/xsmom_design.md. BACKTEST ONLY.
//
// One continuous simulation over the common 1d timeline. A "trade" is one
// leg entry → exit (initial stop = 1R, equal risk per leg) and is attributed
// to the window containing its ENTRY bar, as cmd/trendbt does.
//
// Public klines / funding need no API key; this command never reads .env.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/shipgate"
)

type arm struct {
	name string
	p    Params
}

// The pre-registered arms (design §4). Nothing else is run.
var arms = []arm{
	{"A", Params{L: 30, K: 2, ATRPeriod: 20, StopATR: 3}},
	{"B", Params{L: 14, K: 2, ATRPeriod: 20, StopATR: 3}},
	{"C", Params{L: 30, K: 3, ATRPeriod: 20, StopATR: 3}},
}

func main() {
	symbols := flag.String("symbols", "BTC,ETH,SOL,XRP,SUI,HYPE,NEAR,LINK", "the paper book's 8 (pre-registered)")
	cacheDir := flag.String("cache", "", "kline/funding JSON cache directory")
	cacheTTL := flag.Duration("cache-ttl", 12*time.Hour, "max cache age")
	since := flag.String("since", "2019-01-01", "earliest date to page back to")
	trades := flag.Bool("trades", false, "print arm A's legs at 10 bp")
	flag.Parse()

	from, err := time.Parse("2006-01-02", *since)
	if err != nil {
		log.Fatalf("-since: %v", err)
	}
	client := bingx.New("", "")
	now := time.Now().UTC()

	var shorts []string
	var syms []market.Symbol
	d1 := map[string][]market.Candle{}
	h4 := map[string][]market.Candle{}
	for _, tok := range strings.Split(*symbols, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		sym, short := resolve(tok)
		a, err := fetch(client, sym, market.TF1d, from, *cacheDir, *cacheTTL)
		if err != nil || len(a) < 60 {
			log.Fatalf("%s 1d: %v (len %d)", short, err, len(a))
		}
		b, err := fetch(client, sym, market.TF4h, from, *cacheDir, *cacheTTL)
		if err != nil || len(b) < 60 {
			log.Fatalf("%s 4h: %v (len %d)", short, err, len(b))
		}
		shorts, syms = append(shorts, short), append(syms, sym)
		d1[short], h4[short] = a, b
	}
	fmt.Printf("=== xsmom weekly rotation (docs/xsmom_design.md) · now %s ===\n", now.Format("2006-01-02 15:04"))
	for _, s := range shorts {
		a, b := d1[s], h4[s]
		fmt.Printf("    %-5s 1d %s → %s (%d) | 4h %s → %s (%d)\n", s,
			a[0].OpenTime.Format("2006-01-02"), a[len(a)-1].CloseTime.Format("2006-01-02"), len(a),
			b[0].OpenTime.Format("2006-01-02"), b[len(b)-1].CloseTime.Format("2006-01-02"), len(b))
		if g := gaps(a); g > 0 {
			fmt.Printf("          !! %d missing daily bars\n", g)
		}
	}

	// htf-trend paper book on the same data (10 bp, defaults).
	hp := autostrat.DefaultHTFTrendParams()
	wu := max(hp.EntryN, hp.ATRPeriod) + 1
	var bookH4, bookT1 []symData
	for _, s := range shorts {
		cs := h4[s]
		bookH4 = append(bookH4, symData{s, cs, autostrat.SimulateHTFTrend(cs, indicator.ATR(cs, hp.ATRPeriod), wu, true, true, hp, 10)})
		cs = d1[s]
		bookT1 = append(bookT1, symData{s, cs, autostrat.SimulateHTFTrendTouch(cs, indicator.ATR(cs, hp.ATRPeriod), wu, true, true, hp, 10)})
	}

	type runRes struct {
		data  []symData
		legs  [][]Leg
		books int
	}
	res := map[string]map[float64]runRes{}
	for _, a := range arms {
		res[a.name] = map[float64]runRes{}
		var ss []series
		for _, s := range shorts {
			ss = append(ss, newSeries(s, d1[s], a.p.ATRPeriod))
		}
		for _, fee := range []float64{10, 6, 0} {
			legs, books := simulate(ss, a.p, sundayRebal, fee)
			var data []symData
			for i, s := range shorts {
				data = append(data, symData{s, d1[s], legsToTrades(legs[i])})
			}
			res[a.name][fee] = runRes{data, legs, books}
		}
	}

	for _, fee := range []float64{10, 6, 0} {
		tag := map[float64]string{10: "(VERDICT)", 6: "(reference)", 0: "(GROSS)"}[fee]
		fmt.Printf("\n\n################ fee %.0f bp %s ################\n", fee, tag)
		fmt.Printf("\n--- full history, pooled ---\n")
		fmt.Printf("%-3s %5s %9s %8s %5s %13s %6s %6s %7s %7s %8s %8s %8s %7s %9s %s\n", "arm", "n", "netR", "R/trade", "WR", "avgW/avgL", "SD", "SE", "trd/day", "MDD", "MDDmtm", "dMean", "dSD", "60d t", "drop-best", "span")
		for _, a := range arms {
			r := res[a.name][fee]
			st, span := fullStats(r.data, now)
			first := firstEntry(r.data)
			b := bucketNet(r.data, now, 60, first)
			dr := dailyR(r.data, first, now)
			dm, dsd := meanSD(dr)
			fmt.Printf("%-3s %5d %+9.2f %+8.4f %4.0f%% %+6.2f/%+6.2f %6.2f %6.4f %7.3f %7.2f %8.2f %+8.4f %8.3f %+7.2f %+9.2f %.0fd from %s (books %d)\n",
				a.name, st.n, st.net, st.rpt, st.wr, st.avgWin, st.avgLoss, st.sd, st.se, float64(st.n)/span, st.mdd, mtmMDD(dr), dm, dsd, bucketT(b), dropBest(b), span, first.Format("2006-01-02"), r.books)
		}

		fmt.Printf("\n--- per symbol netR (n) · long/short · exit reasons ---\n")
		for _, a := range arms {
			r := res[a.name][fee]
			var parts []string
			var lN, sN, stopN, rebN, openN int
			var lR, sR float64
			for i, ls := range r.legs {
				var net float64
				for _, l := range ls {
					net += l.NetR
					if l.Side == "long" {
						lN++
						lR += l.NetR
					} else {
						sN++
						sR += l.NetR
					}
					switch l.Reason {
					case "stop":
						stopN++
					case "rebal":
						rebN++
					default:
						openN++
					}
				}
				parts = append(parts, fmt.Sprintf("%s %+.1f(%d)", shorts[i], net, len(ls)))
			}
			fmt.Printf("%-3s %s\n    long %+.2f (%d, %+.3f/t) short %+.2f (%d, %+.3f/t) · exits stop %d rebal %d open %d\n",
				a.name, strings.Join(parts, "  "), lR, lN, lR/float64(max(lN, 1)), sR, sN, sR/float64(max(sN, 1)), stopN, rebN, openN)
		}

		for _, a := range arms {
			fmt.Printf("\n--- %s (L%d k%d): nested 60/90/120 + six 30d buckets, shipgate.Default() (no baseline) ---\n", a.name, a.p.L, a.p.K)
			printGateTable(res[a.name][fee].data, now)
		}

		if fee != 10 {
			continue
		}
		fmt.Printf("\n--- daily R correlation vs htf-trend paper book (Pearson; full common span | last 360d) ---\n")
		corrFrom := date(2022, 10, 15)
		if f := firstEntry(res["A"][10].data); f.After(corrFrom) {
			corrFrom = f
		}
		dH4, dT1 := dailyR(bookH4, corrFrom, now), dailyR(bookT1, corrFrom, now)
		book := addSeries(dH4, dT1)
		fmt.Printf("    from %s, %d days\n", corrFrom.Format("2006-01-02"), len(book))
		fmt.Printf("%-5s %16s %16s %16s\n", "arm", "vs H4(4h close)", "vs T1(1d touch)", "vs BOOK")
		for _, a := range arms {
			s := dailyR(res[a.name][10].data, corrFrom, now)
			fmt.Printf("%-5s", a.name)
			for _, r := range [][]float64{dH4, dT1, book} {
				fmt.Printf("  %+7.3f|%+7.3f", pearson(s, r), pearson(tail(s, 360), tail(r, 360)))
			}
			fmt.Println()
		}
		bm, bsd := meanSD(book)
		as := dailyR(res["A"][10].data, corrFrom, now)
		am, asd := meanSD(as)
		comb := addSeries(book, as)
		cm, csd := meanSD(comb)
		fmt.Printf("    same span: BOOK daily mean %+.4f SD %.3f MDD %.1f | A mean %+.4f SD %.3f MDD %.1f | BOOK+A mean %+.4f SD %.3f MDD %.1f\n",
			bm, bsd, mtmMDD(book), am, asd, mtmMDD(as), cm, csd, mtmMDD(comb))
		hs, _ := fullStats(bookH4, now)
		ts, _ := fullStats(bookT1, now)
		fmt.Printf("    (book check: H4 n %d R/t %+.4f · T1 n %d R/t %+.4f)\n", hs.n, hs.rpt, ts.n, ts.rpt)

		fundingEstimate(client, syms, shorts, res["A"][10].legs, *cacheDir, *cacheTTL)

		if *trades {
			fmt.Printf("\n--- arm A legs, 10 bp ---\n")
			var all []Leg
			for _, ls := range res["A"][10].legs {
				all = append(all, ls...)
			}
			sort.Slice(all, func(i, j int) bool { return all[i].EntryTime.Before(all[j].EntryTime) })
			for _, l := range all {
				fmt.Printf("%s %-5s %-5s %.4f stop %.4f (%.1f%%) → %s %.4f %-5s %+.2fR\n", l.EntryTime.Format("2006-01-02"), shorts[l.Sym], l.Side,
					l.Entry, l.InitStop, l.StopPct, l.ExitTime.Format("2006-01-02"), l.Exit, l.Reason, l.NetR)
			}
		}

		// Decisions (§6), arm A at 10 bp.
		fmt.Printf("\n--- decision (§6), arm A at 10 bp ---\n")
		A, B := res["A"][10], res["B"][10]
		va := shipgate.Evaluate(gateArmOf(A.data, "", now), nil, shipgate.Default())
		vb := shipgate.Evaluate(gateArmOf(B.data, "", now), nil, shipgate.Default())
		sa, _ := fullStats(A.data, now)
		sb, _ := fullStats(B.data, now)
		dropA := dropBest(bucketNet(A.data, now, 60, firstEntry(A.data)))
		corr := pearson(as, book)
		c2 := sa.net > 0 && dropA > 0
		c3 := sb.rpt > 0 && vb.Result != shipgate.Fail
		verdict := "UNDECIDED"
		switch {
		case va.Result == shipgate.Fail || !c2:
			verdict = "REJECTED"
		case corr > 0.7:
			verdict = "REJECTED (duplicate of the htf-trend book)"
		case va.Result == shipgate.Pass && c3 && corr < 0.4:
			verdict = "PASS (paper-eligible; wiring is a separate decision)"
		case va.Result == shipgate.Pass && !c3:
			verdict = "UNDECIDED (parameter-fragile: neighbour L14 fails)"
		case va.Result == shipgate.Pass:
			verdict = "UNDECIDED (overlapping with the book, corr 0.4–0.7)"
		}
		fmt.Printf("1 gate A: %s %s\n", va.Result, strings.Join(va.Reasons, "; "))
		fmt.Printf("2 full netR %+.2f, 60d drop-best %+.2f → %v\n", sa.net, dropA, c2)
		fmt.Printf("3 neighbour B: R/trade %+.4f gate %s %s → %v\n", sb.rpt, vb.Result, strings.Join(vb.Reasons, "; "), c3)
		fmt.Printf("4 corr(A, BOOK) %+.3f\n", corr)
		fmt.Printf("=> %s\n", verdict)
	}
}

// fundingEstimate: design §3.6 — informational, NOT in any R above.
func fundingEstimate(c *bingx.Client, syms []market.Symbol, shorts []string, legs [][]Leg, dir string, ttl time.Duration) {
	fmt.Printf("\n--- funding estimate (NOT modelled; legs inside the funding history only) ---\n")
	var totR float64
	var totN int
	for i, sym := range syms {
		fh, err := fetchFunding(c, sym, dir, ttl)
		if err != nil || len(fh) == 0 {
			fmt.Printf("    %-5s funding unavailable: %v\n", shorts[i], err)
			continue
		}
		var r float64
		n := 0
		for _, l := range legs[i] {
			if l.EntryTime.Before(fh[0].Time) {
				continue
			}
			var f float64
			for _, p := range fh {
				if p.Time.After(l.EntryTime) && !p.Time.After(l.ExitTime) {
					f += -sideDir(l.Side) * p.Rate * l.Entry / l.Risk // a long pays a positive rate
				}
			}
			r += f
			n++
		}
		totR += r
		totN += n
		fmt.Printf("    %-5s since %s: %d legs, funding %+.3fR (%+.4f/leg)\n", shorts[i], fh[0].Time.Format("2006-01-02"), n, r, r/float64(max(n, 1)))
	}
	fmt.Printf("    total %d legs, %+.3fR, %+.4f R/leg (sign: + = received)\n", totN, totR, totR/float64(max(totN, 1)))
}

func fetchFunding(c *bingx.Client, sym market.Symbol, dir string, ttl time.Duration) ([]bingx.FundingPoint, error) {
	var path string
	if dir != "" {
		path = filepath.Join(dir, fmt.Sprintf("funding_%s.json", sym))
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < ttl {
			if b, err := os.ReadFile(path); err == nil {
				var fh []bingx.FundingPoint
				if json.Unmarshal(b, &fh) == nil && len(fh) > 0 {
					return fh, nil
				}
			}
		}
	}
	var fh []bingx.FundingPoint
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		fh, err = c.FundingRateHistory(context.Background(), sym, 1000)
		if err == nil || !strings.Contains(err.Error(), "100410") {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
	}
	if err == nil && path != "" && len(fh) > 0 {
		if b, e := json.Marshal(fh); e == nil {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return fh, err
}

func gaps(cs []market.Candle) int {
	g := 0
	for i := 1; i < len(cs); i++ {
		if cs[i].OpenTime.Sub(cs[i-1].OpenTime) != 24*time.Hour {
			g++
		}
	}
	return g
}

func firstEntry(data []symData) time.Time {
	f := time.Now().UTC()
	for _, d := range data {
		if len(d.trades) > 0 && d.trades[0].EntryTime.Before(f) {
			f = d.trades[0].EntryTime
		}
	}
	return f
}

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

// fetch pages history from `from` to now in chunks of 1000 bars, each chunk
// retried on BingX rate limits (code 100410) with back-off; JSON cache.
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
	dur := map[market.Timeframe]time.Duration{market.TF1d: 24 * time.Hour, market.TF4h: 4 * time.Hour}[tf]
	if dur == 0 {
		return nil, fmt.Errorf("tf %s", tf)
	}
	end := time.Now().UTC()
	var all []market.Candle
	for st := from; st.Before(end); st = st.Add(1000 * dur) {
		en := st.Add(1000 * dur)
		if en.After(end) {
			en = end
		}
		var cs []market.Candle
		var err error
		for attempt := 0; attempt < 8; attempt++ {
			cs, err = c.KlinesRange(context.Background(), sym, tf, st, en)
			if err == nil {
				break
			}
			wait := time.Duration(attempt+1) * 5 * time.Second
			if !strings.Contains(err.Error(), "100410") && attempt >= 2 {
				break
			}
			log.Printf("%s %s chunk %s: %v — retry in %s", sym, tf, st.Format("2006-01-02"), err, wait)
			time.Sleep(wait)
		}
		if err != nil {
			return nil, err
		}
		all = append(all, cs...)
		time.Sleep(200 * time.Millisecond)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].OpenTime.Before(all[j].OpenTime) })
	out := all[:0]
	var prev int64 = -1
	for _, cnd := range all {
		if k := cnd.OpenTime.UnixMilli(); k != prev {
			out = append(out, cnd)
			prev = k
		}
	}
	if path != "" && len(out) > 0 {
		if b, e := json.Marshal(out); e == nil {
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return out, nil
}

func date(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC) }
