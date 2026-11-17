package main

// Cross-sectional momentum rotation (docs/xsmom_design.md §3): on each
// rebalance bar rank the eligible symbols by their L-day return, long the top
// k and short the bottom k at the close, each leg with a fixed 3×ATR(20) stop
// (= 1R, equal risk per leg). A leg that stays in its bucket is held; one
// that leaves it exits at the rebalance close; a stopped leg waits for the
// next rebalance.

import (
	"sort"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// Params are one arm's settings.
type Params struct {
	L         int     // lookback in days (bars)
	K         int     // legs per side
	ATRPeriod int     // ATR for the stop
	StopATR   float64 // stop distance in ATR = 1R
}

// series is one symbol's daily bars with its ATR and an OpenTime index.
type series struct {
	short string
	cs    []market.Candle
	atr   []float64
	at    map[int64]int // OpenTime unix ms → index
}

func newSeries(short string, cs []market.Candle, atrPeriod int) series {
	s := series{short: short, cs: cs, atr: indicator.ATR(cs, atrPeriod), at: make(map[int64]int, len(cs))}
	for i, c := range cs {
		s.at[c.OpenTime.UnixMilli()] = i
	}
	return s
}

// Leg is one trade (one leg entry → exit) with its symbol and exit reason.
type Leg struct {
	autostrat.TrendTrade
	Sym    int    // index into the series slice
	Reason string // "stop" | "rebal" | "open"
}

// sundayRebal is the pre-registered schedule: the 1d bar opening Sunday 00:00
// UTC (it closes Monday 00:00 UTC).
func sundayRebal(t time.Time) bool {
	t = t.UTC()
	return t.Weekday() == time.Sunday && t.Hour() == 0 && t.Minute() == 0
}

func feeR(side string, entry, stop, feeBps float64) float64 {
	f := autotrade.PaperFire{Side: side, Entry: entry, Stop: stop}
	oc := autotrade.WithFee(f, autotrade.Outcome{Status: autotrade.OutStop, FillPrice: entry}, feeBps)
	return -oc.NetR
}

func sideDir(side string) float64 {
	if side == "short" {
		return -1
	}
	return 1
}

// simulate runs the rotation over the union of the series' OpenTimes. It
// returns every leg (closed and open) per symbol, in entry order, plus the
// number of rebalances that had a book (≥ 2k+1 eligible).
func simulate(ss []series, p Params, isRebal func(time.Time) bool, feeBps float64) ([][]Leg, int) {
	out := make([][]Leg, len(ss))
	if p.K < 1 || p.L < 1 || p.StopATR <= 0 {
		return out, 0
	}
	var times []int64
	seen := map[int64]bool{}
	for _, s := range ss {
		for _, c := range s.cs {
			k := c.OpenTime.UnixMilli()
			if !seen[k] {
				seen[k] = true
				times = append(times, k)
			}
		}
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })

	open := make([]*Leg, len(ss))
	closeLeg := func(s int, j int, px float64, reason string) {
		l := open[s]
		cs := ss[s].cs
		l.ExitIdx, l.ExitTime, l.Exit, l.Open, l.Reason = j, cs[j].CloseTime, px, false, reason
		l.BarsHeld = j - l.EntryIdx
		l.GrossR = sideDir(l.Side) * (px - l.Entry) / l.Risk
		l.NetR = l.GrossR - l.FeeR
		out[s] = append(out[s], *l)
		open[s] = nil
	}
	books := 0
	for _, tk := range times {
		t := time.UnixMilli(tk).UTC()
		// 1. stops on this bar for legs entered before it.
		for s := range ss {
			l := open[s]
			if l == nil {
				continue
			}
			j, ok := ss[s].at[tk]
			if !ok || j <= l.EntryIdx {
				continue
			}
			b := ss[s].cs[j]
			if l.Side == "long" {
				if b.Open <= l.InitStop {
					closeLeg(s, j, b.Open, "stop")
				} else if b.Low <= l.InitStop {
					closeLeg(s, j, l.InitStop, "stop")
				}
			} else {
				if b.Open >= l.InitStop {
					closeLeg(s, j, b.Open, "stop")
				} else if b.High >= l.InitStop {
					closeLeg(s, j, l.InitStop, "stop")
				}
			}
		}
		if !isRebal(t) {
			continue
		}
		// 2. rank.
		type cand struct {
			s   int
			ret float64
		}
		var el []cand
		lagKey := t.AddDate(0, 0, -p.L).UnixMilli()
		for s := range ss {
			j, ok := ss[s].at[tk]
			jl, okl := ss[s].at[lagKey]
			if !ok || !okl || ss[s].atr[j] <= 0 || ss[s].cs[jl].Close <= 0 {
				continue
			}
			el = append(el, cand{s, ss[s].cs[j].Close/ss[s].cs[jl].Close - 1})
		}
		want := make([]string, len(ss))
		if len(el) >= 2*p.K+1 {
			books++
			sort.SliceStable(el, func(a, b int) bool {
				if el[a].ret != el[b].ret {
					return el[a].ret > el[b].ret
				}
				return ss[el[a].s].short < ss[el[b].s].short
			})
			for x := 0; x < p.K; x++ {
				want[el[x].s] = "long"
				want[el[len(el)-1-x].s] = "short"
			}
		}
		// 3–5. exit legs that left their bucket, hold the rest, enter new ones.
		for s := range ss {
			j, ok := ss[s].at[tk]
			if !ok {
				continue // no bar: an open leg cannot trade today; it is held
			}
			c := ss[s].cs[j]
			if open[s] != nil && open[s].Side != want[s] {
				closeLeg(s, j, c.Close, "rebal")
			}
			if open[s] == nil && want[s] != "" {
				d := p.StopATR * ss[s].atr[j]
				stop := c.Close - sideDir(want[s])*d
				open[s] = &Leg{Sym: s, TrendTrade: autostrat.TrendTrade{
					Side: want[s], EntryIdx: j, EntryTime: c.CloseTime, Entry: c.Close, InitStop: stop, Risk: d,
					StopPct: d / c.Close * 100, MaxLev: autotrade.MaxLevForStop(d / c.Close),
					FeeR: feeR(want[s], c.Close, stop, feeBps),
				}}
			}
		}
	}
	for s := range ss {
		if open[s] == nil {
			continue
		}
		j := len(ss[s].cs) - 1
		closeLeg(s, j, ss[s].cs[j].Close, "open")
		out[s][len(out[s])-1].Open = true
	}
	return out, books
}

// legsToTrades flattens one symbol's legs to TrendTrades (for MTM helpers).
func legsToTrades(ls []Leg) []autostrat.TrendTrade {
	out := make([]autostrat.TrendTrade, len(ls))
	for i, l := range ls {
		out[i] = l.TrendTrade
	}
	return out
}
