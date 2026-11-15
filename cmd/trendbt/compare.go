package main

// Paper-vs-backtest parity for the htf-trend executor
// (docs/htf_trend_executor_design.md §5 P5 and §6.1 item 6).
//
//   -compare-log PATH -since DATE   the REAL paper log: every backtest signal
//       since DATE must appear as a fire on the same signal bar with the same
//       entry, stop, exit and R (as scored by the live resolver); every
//       backtest signal without a fire must have a written reason in the
//       blocked log; every fire without a backtest signal is an extra. Any
//       unexplained row stops the forward test.
//
//   -replay-days N   the OFFLINE replay: emulate the executor bar by bar over
//       the last N days (its 499-bar trigger window, its 1439-bar kline
//       cache, autotrade.SlotBlocked — the function paperBlocked calls),
//       write the fires it would have logged, and score them with the live
//       resolver AND the /ops panel's path (ReplayWithCaps + ResolveLive +
//       BuildBookWeighted). Checks over hundreds of past trades in one run
//       what live paper would take months to show.
//
// What neither mode can see: tick timing and the 5-min grace window, the
// macro/earnings blackout (the replay fires through it; live paper records a
// "blackout" skip, which the compare reports as explained), restarts, kline
// fetch failures, and exchange kline revisions after the fact.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// Executor kline windows: Klines(limit) drops the forming bar, so a fetch of
// 500 (evalHTFTrend) or 1440 (the trail kline cache) yields one fewer closed
// bar.
const (
	execTriggerBars = 499
	execCacheBars   = 1439
)

// strategyForTF is the strategy name a TF's rule runs under.
func strategyForTF(tf market.Timeframe) string {
	for _, s := range autotrade.KnownStrategies {
		if autotrade.TrailStrategyTF(s) == string(tf) {
			return s
		}
	}
	return ""
}

type cmpRow struct {
	sym    string
	bar    time.Time // signal bar close
	kind   string    // "match" | "mismatch" | "missing" | "extra"
	detail string
	ok     bool // matched, or a missing signal with a written reason
}

// signalBar is the fire's signal-bar close: BarTime, or for an older row the
// newest bar closed at its Time.
func signalBar(f autotrade.PaperFire, cs []market.Candle) time.Time {
	if !f.BarTime.IsZero() {
		return f.BarTime
	}
	i := sort.Search(len(cs), func(k int) bool { return cs[k].CloseTime.After(f.Time) }) - 1
	if i < 0 {
		return time.Time{}
	}
	return cs[i].CloseTime
}

func relDiff(a, b float64) float64 {
	if a == b {
		return 0
	}
	return math.Abs(a-b) / math.Max(math.Abs(a), math.Abs(b))
}

// compareTrade checks one fire against the backtest trade on the same signal
// bar, scoring the fire with oc (the caller's resolver). Entry and exit must
// agree to 1e-9 relative; the stop to 1e-6 (the live trigger seeds ATR 499
// bars back, trendbt at the start of history: ~2e-11 apart, see
// htfTrendLiveBars).
func compareTrade(t autostrat.TrendTrade, f autotrade.PaperFire, oc autotrade.Outcome) string {
	var bad []string
	if f.Side != t.Side {
		bad = append(bad, fmt.Sprintf("side %s vs %s", f.Side, t.Side))
	}
	if relDiff(f.Entry, t.Entry) > 1e-9 {
		bad = append(bad, fmt.Sprintf("entry %.6g vs %.6g", f.Entry, t.Entry))
	}
	if relDiff(f.Stop, t.InitStop) > 1e-6 {
		bad = append(bad, fmt.Sprintf("stop %.6g vs %.6g", f.Stop, t.InitStop))
	}
	switch {
	case t.Open:
		if oc.Status != autotrade.OutOpen {
			bad = append(bad, fmt.Sprintf("status %s vs open", oc.Status))
		}
	case oc.Status != autotrade.OutStop:
		bad = append(bad, fmt.Sprintf("status %s vs stop", oc.Status))
	default:
		if relDiff(oc.ExitPrice, t.Exit) > 1e-9 {
			bad = append(bad, fmt.Sprintf("exit %.6g vs %.6g", oc.ExitPrice, t.Exit))
		}
		if !oc.ExitAt.Equal(t.ExitTime) {
			bad = append(bad, fmt.Sprintf("exit at %s vs %s", oc.ExitAt.UTC().Format("01-02 15:04"), t.ExitTime.UTC().Format("01-02 15:04")))
		}
		if math.Abs(oc.NetR-t.GrossR) > 1e-6 {
			bad = append(bad, fmt.Sprintf("R %+.6f vs %+.6f", oc.NetR, t.GrossR))
		}
	}
	return strings.Join(bad, "; ")
}

