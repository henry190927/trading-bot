package autostrat

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func ohlc(i int, o, h, l, c float64) market.Candle {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ot := t0.Add(time.Duration(i) * 4 * time.Hour)
	return market.Candle{OpenTime: ot, CloseTime: ot.Add(4*time.Hour - time.Millisecond), Open: o, High: h, Low: l, Close: c, Volume: 1}
}

// fixture is hand-traced in TestSimulateHTFTrendHandTraced.
func trendFixture() []market.Candle {
	rows := [][4]float64{
		{10, 11, 9, 10},          // 0
		{10, 11, 9, 10},          // 1
		{10, 11, 9, 10},          // 2
		{10, 12, 10, 11.5},       // 3  close 11.5 > max H(0..2)=11 → LONG 11.5, stop 10.5
		{11.5, 13, 11, 12.8},     // 4  stop 10.5 (min L(2..3)=9 below it)
		{12.8, 14, 12, 13.5},     // 5  stop 10.5 (min L(3..4)=10)
		{13.5, 14.5, 12.5, 14},   // 6  stop 11 (min L(4..5))
		{14, 14.2, 11.5, 12},     // 7  stop 12 (min L(5..6)) → Low 11.5 hits → exit 12, +0.5R
		{12, 12.5, 11.8, 12.2},   // 8  no signal (channel 5..7: 11.5..14.5)
		{12, 12.1, 10, 10.2},     // 9  close 10.2 < min L(6..8)=11.5 → SHORT 10.2, stop 11.2
		{11.5, 11.6, 11.0, 11.4}, // 10 opens 11.5 through the 11.2 stop → exit at the OPEN 11.5, -1.3R
		{11.4, 11.5, 11.3, 11.4}, // 11
	}
	cs := make([]market.Candle, len(rows))
	for i, r := range rows {
		cs[i] = ohlc(i, r[0], r[1], r[2], r[3])
	}
	return cs
}

func constATR(n int, v float64) []float64 {
	a := make([]float64, n)
	for i := range a {
		a[i] = v
	}
	return a
}

