package bingx

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

func TestDropForming(t *testing.T) {
	now := time.Date(2026, 5, 28, 19, 0, 5, 0, time.UTC) // 5s past 19:00 UTC

	mk := func(ct time.Time) market.Candle {
		return market.Candle{OpenTime: ct.Add(-time.Hour), CloseTime: ct}
	}

	tests := []struct {
		name          string
		in            []market.Candle
		wantLen       int
		wantLastClose time.Time // zero value means "don't check"
	}{
		{
			name:    "empty slice returns empty",
			in:      nil,
			wantLen: 0,
		},
		{
			name: "single forming candle is trimmed to empty",
			in: []market.Candle{
				mk(now.Add(55 * time.Minute)), // closes in the future
			},
			wantLen: 0,
		},
		{
			name: "single closed candle kept",
			in: []market.Candle{
				mk(now.Add(-1 * time.Second)),
			},
			wantLen:       1,
			wantLastClose: now.Add(-1 * time.Second),
		},
		{
			name: "trailing forming bar dropped, prior bars kept",
			in: []market.Candle{
				mk(now.Add(-2 * time.Hour)),   // closed
				mk(now.Add(-1 * time.Hour)),   // closed
				mk(now.Add(55 * time.Minute)), // forming
			},
			wantLen:       2,
			wantLastClose: now.Add(-1 * time.Hour),
		},
		{
			name: "all bars closed → no-op",
			in: []market.Candle{
				mk(now.Add(-3 * time.Hour)),
				mk(now.Add(-2 * time.Hour)),
				mk(now.Add(-1 * time.Hour)),
			},
			wantLen:       3,
			wantLastClose: now.Add(-1 * time.Hour),
		},
		{
			name: "bar closing exactly at `now` is kept (boundary)",
			in: []market.Candle{
				mk(now.Add(-1 * time.Hour)),
				mk(now), // CloseTime == now → After(now) is false → keep
			},
			wantLen:       2,
			wantLastClose: now,
		},
		{
			name: "bar closing one nanosecond past `now` is dropped",
			in: []market.Candle{
				mk(now.Add(-1 * time.Hour)),
				mk(now.Add(1 * time.Nanosecond)),
			},
			wantLen:       1,
			wantLastClose: now.Add(-1 * time.Hour),
		},
		{
			name: "two trailing forming bars: only one trimmed (defensive, malformed input)",
			in: []market.Candle{
				mk(now.Add(-1 * time.Hour)),
				mk(now.Add(30 * time.Minute)),
				mk(now.Add(90 * time.Minute)),
			},
			wantLen: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dropForming(tc.in, now)
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d (got: %+v)", len(got), tc.wantLen, got)
			}
			if !tc.wantLastClose.IsZero() && len(got) > 0 {
				last := got[len(got)-1].CloseTime
				if !last.Equal(tc.wantLastClose) {
					t.Errorf("last CloseTime = %v, want %v", last, tc.wantLastClose)
				}
			}
		})
	}
}

// TestDropForming_DoesNotMutate guards against the helper aliasing the input.
func TestDropForming_DoesNotMutate(t *testing.T) {
	now := time.Date(2026, 5, 28, 19, 0, 0, 0, time.UTC)
	in := []market.Candle{
		{CloseTime: now.Add(-time.Hour)},
		{CloseTime: now.Add(time.Hour)}, // forming
	}
	origLen := len(in)
	origLast := in[len(in)-1].CloseTime
	_ = dropForming(in, now)
	if len(in) != origLen || !in[origLen-1].CloseTime.Equal(origLast) {
		t.Errorf("dropForming mutated input slice")
	}
}