// compareSymbol matches one symbol's backtest trades (entry bar after since)
// against its fires (same strategy, signal bar after since).
func compareSymbol(sym string, cs []market.Candle, trades []autostrat.TrendTrade, fires []autotrade.PaperFire,
	since time.Time, resolve func(autotrade.PaperFire) autotrade.Outcome, blockedWhy map[time.Time]string) []cmpRow {
	byBar := map[time.Time]autotrade.PaperFire{}
	for _, f := range fires {
		if b := signalBar(f, cs); b.After(since) {
			byBar[b] = f
		}
	}
	var out []cmpRow
	seen := map[time.Time]bool{}
	for _, t := range trades {
		if !t.EntryTime.After(since) {
			continue
		}
		seen[t.EntryTime] = true
		f, ok := byBar[t.EntryTime]
		if !ok {
			why, has := blockedWhy[t.EntryTime]
			if !has {
				why = "no fire and no blocked-log record"
			}
			out = append(out, cmpRow{sym, t.EntryTime, "missing", why, has})
			continue
		}
		if d := compareTrade(t, f, resolve(f)); d != "" {
			out = append(out, cmpRow{sym, t.EntryTime, "mismatch", d, false})
		} else {
			out = append(out, cmpRow{sym, t.EntryTime, "match", "", true})
		}
	}
	for b, f := range byBar {
		if !seen[b] {
			out = append(out, cmpRow{sym, b, "extra", fmt.Sprintf("%s @%.6g: fired, backtest took no signal on this bar", f.Side, f.Entry), false})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].bar.Before(out[j].bar) })
	return out
}

func printCompare(title string, rows []cmpRow) (matched, total, unexplained int) {
	fmt.Printf("\n--- %s ---\n", title)
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.kind]++
		if r.kind == "match" {
			matched++
		} else {
			fmt.Printf("  %-5s %s %-8s %s\n", r.sym, r.bar.UTC().Format("2006-01-02 15:04"), strings.ToUpper(r.kind), r.detail)
		}
		if !r.ok {
			unexplained++
		}
	}
	total = len(rows)
	rate := 0.0
	if total > 0 {
		rate = 100 * float64(matched) / float64(total)
	}
	fmt.Printf("  %d rows: %d match, %d mismatch, %d missing, %d extra — match rate %.1f%%, UNEXPLAINED %d\n",
		total, counts["match"], counts["mismatch"], counts["missing"], counts["extra"], rate, unexplained)
	return
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if json.Unmarshal([]byte(line), &v) == nil {
			out = append(out, v)
		}
	}
	return out, sc.Err()
}

