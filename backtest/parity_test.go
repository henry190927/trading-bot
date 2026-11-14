package backtest

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// The live-parity options must each change the result — a knob whose output
// is byte-identical across values does nothing (the flipbt false +10R).

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func bar(i int, o, h, l, c float64) market.Candle {
	return market.Candle{
		OpenTime:  t0.Add(time.Duration(i) * time.Hour),
		CloseTime: t0.Add(time.Duration(i+1)*time.Hour - time.Millisecond),
		Open:      o, High: h, Low: l, Close: c, Volume: 100,
	}
}

func longSig(entry, stop float64) signal.Signal {
	r := entry - stop
	return signal.Signal{Side: signal.Long, Score: 3, Plan: signal.Plan{
		Entry: entry, StopLoss: stop,
		TakeProfit: []float64{entry + r, entry + 2*r}, RR: []float64{1, 2},
	}}
}

// Long 100 / stop 99: bar 1 fills, bar 2 tags 1R (101.2) but not 2R, bar 3
// stops. Legacy (TP2) books -1; TP1 books +1 on bar 2. The stop is identical.
func TestTPLevelChangesOutcome(t *testing.T) {
	future := []market.Candle{
		bar(1, 100.2, 100.5, 99.8, 100.1),
		bar(2, 100.1, 101.2, 100.0, 101.0),
		bar(3, 101.0, 101.0, 98.5, 98.8),
	}
	sig := longSig(100, 99)
	legacy, _ := simulate("BTC-USDT", sig, future, 0, 0)
	tp1, _ := simulate("BTC-USDT", sig, future, 0, 1)
	if legacy.Outcome != "stop" || legacy.R != -1 {
		t.Fatalf("legacy = %s %+.2f, want stop -1", legacy.Outcome, legacy.R)
	}
	if tp1.Outcome != "tp1" || math.Abs(tp1.R-1) > 1e-9 || tp1.Stop != legacy.Stop {
		t.Fatalf("tp1 = %s %+.2f stop %.2f, want tp1 +1 stop %.2f", tp1.Outcome, tp1.R, tp1.Stop, legacy.Stop)
	}
	tp2, _ := simulate("BTC-USDT", sig, future, 0, 2)
	if tp2.Outcome != "stop" {
		t.Fatalf("tp-level 2 = %s, want the legacy stop", tp2.Outcome)
	}
}

// A marketable long that drifts for 30 bars and then reaches 1R: the legacy
// 24-bar hold times it out near flat; paper execution has no timeout and
// books the TP. Fee: 6bp of 100 notional over 1 of risk = 0.06R.
func TestPaperExecHasNoTimeout(t *testing.T) {
	cs := []market.Candle{bar(0, 100, 100.1, 99.9, 100)}
	for i := 1; i <= 30; i++ {
		cs = append(cs, bar(i, 100, 100.3, 99.7, 100.05))
	}
	cs = append(cs, bar(31, 100.05, 101.5, 100.0, 101.4))
	sig := longSig(100, 99)

	legacy, _ := simulate("BTC-USDT", sig, cs[1:25], 6, 1)
	if legacy.Outcome != "timeout" {
		t.Fatalf("legacy = %s, want timeout", legacy.Outcome)
	}
	opts := Options{PaperExec: true, TPLevel: 1, FeeBpsRoundTrip: 6, CooldownBars: 6}
	// mark 100 (next open) <= entry 100 → marketable, fills at the fire.
	paper, until := simulatePaper("BTC-USDT", "1h", sig, cs, 0, 100, opts)
	if paper.Outcome != "tp" || math.Abs(paper.R-0.94) > 1e-9 {
		t.Fatalf("paper = %s %+.4f, want tp +0.94", paper.Outcome, paper.R)
	}
	// TP on bar 31: DedupFires absorbs fires before its close, so the next
	// position may open on bar 31 itself.
	if until != 30 {
		t.Fatalf("until = %d, want 30", until)
	}
}

