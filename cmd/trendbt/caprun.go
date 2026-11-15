package main

// -cap-run: the pre-registered same-side cap test (docs/htf_trend_design.md
// §7). The 16-rule group (symbols × {4h, 1d}) on one clock; a signal is
// refused when the group already holds `cap` positions on its side, and the
// refused rule stays flat and keeps scanning. cap 0 must reproduce each rule's
// SimulateHTFTrend trades exactly, or the run aborts.

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

type capRule struct {
	name  string // "BTC 4h"
	cs    []market.Candle
	atr   []float64
	first int // first signal bar
}

type capCand struct {
	rule int
	k    int
	t    time.Time
	trig autostrat.Trigger
}

// simulateCapped runs the group with a same-side cap (0 = off) and returns
// the trades per rule plus how many signals the cap refused.
func simulateCapped(rules []capRule, p autostrat.HTFTrendParams, feeBps float64, capN int) ([][]autostrat.TrendTrade, int) {
	var cands []capCand
	for ri, r := range rules {
		for k := max(r.first, p.EntryN); k < len(r.cs); k++ {
			if tr := autostrat.HTFTrendTrigger(r.cs, r.atr[k], k, true, true, p); tr.Fire {
				cands = append(cands, capCand{ri, k, r.cs[k].CloseTime, tr})
			}
		}
	}
	// Time order; ties in rule order (the executor's `for i := range cfg.Rules`).
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].t.Equal(cands[j].t) {
			return cands[i].t.Before(cands[j].t)
		}
		return cands[i].rule < cands[j].rule
	})
	out := make([][]autostrat.TrendTrade, len(rules))
	busyUntil := make([]time.Time, len(rules)) // exit-bar close of the rule's last trade
	type leg struct {
		side        string
		entry, exit time.Time
	}
	var legs []leg
	refused := 0
	for _, c := range cands {
		if !busyUntil[c.rule].IsZero() && !c.t.After(busyUntil[c.rule]) {
			continue // in a position, or on its exit bar (next signal is bar >= exit+1)
		}
		if capN > 0 {
			n := 0
			for _, l := range legs {
				if l.side == c.trig.Side && !l.entry.After(c.t) && c.t.Before(l.exit) {
					n++
				}
			}
			if n >= capN {
				refused++
				continue
			}
		}
		r := rules[c.rule]
		t := capTrade(r.cs, c.k, c.trig, p.TrailM, feeBps)
		out[c.rule] = append(out[c.rule], t)
		busyUntil[c.rule] = t.ExitTime
		exit := t.ExitTime
		if t.Open {
			exit = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC) // still on the book
		}
		legs = append(legs, leg{t.Side, t.EntryTime, exit})
	}
	return out, refused
}

// capTrade builds the TrendTrade SimulateHTFTrend would for this signal,
// from the shared walk and the fee conversion autotrade.WithFee uses.
func capTrade(cs []market.Candle, i int, tr autostrat.Trigger, m int, feeBps float64) autostrat.TrendTrade {
	risk := math.Abs(tr.Entry - tr.Stop)
	f := autotrade.PaperFire{Side: tr.Side, Entry: tr.Entry, Stop: tr.Stop}
	feeR := -autotrade.WithFee(f, autotrade.Outcome{Status: autotrade.OutStop, FillPrice: tr.Entry}, feeBps).NetR
	w := autotrade.WalkTrail(cs, i, tr.Side, tr.Stop, m)
	dir := 1.0
	if tr.Side == "short" {
		dir = -1
	}
	g := dir * (w.Exit - tr.Entry) / risk
	return autostrat.TrendTrade{
		Side: tr.Side, EntryIdx: i, EntryTime: cs[i].CloseTime, Entry: tr.Entry, InitStop: tr.Stop, Risk: risk,
		ExitIdx: w.ExitIdx, ExitTime: cs[w.ExitIdx].CloseTime, Exit: w.Exit, Open: w.Open,
		GrossR: g, FeeR: feeR, NetR: g - feeR, StopPct: risk / tr.Entry * 100,
		MaxLev: autotrade.MaxLevForStop(risk / tr.Entry), BarsHeld: w.ExitIdx - i,
	}
}

type capResult struct {
	capN    int
	refused int
	all     winStats
	last360 winStats
	bucket  []winStats // disjoint 60d, newest first
}