// runCompareLog is -compare-log: the real paper log against the backtest.
func runCompareLog(data []symData, tf market.Timeframe, logPath, blockedPath string, since time.Time) int {
	strat := strategyForTF(tf)
	fires, err := readJSONL[autotrade.PaperFire](logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compare-log: %v\n", err)
		return 2
	}
	var blocked []autotrade.BlockedCandidate
	if blockedPath != "" {
		if blocked, err = readJSONL[autotrade.BlockedCandidate](blockedPath); err != nil {
			fmt.Fprintf(os.Stderr, "blocked-log: %v (missing signals will read as unexplained)\n", err)
		}
	}
	var rows []cmpRow
	for _, d := range data {
		var mine []autotrade.PaperFire
		for _, f := range fires {
			if f.Symbol == d.short && f.Strategy == strat {
				mine = append(mine, f)
			}
		}
		why := map[time.Time]string{}
		for _, b := range blocked {
			if b.Fire.Symbol == d.short && b.Fire.Strategy == strat {
				why[b.BarTime] = b.Reason
			}
		}
		// The panel's view: full history and the live resolver.
		resolve := func(f autotrade.PaperFire) autotrade.Outcome { return autotrade.Resolve(f, d.cs, 6) }
		rows = append(rows, compareSymbol(d.short, d.cs, d.trades, mine, since, resolve, why)...)
	}
	_, _, unexplained := printCompare(fmt.Sprintf("compare-log %s · %s since %s", logPath, strat, since.Format("2006-01-02")), rows)
	if unexplained > 0 {
		return 1
	}
	return 0
}

// emulateExecutor writes the fires the paper executor would have logged for
// one symbol, bar by bar from index start: the trigger on its 499-bar window,
// one position per rule via autotrade.SlotBlocked on its 1439-bar cache, each
// fire a minute after its signal bar's close (inside the grace window) with
// the rule's sizing fields as fireRecord writes them.
func emulateExecutor(short string, cs []market.Candle, tf market.Timeframe, start int, riskPct float64) []autotrade.PaperFire {
	strat := strategyForTF(tf)
	rule := autotrade.Rule{Symbol: short, Strategy: strat, TF: string(tf), Side: "auto", RiskPct: riskPct}
	dur := market.BarDuration(tf)
	var out []autotrade.PaperFire
	for k := max(start, 1); k < len(cs); k++ {
		trig := autostrat.HTFTrendOnWindow(cs[max(0, k-execTriggerBars+1):k+1], rule)
		if !trig.Fire {
			continue
		}
		if n := len(out); n > 0 && autotrade.SlotBlocked(out[n-1], cs[max(0, k-execCacheBars+1):k+1], trig.BarTime, rule.CooldownBars, dur) {
			continue
		}
		out = append(out, autotrade.PaperFire{
			Time: trig.BarTime.Add(time.Minute), BarTime: trig.BarTime, Symbol: short, TF: string(tf), Strategy: strat,
			Side: trig.Side, Market: trig.Market, Entry: trig.Entry, Stop: trig.Stop, TP: trig.TP,
			TrailM: trig.TrailM, CooldownBars: rule.CooldownBars, RiskPct: riskPct, Why: trig.Why,
		})
	}
	return out
}

