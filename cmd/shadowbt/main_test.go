package main

import (
	"math"
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// bar builds one candle. Times are only used for ordering and fire stamps.
func bar(i int, o, h, l, c float64) market.Candle {
	t := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
	return market.Candle{
		OpenTime: t, CloseTime: t.Add(time.Hour - time.Millisecond),
		Open: o, High: h, Low: l, Close: c, Volume: 100,
	}
}

// flat builds n identical bars so the detector's 60-bar warmup is satisfied
// without any of the padding accidentally creating levels near the test bar.
func flat(n int, px float64) []market.Candle {
	out := make([]market.Candle, n)
	for i := range out {
		out[i] = bar(i, px, px+0.01, px-0.01, px)
	}
	return out
}

func constATR(n int, v float64) []float64 {
	a := make([]float64, n)
	for i := range a {
		a[i] = v
	}
	return a
}

// A bar that pokes ABOVE a level and closes back below is a rejected
// resistance — the short side. This is the shape the engine reacts to one
// close later, at a worse price, which is the whole subject of the harness.
func TestDetectHitsUpperWickReject(t *testing.T) {
	cs := flat(61, 100)
	// Level = the weekly/daily open at 100 (all the padding opens there).
	// Test bar: opens 99, spikes to 103, closes 98 -> poked through 100.
	cs = append(cs, bar(61, 99, 103, 97, 98))
	atr := constATR(len(cs), 10)

	hits := detectHits(cs, atr, 0.0015, true, false) // opens only, precise level
	var found *levelHit
	for i := range hits {
		if hits[i].Bar == 61 && hits[i].Side == "short" {
			found = &hits[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no short hit on the poke-above bar; hits=%+v", hits)
	}
	if found.Wick != 103 {
		t.Errorf("Wick = %v, want the bar High 103", found.Wick)
	}
	if found.Close != 98 {
		t.Errorf("Close = %v, want 98", found.Close)
	}
	// depth = (High - level)/ATR = (103-100)/10 = 0.30
	if math.Abs(found.DepthATR-0.30) > 1e-9 {
		t.Errorf("DepthATR = %v, want 0.30", found.DepthATR)
	}
	// slip = (level - Close)/ATR = (100-98)/10 = 0.20 — what the closed-bar
	// rule gives up versus entering at the level.
	if math.Abs(found.SlipATR-0.20) > 1e-9 {
		t.Errorf("SlipATR = %v, want 0.20", found.SlipATR)
	}
}

// Mirror: poke BELOW and close back above is a reclaimed support -> long.
func TestDetectHitsLowerWickReclaim(t *testing.T) {
	cs := flat(61, 100)
	cs = append(cs, bar(61, 101, 103, 97, 102))
	atr := constATR(len(cs), 10)

	hits := detectHits(cs, atr, 0.0015, true, false)
	var found *levelHit
	for i := range hits {
		if hits[i].Bar == 61 && hits[i].Side == "long" {
			found = &hits[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no long hit on the poke-below bar; hits=%+v", hits)
	}
	if found.Wick != 97 {
		t.Errorf("Wick = %v, want the bar Low 97", found.Wick)
	}
	if math.Abs(found.DepthATR-0.30) > 1e-9 { // (100-97)/10
		t.Errorf("DepthATR = %v, want 0.30", found.DepthATR)
	}
	if math.Abs(found.SlipATR-0.20) > 1e-9 { // (102-100)/10
		t.Errorf("SlipATR = %v, want 0.20", found.SlipATR)
	}
}

// A bar that closes THROUGH the level is not a rejection — it is a break, and
// counting it would measure something else entirely.
func TestDetectHitsIgnoresCleanBreaks(t *testing.T) {
	cs := flat(61, 100)
	cs = append(cs, bar(61, 99, 105, 98.5, 104)) // opened below, closed above
	atr := constATR(len(cs), 10)
	for _, h := range detectHits(cs, atr, 0.0015, true, false) {
		if h.Bar == 61 {
			t.Errorf("a clean break through the level must not be a hit: %+v", h)
		}
	}
}

// A bar entirely on one side of the level never touched it.
func TestDetectHitsIgnoresNoTouch(t *testing.T) {
	cs := flat(61, 100)
	cs = append(cs, bar(61, 102, 104, 101, 103))
	atr := constATR(len(cs), 10)
	for _, h := range detectHits(cs, atr, 0.0015, true, false) {
		if h.Bar == 61 {
			t.Errorf("bar never reached the level: %+v", h)
		}
	}
}

// No lookahead: a level that only exists BECAUSE of the current bar must not
// be usable at that bar. Pools come from cs[:i], so a pool formed by bar i
// cannot generate a hit at bar i.
func TestDetectHitsNoLookahead(t *testing.T) {
	cs := flat(61, 100)
	// Two bars that between them create an EQH pool at ~110.
	cs = append(cs, bar(61, 100, 110, 99, 100))
	cs = append(cs, bar(62, 100, 110, 99, 100))
	atr := constATR(len(cs), 10)
	for _, h := range detectHits(cs, atr, 0.0015, false, true) { // pools only
		if h.Bar <= 62 && math.Abs(h.Level-110) < 1 {
			t.Errorf("hit at bar %d used a level its own bars created: %+v", h.Bar, h)
		}
	}
}

// A zero ATR bar cannot produce a depth in ATR terms; it must be skipped
// rather than emitting Inf/NaN into the statistics.
func TestDetectHitsSkipsZeroATR(t *testing.T) {
	cs := flat(61, 100)
	cs = append(cs, bar(61, 99, 103, 97, 98))
	atr := constATR(len(cs), 0)
	if hits := detectHits(cs, atr, 0.0015, true, false); len(hits) != 0 {
		t.Errorf("zero ATR must yield no hits (would be Inf depth), got %+v", hits)
	}
}

func TestMedian(t *testing.T) {
	if got := median(nil); got != 0 {
		t.Errorf("median(nil) = %v, want 0", got)
	}
	if got := median([]float64{5}); got != 5 {
		t.Errorf("single = %v", got)
	}
	if got := median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("odd = %v, want 2", got)
	}
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("even = %v, want 2.5", got)
	}
	// Must not reorder the caller's slice — the same slices are appended into
	// the aggregate distribution afterwards.
	in := []float64{3, 1, 2}
	_ = median(in)
	if in[0] != 3 {
		t.Errorf("median mutated its input: %v", in)
	}
}

func TestAlignRight(t *testing.T) {
	if got := alignRight([]float64{1, 2, 3}, 3); len(got) != 3 || got[0] != 1 {
		t.Errorf("exact = %v", got)
	}
	// Longer input keeps the RIGHT-hand (most recent) values.
	got := alignRight([]float64{1, 2, 3, 4, 5}, 3)
	if len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Errorf("trim = %v, want [3 4 5]", got)
	}
	// Shorter input is left-padded with zeros so index i still means bar i.
	got = alignRight([]float64{9}, 3)
	if len(got) != 3 || got[0] != 0 || got[1] != 0 || got[2] != 9 {
		t.Errorf("pad = %v, want [0 0 9]", got)
	}
}