func runCapRun(client fetcher, symbols []string, feeBps float64, start time.Time, caps []int, bucketDays int) {
	p := autostrat.DefaultHTFTrendParams()
	wu := max(p.EntryN, p.ATRPeriod) + 1
	var rules []capRule
	for _, tf := range []market.Timeframe{market.TF4h, market.TF1d} {
		for _, tok := range symbols {
			sym, short := resolve(tok)
			cs, err := client(sym, tf)
			if err != nil || len(cs) < wu+5 {
				log.Printf("%s %s: klines %v (len %d) — skipped", short, tf, err, len(cs))
				continue
			}
			first := max(wu, sort.Search(len(cs), func(k int) bool { return !cs[k].CloseTime.Before(start) }))
			rules = append(rules, capRule{short + " " + string(tf), cs, indicator.ATR(cs, p.ATRPeriod), first})
		}
	}
	// Model check: cap off must be SimulateHTFTrend, rule by rule, exactly.
	off, _ := simulateCapped(rules, p, feeBps, 0)
	for ri, r := range rules {
		want := autostrat.SimulateHTFTrend(r.cs, r.atr, r.first, true, true, p, feeBps)
		if len(want) != len(off[ri]) {
			log.Fatalf("model check: %s has %d trades capped-off vs %d in SimulateHTFTrend", r.name, len(off[ri]), len(want))
		}
		for k := range want {
			if want[k] != off[ri][k] {
				log.Fatalf("model check: %s trade %d differs:\n %+v\n %+v", r.name, k, off[ri][k], want[k])
			}
		}
	}
	now := time.Now().UTC()
	collectAll := func(per [][]autostrat.TrendTrade, from, to time.Time) []tradeRef {
		var out []tradeRef
		for ri, ts := range per {
			for _, t := range ts {
				if t.EntryTime.After(from) && !t.EntryTime.After(to) {
					out = append(out, tradeRef{rules[ri].name, t})
				}
			}
		}
		return out
	}
	totalDays := int(now.Sub(start).Hours()/24) + 1
	var res []capResult
	for _, c := range caps {
		per, refused := simulateCapped(rules, p, feeBps, c)
		cr := capResult{capN: c, refused: refused,
			all:     stats(collectAll(per, start.Add(-time.Second), now), totalDays),
			last360: stats(collectAll(per, now.AddDate(0, 0, -360), now), 360)}
		for off := 0; ; off += bucketDays {
			end := now.AddDate(0, 0, -off)
			if end.Before(start) {
				break
			}
			cr.bucket = append(cr.bucket, stats(collectAll(per, end.AddDate(0, 0, -bucketDays), end), bucketDays))
		}
		res = append(res, cr)
	}

	fmt.Printf("=== same-side cap run (pre-registered, docs/htf_trend_design.md §7) · %d rules · %.0fbp · signals from %s ===\n",
		len(rules), feeBps, start.Format("2006-01-02"))
	fmt.Printf("rules (tie order): %s\n", func() string {
		var n []string
		for _, r := range rules {
			n = append(n, r.name)
		}
		return strings.Join(n, ", ")
	}())
	fmt.Println("model check: cap off == SimulateHTFTrend for every rule ✓")
	fmt.Printf("\n%-5s %8s %5s %9s %8s %6s %6s | %-28s\n", "cap", "refused", "n", "netR", "R/trade", "SE", "MDD", "latest 360d: n netR R/t MDD")
	for _, r := range res {
		fmt.Printf("%-5s %8d %5d %+9.2f %+8.4f %6.4f %6.2f | %4d %+8.2f %+7.4f %6.2f\n", capLabel(r.capN), r.refused,
			r.all.n, r.all.net, r.all.rpt, r.all.se, r.all.mdd, r.last360.n, r.last360.net, r.last360.rpt, r.last360.mdd)
	}
	fmt.Printf("\n--- disjoint %dd buckets (newest first): netR / within-bucket MDD ---\n", bucketDays)
	for b := range res[0].bucket {
		end := now.AddDate(0, 0, -b*bucketDays)
		fmt.Printf("%s..%s", end.AddDate(0, 0, -bucketDays).Format("06-01-02"), end.Format("06-01-02"))
		for _, r := range res {
			fmt.Printf("  %s %+7.2f/%5.2f", capLabel(r.capN), r.bucket[b].net, r.bucket[b].mdd)
		}
		fmt.Println()
	}

	// The decision rule, verbatim from §7.
	base := res[0]
	fmt.Printf("\n--- decision rule (§7) vs off: R/trade floor %+.4f (off %+.4f − 1 SE %.4f), MDD off %.2f ---\n",
		base.all.rpt-base.all.se, base.all.rpt, base.all.se, base.all.mdd)
	best := -1
	for i, r := range res[1:] {
		i++
		c1 := r.all.rpt >= base.all.rpt-base.all.se
		c2 := r.all.mdd < base.all.mdd
		better, differ := 0, 0
		var diffs []float64
		for b := range r.bucket {
			if r.bucket[b].mdd != base.bucket[b].mdd {
				differ++
				if r.bucket[b].mdd <= base.bucket[b].mdd {
					better++
				}
			}
			if r.bucket[b].n+base.bucket[b].n > 0 {
				diffs = append(diffs, r.bucket[b].net-base.bucket[b].net)
			}
		}
		c3 := differ == 0 || float64(better) >= 2.0/3.0*float64(differ)
		tdiff := bucketT(diffs)
		c4 := tdiff >= -1
		ok := c1 && c2 && c3 && c4
		fmt.Printf("cap %d: (1) R/trade %+.4f %s  (2) MDD %.2f %s  (3) bucket MDD <= off in %d/%d differing %s  (4) bucket netR diff t = %+.2f %s  → %s\n",
			r.capN, r.all.rpt, mark(c1), r.all.mdd, mark(c2), better, differ, mark(c3), tdiff, mark(c4),
			map[bool]string{true: "ELIGIBLE", false: "not eligible"}[ok])
		if ok && (best < 0 || r.all.mdd < res[best].all.mdd-1 || (math.Abs(r.all.mdd-res[best].all.mdd) <= 1 && r.capN > res[best].capN)) {
			best = i
		}
	}
	if best < 0 {
		fmt.Println("RECOMMENDATION: off (max_same_side 0) — no tested cap is eligible")
	} else {
		fmt.Printf("RECOMMENDATION: max_same_side %d\n", res[best].capN)
	}
}

func capLabel(c int) string {
	if c == 0 {
		return "off"
	}
	return fmt.Sprintf("%d", c)
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
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

// fetcher loads one (symbol, TF) history.
type fetcher func(sym market.Symbol, tf market.Timeframe) ([]market.Candle, error)
