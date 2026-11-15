package autotrade

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// Fixed-bracket scoring and the panel's totals must not move when the trail
// path lands. This pins them: a log shaped like the real one (four strategies,
// market and resting-limit entries, tp / stop / no-fill / open / pending /
// unscored, a same-side collision the caps refuse, re-fires inside a held
// slot) run through every function the executor and /ops call, compared with
// output captured on the commit BEFORE the trail resolver existed (09db44a).
// A change here is a change to every existing rule's numbers.

func pinCandles() []market.Candle {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cs := make([]market.Candle, 240)
	prev := 100.0
	for i := range cs {
		c := 100 + 6*math.Sin(float64(i)/9) + 2.5*math.Sin(float64(i)/2.3)
		hi := math.Max(prev, c) + 0.4 + 0.3*math.Abs(math.Sin(float64(i)))
		lo := math.Min(prev, c) - 0.4 - 0.3*math.Abs(math.Cos(float64(i)))
		ot := t0.Add(time.Duration(i) * time.Hour)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(time.Hour - time.Millisecond), Open: prev, High: hi, Low: lo, Close: c}
		prev = c
	}
	return cs
}

func pinFires(cs []market.Candle) []PaperFire { // oldest-first
	var out []PaperFire
	add := func(i int, sym, strat, side string, market bool, entOff, stopD, tpD float64, expiry int) {
		var t time.Time
		var px float64
		if i < 0 { // before the candles: unscored
			t, px = cs[0].OpenTime.Add(time.Duration(i)*time.Hour), 100
		} else {
			t, px = cs[i].CloseTime.Add(time.Minute), cs[i].Close
		}
		e := px + entOff
		f := PaperFire{Time: t, Symbol: sym, TF: "1h", Strategy: strat, Side: side, Market: market,
			Entry: e, Margin: 20, Lev: 10, RiskUSDT: 4, Qty: 1, ExpiryBars: expiry}
		if side == "long" {
			f.Stop, f.TP = e-stopD, e+tpD
		} else {
			f.Stop, f.TP = e+stopD, e-tpD
		}
		out = append(out, f)
	}
	add(-30, "BTC", "range-edge", "long", true, 0, 1.5, 3, 0)
	for i := 3; i < 236; i += 7 {
		side := "long"
		if (i/7)%2 == 1 {
			side = "short"
		}
		add(i, "BTC", "range-edge", side, true, 0, 1.5, 3, 0)
		add(i+1, "ETH", "engine", side, false, map[string]float64{"long": -0.8, "short": 0.8}[side], 2, 4, 0)
		add(i+1, "ETH", "htf-snr", side, true, 0, 1.2, 2.4, 0) // same symbol+side, same bar: caps
		if i%3 == 0 {
			add(i+2, "SOL", "vp-node", side, false, map[string]float64{"long": -3, "short": 3}[side], 1, 5, 24)
		}
	}
	add(238, "BTC", "range-edge", "long", true, 0, 10, 20, 0) // wide bracket: still open at the end
	return out
}

