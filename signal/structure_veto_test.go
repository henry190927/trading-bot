package signal

import (
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// walk is a deterministic hourly random walk with regime drift, long enough
// to contain trends, BOS/CHoCH events and neutral stretches.
func walk(seed int64, n int) []market.Candle {
	r := rand.New(rand.NewSource(seed))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px, drift := 100.0, 0.0
	cs := make([]market.Candle, n)
	for i := range cs {
		if i%120 == 0 {
			drift = (r.Float64() - 0.5) * 0.4
		}
		o := px
		px += drift + r.NormFloat64()*0.6
		hi := max(o, px) + r.Float64()*0.4
		lo := min(o, px) - r.Float64()*0.4
		ot := t0.Add(time.Duration(i) * time.Hour)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(time.Hour), Open: o, High: hi, Low: lo, Close: px}
	}
	return cs
}

func TestStructureVetoMatchesEngineDefinition(t *testing.T) {
	cases := []struct {
		st          StructureState
		side        Side
		trend, evnt bool
	}{
		{StructureState{Trend: StructDowntrend}, Long, true, false},
		{StructureState{Trend: StructUptrend}, Long, false, false},
		{StructureState{Event: EvCHoCHDown}, Long, false, true},
		{StructureState{Event: EvBOSDown}, Long, false, true},
		{StructureState{Event: EvBOSUp}, Long, false, false},
		{StructureState{Trend: StructUptrend, Event: EvCHoCHUp}, Short, true, true},
		{StructureState{Event: EvBOSUp}, Short, false, true},
		{StructureState{Trend: StructDowntrend, Event: EvBOSDown}, Short, false, false},
		{StructureState{Trend: StructNeutral}, Short, false, false},
		{StructureState{Trend: StructDowntrend, Event: EvBOSDown}, Flat, false, false},
	}
	for i, c := range cases {
		tr, ev := StructureVeto(c.st, c.side)
		if tr != c.trend || ev != c.evnt {
			t.Errorf("case %d: got (%v,%v) want (%v,%v)", i, tr, ev, c.trend, c.evnt)
		}
	}
}

// The veto at bar i may read bars <= i and nothing else. Replace every bar
// after i with garbage and the decision must not move. A version that read
// cs[:i+2] (one bar of look-ahead), or analysed the whole series, fails.
func TestStructureVetoAtReadsNoFutureBars(t *testing.T) {
	cs := walk(7, 900)
	var vetoedL, vetoedS, passed int
	for i := 0; i < len(cs); i += 3 {
		poisoned := append([]market.Candle(nil), cs...)
		for j := i + 1; j < len(poisoned); j++ {
			p := 1000.0 + float64(j%7)*50 // wild swings that would create new pivots/events
			if j%2 == 0 {
				p = 1.0
			}
			poisoned[j].Open, poisoned[j].High, poisoned[j].Low, poisoned[j].Close = p, p+5, p-0.5, p
		}
		for _, side := range []Side{Long, Short} {
			a := StructureVetoAt(cs, i, 250, side)
			b := StructureVetoAt(poisoned, i, 250, side)
			if a != b {
				t.Fatalf("bar %d side %v: veto %v on clean series, %v once bars after it change — future leak", i, side, a, b)
			}
			switch {
			case a && side == Long:
				vetoedL++
			case a && side == Short:
				vetoedS++
			default:
				passed++
			}
		}
	}
	// Non-vacuous: the fixture must exercise both outcomes on both sides.
	if vetoedL == 0 || vetoedS == 0 || passed == 0 {
		t.Fatalf("fixture too flat: vetoed long %d, short %d, passed %d", vetoedL, vetoedS, passed)
	}
}

// HTF alignment: at asOf, only HTF bars with CloseTime <= asOf count. A 4h
// bar still forming at asOf (or any later bar) is poisoned and must not move
// the decision.
func TestStructureVetoAsOfReadsNoUnclosedHTFBars(t *testing.T) {
	h := walk(5, 400)
	for k := range h { // stretch to 4h bars
		ot := h[0].OpenTime.Add(time.Duration(k) * 4 * time.Hour)
		h[k].OpenTime, h[k].CloseTime = ot, ot.Add(4*time.Hour)
	}
	var vetoes, passes int
	for b := 0; b < 400*4; b += 7 { // base 1h bars
		asOf := h[0].OpenTime.Add(time.Duration(b) * time.Hour) // the base bar's OpenTime
		poisoned := append([]market.Candle(nil), h...)
		for j := range poisoned {
			if poisoned[j].CloseTime.After(asOf) {
				p := 1.0 + float64(j%3)*900
				poisoned[j].Open, poisoned[j].High, poisoned[j].Low, poisoned[j].Close = p, p+5, p-0.5, p
			}
		}
		for _, side := range []Side{Long, Short} {
			a := StructureVetoAsOf(h, asOf, 250, side)
			if a != StructureVetoAsOf(poisoned, asOf, 250, side) {
				t.Fatalf("base bar %d side %v: HTF veto read a bar not closed by %v", b, side, asOf)
			}
			if a {
				vetoes++
			} else {
				passes++
			}
		}
	}
	if vetoes == 0 || passes == 0 {
		t.Fatalf("fixture vacuous: vetoes %d passes %d", vetoes, passes)
	}
	if StructureVetoAsOf(h, h[0].OpenTime, 250, Long) {
		t.Error("before the first HTF close there is no structure, so no veto")
	}
}

// The lookback window ends AT i: the result must equal AnalyzeStructure on
// exactly cs[i-249 : i+1].
func TestStructureVetoAtWindow(t *testing.T) {
	cs := walk(11, 600)
	for i := 0; i < len(cs); i += 5 {
		lb := max(0, i-249)
		tr, ev := StructureVeto(AnalyzeStructure(cs[lb:i+1], 2), Long)
		if got := StructureVetoAt(cs, i, 250, Long); got != (tr || ev) {
			t.Fatalf("bar %d: StructureVetoAt %v, direct %v", i, got, tr || ev)
		}
	}
	if StructureVetoAt(cs, -1, 250, Long) || StructureVetoAt(cs, len(cs), 250, Long) {
		t.Error("out-of-range index must not veto")
	}
}