// A stop adds the cooldown; a no-fill holds the slot for the expiry.
func TestPaperDedupHolds(t *testing.T) {
	cs := []market.Candle{bar(0, 100, 100.1, 99.9, 100)}
	cs = append(cs, bar(1, 100, 100.2, 98.5, 98.7)) // stops a marketable long
	for i := 2; i < 30; i++ {
		cs = append(cs, bar(i, 98.7, 98.9, 98.6, 98.8))
	}
	sig := longSig(100, 99)
	st, until := simulatePaper("BTC-USDT", "1h", sig, cs, 0, 100, Options{PaperExec: true, CooldownBars: 6})
	if st.Outcome != "stop" || until != 6 { // exit bar 1 + 6 bars: fires through bar 6 absorbed
		t.Fatalf("stop: %s until %d, want stop until 6", st.Outcome, until)
	}
	_, until0 := simulatePaper("BTC-USDT", "1h", sig, cs, 0, 100, Options{PaperExec: true})
	if until0 != 0 {
		t.Fatalf("cooldown 0: until %d, want 0", until0)
	}
	// Resting long limit at 97 (mark 98.7 > entry) never touched → no-fill
	// after 6 bars, and the slot is held through bar 6+1-1.
	lim := longSig(97, 96)
	nf, untilNF := simulatePaper("BTC-USDT", "1h", lim, cs, 1, 98.7, Options{PaperExec: true})
	if nf.Outcome != "no-fill" || untilNF != 6 {
		t.Fatalf("no-fill: %s until %d, want no-fill until 6", nf.Outcome, untilNF)
	}
	nf3, untilNF3 := simulatePaper("BTC-USDT", "1h", lim, cs, 1, 98.7, Options{PaperExec: true, ExpiryBars: 3})
	if nf3.Outcome != "no-fill" || untilNF3 != 3 {
		t.Fatalf("expiry 3: %s until %d, want no-fill until 3", nf3.Outcome, untilNF3)
	}
}

func walk(n int, seed int64) []market.Candle {
	rng := rand.New(rand.NewSource(seed))
	cs := make([]market.Candle, n)
	px := 100.0
	for i := range cs {
		o := px
		c := o * (1 + rng.NormFloat64()*0.012)
		h := math.Max(o, c) * (1 + math.Abs(rng.NormFloat64())*0.006)
		l := math.Min(o, c) * (1 - math.Abs(rng.NormFloat64())*0.006)
		cs[i] = bar(i, o, h, l, c)
		cs[i].Volume = 100 + rng.Float64()*200
		px = c
	}
	return cs
}

func resultKey(r Result) [3]float64 {
	return [3]float64{float64(r.NumSignals), float64(len(r.Trades)), r.TotalR}
}

// Through Run, on a seeded random walk: each parity option moves the result.
func TestParityOptionsAreNotInert(t *testing.T) {
	cs := walk(900, 7)
	base := Run("BTC-USDT", "1h", cs, nil, 3, 24, Options{FeeBpsRoundTrip: 6})
	if base.NumSignals < 5 {
		t.Fatalf("walk produced %d signals; the comparison below would be vacuous", base.NumSignals)
	}
	for name, o := range map[string]Options{
		"TPLevel=1":        {FeeBpsRoundTrip: 6, TPLevel: 1},
		"PaperExec":        {FeeBpsRoundTrip: 6, PaperExec: true, CooldownBars: 6},
		"EngineWindow=150": {FeeBpsRoundTrip: 6, EngineWindow: 150},
	} {
		got := Run("BTC-USDT", "1h", cs, nil, 3, 24, o)
		if resultKey(got) == resultKey(base) {
			t.Errorf("%s: identical to legacy %v — the option is inert", name, resultKey(base))
		}
	}
}

// BiasWindow below MACD's 35-bar minimum is Flat everywhere (signal.Bias),
// while the whole-series MACD is not — so the window is really applied.
func TestBiasWindowApplied(t *testing.T) {
	base := walk(400, 1)
	bias := make([]market.Candle, 100)
	for i := range bias {
		b := base[i*4]
		bias[i] = market.Candle{OpenTime: b.OpenTime, CloseTime: base[i*4+3].CloseTime, Close: base[i*4+3].Close}
	}
	full := precomputeBiases(base, bias)
	short := precomputeBiasesWindowed(base, bias, 20)
	wide := precomputeBiasesWindowed(base, bias, 1000)
	nonFlat := 0
	for i := range base {
		if full[i] != signal.Flat {
			nonFlat++
		}
		if short[i] != signal.Flat {
			t.Fatalf("bar %d: window 20 < 35 gave %v, want Flat", i, short[i])
		}
		// Legacy reads MACD at any index once the series has 35 bars;
		// signal.Bias wants 35 bars IN the window. Equal from bias bar 34 on.
		if base[i].OpenTime.After(bias[34].CloseTime) && wide[i] != full[i] {
			t.Fatalf("bar %d: window covering everything %v != full %v", i, wide[i], full[i])
		}
	}
	if nonFlat == 0 {
		t.Fatal("full-series bias is Flat everywhere; the test proves nothing")
	}
}