func pinRender(t *testing.T, resolve func(PaperFire, []market.Candle, float64) Outcome, resolveClosed func(PaperFire, []market.Candle) Outcome) string {
	t.Helper()
	cs := pinCandles()
	fires := pinFires(cs)
	now := cs[len(cs)-1].CloseTime
	live := cs[len(cs)-1].Close + 0.3
	newest := make([]PaperFire, len(fires))
	for i, f := range fires {
		newest[len(fires)-1-i] = f
	}
	candles := func(sym, tf string) []market.Candle {
		if sym == "SOL" {
			return cs[:200] // a short window: its newest fire is past the end
		}
		return cs
	}
	var b strings.Builder
	bk, un := BuildBook(newest, candles, now, 6)
	fmt.Fprintf(&b, "book open=%d margin=%.2f todayR=%+.6f legs=%d unscored=%d\n", bk.OpenCount, bk.OpenMargin, bk.RealizedRToday, len(bk.OpenLegs), un)

	cfg := Config{MaxSameSymbolSide: 1, MaxConcurrentTotal: 4, DailyLossHaltR: -3,
		Rules: []Rule{{Symbol: "BTC", Strategy: "range-edge"}, {Symbol: "ETH", Strategy: "engine"}, {Symbol: "ETH", Strategy: "htf-snr"}, {Symbol: "SOL", Strategy: "vp-node"}}}
	ps, blocked := ReplayWithCaps(fires, cfg, 6, 6, time.Hour, func(f PaperFire) Outcome { return resolve(f, candles(f.Symbol, f.TF), live) })
	var outs []Outcome
	for _, p := range ps {
		outs = append(outs, p.Outcome)
		fmt.Fprintf(&b, "%s %s %s %s %s %+.6f %+.6f abs=%d\n", p.Fire.Time.Format("01-02T15"), p.Fire.Symbol, p.Fire.Strategy, p.Fire.Side,
			p.Outcome.Status, p.Outcome.NetR, p.Outcome.UnrealR, p.Absorbed)
	}
	fmt.Fprintf(&b, "blocked=%d\n", len(blocked))
	s := Summarize(outs)
	fmt.Fprintf(&b, "sum %+v rpt=%.6f\n", s, s.RPerTrade())
	for _, g := range Breakdown(ps, ByStrategy) {
		fmt.Fprintf(&b, "grp %s n=%d tp=%d stop=%d netR=%+.6f wr=%.6f\n", g.Key, g.Total, g.TP, g.Stop, g.NetR, g.WinRate)
	}
	dd := DedupFires(fires, 6, 6, time.Hour, func(f PaperFire) Outcome { return resolveClosed(f, candles(f.Symbol, f.TF)) })
	var douts []Outcome
	for _, p := range dd {
		douts = append(douts, p.Outcome)
	}
	fmt.Fprintf(&b, "dedup %+v\n", Summarize(douts))
	pb := ComputePaperBalance(ps, 400, cs[0].OpenTime)
	fmt.Fprintf(&b, "bal %+v\n", pb)
	return b.String()
}

func TestFixedBracketPanelPinned(t *testing.T) {
	got := pinRender(t,
		func(f PaperFire, cs []market.Candle, live float64) Outcome { return EvaluateFireLive(f, cs, 6, live) },
		func(f PaperFire, cs []market.Candle) Outcome { return EvaluateFire(f, cs, 6) })
	if got != pinGolden {
		t.Errorf("fixed-bracket panel output moved.\n--- got ---\n%s\n--- want ---\n%s", got, pinGolden)
	}
}

// The same log through the dispatchers the call sites now use (book.go,
// autoexec.go paperBlocked, cmd/web/autotrade.go): identical output.
func TestFixedBracketPanelPinnedViaResolve(t *testing.T) {
	got := pinRender(t,
		func(f PaperFire, cs []market.Candle, live float64) Outcome { return ResolveLive(f, cs, 6, live) },
		func(f PaperFire, cs []market.Candle) Outcome { return Resolve(f, cs, 6) })
	if got != pinGolden {
		t.Errorf("fixed-bracket panel output moved through Resolve.\n--- got ---\n%s\n--- want ---\n%s", got, pinGolden)
	}
}

