package autostrat

import (
	"math"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// ppPath is the hand-traced fixture for the pivot-pullback trigger. 1h closes
// are linear between waypoints (High/Low = close ± 0.1, Open = Close, so no
// two neighbouring bars tie) and 4h bars aggregate four 1h bars.
//
// 4h swings (strength 2): bottoms at 4h 2/12/24/36/45 = 99.9/109.9/119.9/
// 129.9/151.9, tops at 4h 6/18/30/42 = 120.1/130.1/140.1/170.1 → HH-HL.
// Impulse L→T = 129.9 (4h 36) → 170.1 (4h 42): span 40.2, 樞紐區 Hi 150.0 /
// Lo 141.759, invalidate 129.9, target 210.3.
// 1h pullback: 170 (1h 168) → 152 (180) → 157 (186, the lower high, top
// 157.1) → 146 (198, low 145.9 ≤ 150 = in the zone) → 172 (214).
// The close first crosses 157.1 at 1h 205 (157.375, prev 155.75).
// The close first crosses back above 150.0 at 1h 201 (150.875, prev 149.25).
var ppWaypoints = [][2]float64{
	{0, 108}, {8, 100}, {24, 120}, {48, 110}, {72, 130}, {96, 120}, {120, 140},
	{144, 130}, {168, 170}, {180, 152}, {186, 157}, {198, 146}, {214, 172},
}

var ppT0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func ppSeries(mirror bool) (cs, htf []market.Candle) {
	px := func(i int) float64 {
		for k := 1; k < len(ppWaypoints); k++ {
			a, b := ppWaypoints[k-1], ppWaypoints[k]
			if float64(i) <= b[0] {
				return a[1] + (b[1]-a[1])*(float64(i)-a[0])/(b[0]-a[0])
			}
		}
		return ppWaypoints[len(ppWaypoints)-1][1]
	}
	last := int(ppWaypoints[len(ppWaypoints)-1][0])
	for i := 0; i <= last; i++ {
		c := px(i)
		if mirror {
			c = 300 - c
		}
		ot := ppT0.Add(time.Duration(i) * time.Hour)
		cs = append(cs, market.Candle{OpenTime: ot, CloseTime: ot.Add(time.Hour - time.Millisecond),
			Open: c, High: c + 0.1, Low: c - 0.1, Close: c, Volume: 1})
	}
	for k := 0; 4*k+3 < len(cs); k++ {
		g := cs[4*k : 4*k+4]
		h := market.Candle{OpenTime: g[0].OpenTime, CloseTime: g[3].CloseTime, Open: g[0].Open, Close: g[3].Close, High: g[0].High, Low: g[0].Low, Volume: 4}
		for _, c := range g[1:] {
			h.High = math.Max(h.High, c.High)
			h.Low = math.Min(h.Low, c.Low)
		}
		htf = append(htf, h)
	}
	return cs, htf
}

func nearPP(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestPivotPullbackLongFiresOnMicroBOS(t *testing.T) {
	cs, htf := ppSeries(false)
	p := DefaultPivotPullbackParams()
	tr := PivotPullbackTrigger(cs, htf, 205, true, true, p)
	if !tr.Fire || tr.Side != "long" || !tr.Market {
		t.Fatalf("want a market long at 1h 205, got %+v", tr)
	}
	if !nearPP(tr.Entry, 157.375) || !nearPP(tr.Stop, 129.9) || !nearPP(tr.TP, 210.3) {
		t.Errorf("bracket entry %.4f stop %.4f tp %.4f, want 157.375 / 129.9 / 210.3", tr.Entry, tr.Stop, tr.TP)
	}
	// Only the crossing bar fires: before it the close is below the lower
	// high, after it the previous close is already above.
	for _, i := range []int{198, 201, 204, 206, 207} {
		if tr := PivotPullbackTrigger(cs, htf, i, true, true, p); tr.Fire {
			t.Errorf("1h %d fired: %+v", i, tr)
		}
	}
	if tr := PivotPullbackTrigger(cs, htf, 205, false, true, p); tr.Fire {
		t.Errorf("long disabled by side but fired: %+v", tr)
	}
}

func TestPivotPullbackShortMirrors(t *testing.T) {
	cs, htf := ppSeries(true)
	tr := PivotPullbackTrigger(cs, htf, 205, true, true, DefaultPivotPullbackParams())
	if !tr.Fire || tr.Side != "short" {
		t.Fatalf("want a short at 1h 205 on the mirrored series, got %+v", tr)
	}
	if !nearPP(tr.Entry, 300-157.375) || !nearPP(tr.Stop, 300-129.9) || !nearPP(tr.TP, 300-210.3) {
		t.Errorf("bracket entry %.4f stop %.4f tp %.4f", tr.Entry, tr.Stop, tr.TP)
	}
}

// TestPivotPullbackNoLookAhead poisons every 1h bar after the signal bar AND
// every 4h bar that has not closed by the signal bar's open — including 4h 51
// (1h 204–207), which is still forming while 1h 205 is the signal bar. The
// trigger must not change.
func TestPivotPullbackNoLookAhead(t *testing.T) {
	cs, htf := ppSeries(false)
	const i = 205
	p := DefaultPivotPullbackParams()
	want := PivotPullbackTrigger(cs, htf, i, true, true, p)
	if !want.Fire {
		t.Fatal("fixture should fire at 205")
	}
	for j := i + 1; j < len(cs); j++ {
		cs[j].High, cs[j].Low, cs[j].Close, cs[j].Open = 1000, 1, 1, 1000
	}
	poisoned := 0
	for k := range htf {
		if htf[k].CloseTime.After(cs[i].OpenTime) {
			htf[k].High, htf[k].Low, htf[k].Close = 1000, 1, 1
			poisoned++
		}
	}
	if poisoned == 0 {
		t.Fatal("no forming/future 4h bar was poisoned; fixture no longer tests the HTF cut")
	}
	got := PivotPullbackTrigger(cs, htf, i, true, true, p)
	if got.Fire != want.Fire || got.Side != want.Side || got.Entry != want.Entry || got.Stop != want.Stop || got.TP != want.TP {
		t.Errorf("future bars changed the trigger:\nwant %+v\ngot  %+v", want, got)
	}
}

// TestPivotPullbackParamsAreLive: every pre-registered knob must change the
// output on this fixture, or the A/B between arms measures nothing
// (feedback_inert_param_is_a_bug).
func TestPivotPullbackParamsAreLive(t *testing.T) {
	cs, htf := ppSeries(false)
	p0 := DefaultPivotPullbackParams()

	v1 := p0
	v1.Trigger = "reclaim"
	if tr := PivotPullbackTrigger(cs, htf, 201, true, true, v1); !tr.Fire || !nearPP(tr.Entry, 150.875) {
		t.Errorf("reclaim should fire at 201 (close back above 150.0): %+v", tr)
	}
	if tr := PivotPullbackTrigger(cs, htf, 201, true, true, p0); tr.Fire {
		t.Errorf("micro-BOS must not fire at 201 (below the lower high): %+v", tr)
	}
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, v1); tr.Fire {
		t.Errorf("reclaim must not fire at 205 (prev close already above the edge): %+v", tr)
	}

	v2 := p0
	v2.Stop = "pullback"
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, v2); !tr.Fire || !nearPP(tr.Stop, 145.9) || !nearPP(tr.TP, 210.3) {
		t.Errorf("pullback stop should be the dip low 145.9 with the same TP: %+v", tr)
	}

	short := p0
	short.SegmentCap = 10 // segment 196..205 no longer contains the lower high at 186
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, short); tr.Fire {
		t.Errorf("SegmentCap 10 should drop the lower high: %+v", tr)
	}
	thin := p0
	thin.HTFLookback = 20 // 4h 31..50: too few swings for HH-HL
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, thin); tr.Fire {
		t.Errorf("HTFLookback 20 should leave no trend: %+v", tr)
	}
	hi := p0
	hi.MinR = 2.0 // the fixture's bracket is 1.93R
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, hi); tr.Fire {
		t.Errorf("MinR 2.0 should reject a 1.93R bracket: %+v", tr)
	}
	ms := p0
	ms.MicroStrength = 13 // 1h 173 (162.5) is within 13 bars left of 186: no longer a fractal top
	if tr := PivotPullbackTrigger(cs, htf, 205, true, true, ms); tr.Fire {
		t.Errorf("MicroStrength 13 should leave no lower high: %+v", tr)
	}
}

func TestPivotPullbackLegFailures(t *testing.T) {
	cs, htf := ppSeries(false)
	p := DefaultPivotPullbackParams()
	// A 1h high above the leg high inside the segment = the leg extended.
	cs2 := append([]market.Candle(nil), cs...)
	cs2[190].High = 171
	if tr := PivotPullbackTrigger(cs2, htf, 205, true, true, p); tr.Fire {
		t.Errorf("a new high in the segment should cancel: %+v", tr)
	}
	// A 1h close through the leg origin = the leg failed.
	cs3 := append([]market.Candle(nil), cs...)
	cs3[199].Close = 129
	if tr := PivotPullbackTrigger(cs3, htf, 205, true, true, p); tr.Fire {
		t.Errorf("a close below the origin should cancel: %+v", tr)
	}
}