func fixtureParams() HTFTrendParams {
	return HTFTrendParams{EntryN: 3, TrailM: 2, ATRPeriod: 20, StopATR: 1}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSimulateHTFTrendHandTraced(t *testing.T) {
	cs := trendFixture()
	tr := SimulateHTFTrend(cs, constATR(len(cs), 1), 0, true, true, fixtureParams(), 10)
	if len(tr) != 2 {
		t.Fatalf("want 2 trades, got %d: %+v", len(tr), tr)
	}
	a, b := tr[0], tr[1]
	if a.Side != "long" || a.EntryIdx != 3 || !near(a.Entry, 11.5) || !near(a.InitStop, 10.5) ||
		a.ExitIdx != 7 || !near(a.Exit, 12) || !near(a.GrossR, 0.5) || a.Open || a.BarsHeld != 4 {
		t.Errorf("long: %+v", a)
	}
	// fee = 10bp × 11.5 / 1.0 risk
	if !near(a.FeeR, 0.0115) || !near(a.NetR, 0.5-0.0115) {
		t.Errorf("long fee %.6f net %.6f", a.FeeR, a.NetR)
	}
	if b.Side != "short" || b.EntryIdx != 9 || !near(b.Entry, 10.2) || !near(b.InitStop, 11.2) ||
		b.ExitIdx != 10 || !near(b.Exit, 11.5) || !near(b.GrossR, -1.3) || b.Open {
		t.Errorf("short (gap through stop → fill at open): %+v", b)
	}
	if !near(b.FeeR, 0.0102) {
		t.Errorf("short fee %.6f", b.FeeR)
	}
	// MTM: before entry 0, entry bar = -fee, bar 5 close 13.5 → +2.0 - fee, after exit NetR.
	if a.MTMR(cs, 2) != 0 || !near(a.MTMR(cs, 3), -0.0115) || !near(a.MTMR(cs, 5), 2-0.0115) || !near(a.MTMR(cs, 11), a.NetR) {
		t.Errorf("MTM path wrong")
	}
}

// The fee must be exactly autotrade.WithFee's conversion.
func TestHTFTrendFeeMatchesWithFee(t *testing.T) {
	f := autotrade.PaperFire{Side: "long", Entry: 100, Stop: 97}
	oc := autotrade.WithFee(f, autotrade.Outcome{Status: autotrade.OutTP, NetR: 2, FillPrice: 100}, 6)
	if got := trendFeeR("long", 100, 97, 6); !near(got, 2-oc.NetR) {
		t.Errorf("fee %.6f, WithFee charged %.6f", got, 2-oc.NetR)
	}
}

func TestSimulateHTFTrendParamsAreLive(t *testing.T) {
	cs := trendFixture()
	atr := constATR(len(cs), 1)
	base := SimulateHTFTrend(cs, atr, 0, true, true, fixtureParams(), 0)

	m1 := fixtureParams()
	m1.TrailM = 1 // trail ratchets to bar 6's low 12.5 → long exits 12.5, +1.0R
	if got := SimulateHTFTrend(cs, atr, 0, true, true, m1, 0); len(got) == 0 || near(got[0].GrossR, base[0].GrossR) || !near(got[0].GrossR, 1.0) {
		t.Errorf("TrailM inert: %+v", got)
	}
	k2 := fixtureParams()
	k2.StopATR = 2
	if got := SimulateHTFTrend(cs, atr, 0, true, true, k2, 0); len(got) == 0 || near(got[0].Risk, base[0].Risk) {
		t.Errorf("StopATR inert: %+v", got)
	}
	n5 := fixtureParams()
	n5.EntryN = 5
	got := SimulateHTFTrend(cs, atr, 0, true, true, n5, 0)
	if len(got) > 0 && got[0].EntryIdx == base[0].EntryIdx {
		t.Errorf("EntryN inert: %+v", got)
	}
	if got := SimulateHTFTrend(cs, atr, 0, false, true, fixtureParams(), 0); len(got) == 0 || got[0].Side != "short" {
		t.Errorf("wantLong=false still went long: %+v", got)
	}
	if got := SimulateHTFTrend(cs, atr, 0, true, true, fixtureParams(), 10); near(got[0].NetR, base[0].NetR) {
		t.Errorf("feeBps inert")
	}
}

func randomWalk(n int, seed int64) []market.Candle {
	r := rand.New(rand.NewSource(seed))
	cs := make([]market.Candle, n)
	px := 100.0
	for i := range cs {
		o := px
		c := o * (1 + r.NormFloat64()*0.02 + 0.002*math.Sin(float64(i)/25))
		h := math.Max(o, c) * (1 + r.Float64()*0.01)
		l := math.Min(o, c) * (1 - r.Float64()*0.01)
		cs[i] = ohlc(i, o, h, l, c)
		px = c
	}
	return cs
}

// Poison every bar from P on with absurd values: no trigger at i < P may
// change, and every trade that exited before P must be byte-identical.
func TestHTFTrendNoFutureBars(t *testing.T) {
	p := DefaultHTFTrendParams()
	for seed := int64(1); seed <= 5; seed++ {
		cs := randomWalk(400, seed)
		atr := indicator.ATR(cs, p.ATRPeriod)
		clean := SimulateHTFTrend(cs, atr, 21, true, true, p, 6)
		if len(clean) < 5 {
			t.Fatalf("seed %d: only %d trades, fixture too quiet", seed, len(clean))
		}
		for _, P := range []int{150, 250, 330} {
			bad := append([]market.Candle(nil), cs...)
			for k := P; k < len(bad); k++ {
				bad[k].Open, bad[k].High, bad[k].Low, bad[k].Close = 1e6, 1e7, 1e-6, 5e5*float64(k%2)+1
			}
			badATR := indicator.ATR(bad, p.ATRPeriod) // ATR is causal: equal to clean before P
			for i := p.EntryN; i < P; i++ {
				if a, b := HTFTrendTrigger(cs, atr[i], i, true, true, p), HTFTrendTrigger(bad, badATR[i], i, true, true, p); a.Fire != b.Fire || a.Side != b.Side || a.Entry != b.Entry || a.Stop != b.Stop {
					t.Fatalf("seed %d P %d: trigger at bar %d read the future", seed, P, i)
				}
			}
			poisoned := SimulateHTFTrend(bad, badATR, 21, true, true, p, 6)
			for k, tc := range clean {
				if tc.ExitIdx >= P {
					break
				}
				if k >= len(poisoned) || poisoned[k] != tc {
					t.Fatalf("seed %d P %d: trade %d exiting at %d changed with bars ≥ %d poisoned", seed, P, k, tc.ExitIdx, P)
				}
			}
		}
	}
}

// A trade's stop on bar j must not use bar j itself: a bar whose low first
// raises the M-bar channel and then trades through it must NOT exit at the
// raised level.
func TestHTFTrendTrailUsesOnlyPriorBars(t *testing.T) {
	cs := trendFixture()
	// On bar 6 the stop is 11 (from bars 4..5). Bar 6's own low (12.5) would
	// raise it to 12 only for bar 7. Make bar 6 dip to 11.2: still above 11,
	// so no exit on 6 even though min L(5..6) would be 11.2 > 11.
	cs[6].Low = 11.2
	tr := SimulateHTFTrend(cs, constATR(len(cs), 1), 0, true, true, fixtureParams(), 0)
	if tr[0].ExitIdx == 6 {
		t.Fatalf("exited on bar 6 using a stop derived from bar 6: %+v", tr[0])
	}
}