// runReplay is -replay-days: the offline replay of §6.1 item 6.
func runReplay(data []symData, tf market.Timeframe, days int, riskPct, bookPct float64, out string) int {
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -days)
	// Burn-in: emulation starts this much earlier so that the executor and
	// the backtest are both flat at some point before the window opens (a
	// trend trade can outlast any short burn-in).
	burn := now.AddDate(0, 0, -days-max(days, 120))
	strat := strategyForTF(tf)
	var all []autotrade.PaperFire
	var execRows, panelRows []cmpRow
	cfg := autotrade.Config{RiskPerTradePct: bookPct, DailyLossHaltR: 0,
		StrategyCaps: map[string]autotrade.StrategyCap{"htf-trend": {Strategies: []string{"htf-trend", "htf-trend-1d"}}}}
	for _, d := range data {
		start := sort.Search(len(d.cs), func(k int) bool { return d.cs[k].CloseTime.After(burn) })
		fires := emulateExecutor(d.short, d.cs, tf, start, riskPct)
		all = append(all, fires...)
		// Executor view, as at the newest bar: its 1439-bar cache.
		execCS := d.cs[max(0, len(d.cs)-execCacheBars):]
		resolve := func(f autotrade.PaperFire) autotrade.Outcome { return autotrade.Resolve(f, execCS, 6) }
		execRows = append(execRows, compareSymbol(d.short, d.cs, d.trades, fires, since, resolve, nil)...)
	}
	// Panel view: every synthetic fire through the panel's scoring path, all
	// symbols in one ReplayWithCaps exactly as /ops/autotrade runs it (full
	// history from klinesSince, live price = the last close).
	bySym := map[string]symData{}
	for _, d := range data {
		bySym[d.short] = d
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	panelResolve := func(f autotrade.PaperFire) autotrade.Outcome {
		cs := bySym[f.Symbol].cs
		return autotrade.ResolveLive(f, cs, 6, cs[len(cs)-1].Close)
	}
	positions, blocked := autotrade.ReplayWithCaps(all, cfg, 6, 6, time.Hour, panelResolve)
	posBy := map[string]autotrade.Position{}
	absorbed := 0
	for _, p := range positions {
		posBy[p.Fire.Symbol+"|"+p.Fire.BarTime.String()] = p
		absorbed += p.Absorbed
	}
	for _, d := range data {
		var fs []autotrade.PaperFire
		for _, p := range positions {
			if p.Fire.Symbol == d.short {
				fs = append(fs, p.Fire)
			}
		}
		resolve := func(f autotrade.PaperFire) autotrade.Outcome { return posBy[f.Symbol+"|"+f.BarTime.String()].Outcome }
		panelRows = append(panelRows, compareSymbol(d.short, d.cs, d.trades, fs, since, resolve, nil)...)
	}
	fmt.Printf("\n=== offline replay · %s · last %dd (burn-in from %s) · %d synthetic fires, %d symbols ===\n",
		strat, days, burn.Format("2006-01-02"), len(all), len(data))
	em, et, eu := printCompare("executor view: emulated fires (SlotBlocked + 499-bar trigger) scored by autotrade.Resolve on the 1439-bar cache", execRows)
	pm, pt, pu := printCompare("panel view: ReplayWithCaps + ResolveLive (full history, live = last close)", panelRows)
	fmt.Printf("  panel replay: %d positions, %d absorbed re-fires, %d caps-blocked\n", len(positions), absorbed, len(blocked))

	// The breaker/book view: open trail positions per BuildBookWeighted
	// against the backtest's still-open trades.
	newest := make([]autotrade.PaperFire, len(all))
	for i, f := range all {
		newest[len(all)-1-i] = f
	}
	bk, _, _ := autotrade.BuildBookWeighted(newest, func(sym, _ string) []market.Candle {
		cs := bySym[sym].cs
		return cs[max(0, len(cs)-execCacheBars):]
	}, now, 0, bookPct)
	btOpen := 0
	for _, d := range data {
		for _, t := range d.trades {
			if t.Open {
				btOpen++
			}
		}
	}
	bookOK := bk.OpenCount == btOpen
	fmt.Printf("  book: BuildBookWeighted open %d vs backtest open %d (%s); today's weighted R %+.4f\n",
		bk.OpenCount, btOpen, map[bool]string{true: "match", false: "MISMATCH"}[bookOK], bk.RealizedRToday)
	fmt.Printf("REPLAY %s: executor %d/%d, panel %d/%d matched\n", strat, em, et, pm, pt)

	if out != "" {
		f, err := os.Create(out)
		if err == nil {
			w := bufio.NewWriter(f)
			for _, pf := range all {
				b, _ := json.Marshal(pf)
				_, _ = w.Write(append(b, '\n'))
			}
			_ = w.Flush()
			_ = f.Close()
			fmt.Printf("  synthetic paper log written to %s (readable by -compare-log)\n", out)
		}
	}
	if eu+pu > 0 || absorbed > 0 || !bookOK {
		return 1
	}
	return 0
}
