package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// Hourly bars opening at h0+from·1h .. h0+(to-1)·1h. Close tags where a bar
// came from: 1 = cached, 2 = recent.
func hrs(from, to int, tag float64) []market.Candle {
	h0 := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	var cs []market.Candle
	for i := from; i < to; i++ {
		t := h0.Add(time.Duration(i) * time.Hour)
		cs = append(cs, market.Candle{OpenTime: t, CloseTime: t.Add(time.Hour), Close: tag})
	}
	return cs
}

func TestMergeCandles(t *testing.T) {
	cases := []struct {
		name           string
		cached, recent []market.Candle
		wantOK         bool
		wantLen        int
		firstRecent    int // index in merged where recent's bars start
	}{
		// cached 0..9, recent 7..12: keep cached 0..6 (7 bars), then 6 recent
		{"overlap: recent wins from its first bar", hrs(0, 10, 1), hrs(7, 13, 2), true, 13, 7},
		// cached 0..9, recent 10..12: all 10 cached + 3 recent
		{"exactly one bar after: meets", hrs(0, 10, 1), hrs(10, 13, 2), true, 13, 10},
		// cached 0..9, recent 11..13: bar 10 is missing
		{"gap of one bar: does not meet", hrs(0, 10, 1), hrs(11, 14, 2), false, 0, 0},
		// cached 5..9, recent 3..12: no cached bar opens before 3
		{"recent reaches back past the cache", hrs(5, 10, 1), hrs(3, 13, 2), true, 10, 0},
		// cached 0..9, recent 9..9: only the forming last bar is replaced
		{"only the forming bar is refreshed", hrs(0, 10, 1), hrs(9, 10, 2), true, 10, 9},
	}
	for _, c := range cases {
		got, ok := mergeCandles(c.cached, c.recent, time.Hour)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != c.wantLen {
			t.Errorf("%s: %d bars, want %d", c.name, len(got), c.wantLen)
			continue
		}
		for i := 1; i < len(got); i++ {
			if got[i].OpenTime.Sub(got[i-1].OpenTime) != time.Hour {
				t.Errorf("%s: bars %d→%d are not one hour apart", c.name, i-1, i)
			}
		}
		for i, b := range got {
			want := 1.0
			if i >= c.firstRecent {
				want = 2
			}
			if b.Close != want {
				t.Errorf("%s: bar %d came from the wrong side (Close %v, want %v)", c.name, i, b.Close, want)
				break
			}
		}
	}
}

// The cache's slice may have room past its length. Writing the merge into it
// would rewrite bars another request is still replaying on.
func TestMergeCandlesLeavesCachedAlone(t *testing.T) {
	cached := make([]market.Candle, 0, 32)
	cached = append(cached, hrs(0, 10, 1)...)
	if _, ok := mergeCandles(cached, hrs(7, 13, 2), time.Hour); !ok {
		t.Fatal("overlap should merge")
	}
	for i, b := range cached {
		if b.Close != 1 {
			t.Fatalf("cached[%d] was overwritten (Close %v)", i, b.Close)
		}
	}
	if spare := cached[:cap(cached)][10]; spare.Close != 0 {
		t.Fatalf("cached's spare capacity was written into (Close %v)", spare.Close)
	}
}