// Captured on 09db44a (before EvaluateTrailFire / Resolve / WinRate-by-NetR).
const pinGolden = `book open=3 margin=60.00 todayR=-2.000000 legs=3 unscored=0
09-29T18 BTC range-edge long unscored +0.000000 +0.000000 abs=0
10-01T04 BTC range-edge long stop -1.000000 +0.000000 abs=1
10-01T05 ETH engine long tp +2.000000 +0.000000 abs=1
10-01T06 SOL vp-node long stop -1.000000 +0.000000 abs=1
10-01T12 ETH htf-snr short stop -1.000000 +0.000000 abs=1
10-01T18 BTC range-edge long stop -1.000000 +0.000000 abs=1
10-01T19 ETH engine long stop -1.000000 +0.000000 abs=1
10-02T02 ETH htf-snr short tp +2.000000 +0.000000 abs=1
10-02T08 BTC range-edge long stop -1.000000 +0.000000 abs=1
10-02T09 ETH engine long stop -1.000000 +0.000000 abs=1
10-02T16 ETH htf-snr short stop -1.000000 +0.000000 abs=1
10-02T22 BTC range-edge long tp +2.000000 +0.000000 abs=1
10-02T23 ETH engine long tp +2.000000 +0.000000 abs=1
10-03T00 SOL vp-node long no-fill +0.000000 +0.000000 abs=1
10-03T06 ETH htf-snr short stop -1.000000 +0.000000 abs=1
10-03T12 BTC range-edge long tp +2.000000 +0.000000 abs=0
10-03T13 ETH engine long tp +2.000000 +0.000000 abs=1
10-03T19 BTC range-edge short stop -1.000000 +0.000000 abs=1
10-03T20 ETH htf-snr short stop -1.000000 +0.000000 abs=1
10-04T03 ETH engine long stop -1.000000 +0.000000 abs=1
10-04T09 BTC range-edge short tp +2.000000 +0.000000 abs=0
10-04T10 ETH htf-snr short tp +2.000000 +0.000000 abs=0
10-04T16 BTC range-edge long stop -1.000000 +0.000000 abs=1
10-04T17 ETH engine long stop -1.000000 +0.000000 abs=1
10-04T18 SOL vp-node long stop -1.000000 +0.000000 abs=0
10-05T00 ETH htf-snr short tp +2.000000 +0.000000 abs=0
10-05T06 BTC range-edge long tp +2.000000 +0.000000 abs=0
10-05T07 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-05T13 BTC range-edge short stop -1.000000 +0.000000 abs=1
10-05T14 ETH engine short stop -1.000000 +0.000000 abs=1
10-05T15 SOL vp-node short stop -1.000000 +0.000000 abs=0
10-05T21 ETH htf-snr long tp +2.000000 +0.000000 abs=0
10-06T03 BTC range-edge short tp +2.000000 +0.000000 abs=0
10-06T04 ETH engine short no-fill +0.000000 +0.000000 abs=0
10-06T10 BTC range-edge long tp +2.000000 +0.000000 abs=0
10-06T11 ETH engine long stop -1.000000 +0.000000 abs=1
10-06T12 SOL vp-node long stop -1.000000 +0.000000 abs=0
10-06T17 BTC range-edge short tp +2.000000 +0.000000 abs=0
10-06T18 ETH htf-snr short tp +2.000000 +0.000000 abs=0
10-07T00 BTC range-edge long stop -1.000000 +0.000000 abs=1
10-07T01 ETH engine long stop -1.000000 +0.000000 abs=1
10-07T08 ETH htf-snr short tp +2.000000 +0.000000 abs=0
10-07T09 SOL vp-node short stop -1.000000 +0.000000 abs=0
10-07T14 BTC range-edge long tp +2.000000 +0.000000 abs=0
10-07T15 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-07T21 BTC range-edge short stop -1.000000 +0.000000 abs=1
10-07T22 ETH engine short stop -1.000000 +0.000000 abs=1
10-08T05 ETH htf-snr long tp +2.000000 +0.000000 abs=0
10-08T06 SOL vp-node long stop -1.000000 +0.000000 abs=1
10-08T11 BTC range-edge short tp +2.000000 +0.000000 abs=0
10-08T12 ETH engine short no-fill +0.000000 +0.000000 abs=0
10-08T18 BTC range-edge long tp +2.000000 +0.000000 abs=0
10-08T19 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-09T01 BTC range-edge short tp +2.000000 +0.000000 abs=0
10-09T02 ETH engine short no-fill +0.000000 +0.000000 abs=0
10-09T08 BTC range-edge long stop -1.000000 +0.000000 abs=2
10-09T09 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-09T16 ETH engine short no-fill +0.000000 +0.000000 abs=0
10-09T23 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-10T00 SOL vp-node long pending +0.000000 +0.000000 abs=1
10-10T05 BTC range-edge short stop -1.000000 +0.000000 abs=2
10-10T06 ETH engine short no-fill +0.000000 +0.000000 abs=0
10-10T13 ETH engine long no-fill +0.000000 +0.000000 abs=0
10-10T20 ETH engine short pending +0.000000 +0.000000 abs=0
10-10T23 BTC range-edge long open +0.000000 -0.064344 abs=0
blocked=18
sum {Total:65 Filled:50 Pending:2 NoFill:12 Open:1 TP:21 Stop:28 Unscored:1 NetR:14 UnrealR:-0.06434403021332713 WinRate:0.42857142857142855 FillPct:0.8064516129032258} rpt=0.285714
grp range-edge n=23 tp=11 stop=10 netR=+12.000000 wr=0.523810
grp htf-snr n=11 tp=7 stop=4 netR=+10.000000 wr=0.636364
grp engine n=23 tp=3 stop=8 netR=-2.000000 wr=0.272727
grp vp-node n=8 tp=0 stop=6 netR=-6.000000 wr=0.000000
dedup {Total:74 Filled:59 Pending:2 NoFill:12 Open:1 TP:22 Stop:36 Unscored:1 NetR:8 UnrealR:-0.09434403021332685 WinRate:0.3793103448275862 FillPct:0.8309859154929577}
bal {Base:400 Since:2026-10-01 00:00:00 +0000 UTC Realized:53.04566191722058 Unrealized:-0.2573761208533085 Balance:453.04566191722057 Closed:49 Open:1 NoRisk:0}
`
