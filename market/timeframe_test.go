package market

import (
	"testing"
	"time"
)

func TestBarDuration(t *testing.T) {
	for _, c := range []struct {
		tf   Timeframe
		want time.Duration
	}{
		{TF1m, time.Minute},
		{TF5m, 5 * time.Minute},
		{TF15m, 15 * time.Minute},
		{TF30m, 30 * time.Minute},
		{TF1h, time.Hour},
		{TF2h, 2 * time.Hour},
		{TF4h, 4 * time.Hour},
		{TF1d, 24 * time.Hour},
		// Unknown must be 0, not a fallback: oi.PrevFor treats 0 as
		// "cannot answer" and stays silent, whereas a 1h fallback would
		// have it compare against the wrong span on a timeframe nobody
		// defined.
		{Timeframe("3h"), 0},
		{Timeframe(""), 0},
		{Timeframe("1w"), 0},
	} {
		if got := BarDuration(c.tf); got != c.want {
			t.Errorf("BarDuration(%q) = %v, want %v", c.tf, got, c.want)
		}
	}
}
