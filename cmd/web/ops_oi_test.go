package main

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/oi"
)

func oiRows(sym string, vals ...float64) []oi.Snapshot {
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	out := make([]oi.Snapshot, 0, len(vals))
	for i, v := range vals {
		out = append(out, oi.Snapshot{
			Time:   base.Add(time.Duration(i) * 5 * time.Minute),
			Symbol: sym,
			OI:     v,
		})
	}
	return out
}

func rep(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestOIIsStatic(t *testing.T) {
	cases := []struct {
		name  string
		snaps []oi.Snapshot
		sym   string
		want  bool
	}{
		{
			// The measured behaviour of the NCCO*/NCSK* synthetics: one value,
			// every sample, forever.
			name:  "constant with enough samples",
			snaps: oiRows("NCCOXAG2USD-USDT", rep(2199432, 8)...),
			sym:   "NCCOXAG2USD-USDT",
			want:  true,
		},
		{
			// A native perp: one change is enough to prove the feed is live.
			name:  "one differing sample is not static",
			snaps: oiRows("BTC-USDT", append(rep(1000, 7), 1001)...),
			sym:   "BTC-USDT",
			want:  false,
		},
		{
			// Quiet is not the same as constant. Below the evidence bar it
			// must refuse to claim, so a freshly started sampler does not
			// label every symbol n/a for its first half hour.
			name:  "constant but too few samples",
			snaps: oiRows("SUI-USDT", rep(31000000, 5)...),
			sym:   "SUI-USDT",
			want:  false,
		},
		{
			name:  "no samples for this symbol",
			snaps: oiRows("BTC-USDT", rep(1000, 8)...),
			sym:   "ETH-USDT",
			want:  false,
		},
		{
			name:  "empty store",
			snaps: nil,
			sym:   "BTC-USDT",
			want:  false,
		},
	}
	for _, c := range cases {
		if got := oiIsStatic(c.snaps, c.sym); got != c.want {
			t.Errorf("%s: oiIsStatic(%q) = %v, want %v", c.name, c.sym, got, c.want)
		}
	}
}

// Another symbol's movement must not make this one look live, and vice
// versa — the filter is per-symbol.
func TestOIIsStaticIgnoresOtherSymbols(t *testing.T) {
	snaps := append(
		oiRows("NCCOXAG2USD-USDT", rep(2199432, 8)...),
		oiRows("BTC-USDT", 1000, 1010, 1020, 1030, 1040, 1050, 1060, 1070)...,
	)
	if !oiIsStatic(snaps, "NCCOXAG2USD-USDT") {
		t.Error("XAG should still read static beside a moving BTC series")
	}
	if oiIsStatic(snaps, "BTC-USDT") {
		t.Error("BTC should still read live beside a static XAG series")
	}
}
