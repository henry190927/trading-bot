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

func TestOIQuadrant(t *testing.T) {
	cases := []struct {
		name        string
		oiD, priceD float64
		wantKey     string
	}{
		// The bug this function exists to fix. The first version of the card
		// called any OI rise "shorts crowding"; ETH ran OI up while price
		// rose, which is longs being added.
		{"OI up, price up = longs", 0.025, +0.005, "longs-building"},
		{"OI up, price down = shorts", 0.050, -0.005, "shorts-building"},
		// SUI and HYPE on 2026-09-11: OI +5.0% / +4.8% into three red bars.
		{"OI up hard, price down hard", 0.0502, -0.013, "shorts-building"},
		// A 1.9% OI move against a -0.06% drift is not evidence of a side.
		{"OI up, price flat = indeterminate", 0.030, -0.0004, "new-positions"},
		{"OI down, price down = long unwind", -0.046, -0.007, "longs-unwinding"},
		{"OI down, price up = shorts covering", -0.046, +0.007, "shorts-covering"},
		{"OI down, price flat", -0.046, 0.0, "closing"},
		// Below the OI threshold nothing is claimed, however big the move.
		{"OI inside threshold", 0.019, -0.05, ""},
		{"OI inside threshold negative", -0.019, +0.05, ""},
		// Boundaries are inclusive on the OI side and on the noise floor.
		{"exactly at OI threshold", 0.02, -0.001, "shorts-building"},
		{"exactly at noise floor up", 0.02, +0.001, "longs-building"},
	}
	for _, c := range cases {
		key, label := oiQuadrant(c.oiD, c.priceD)
		if key != c.wantKey {
			t.Errorf("%s: oiQuadrant(%.4f, %.4f) = %q, want %q",
				c.name, c.oiD, c.priceD, key, c.wantKey)
		}
		if (key == "") != (label == "") {
			t.Errorf("%s: key %q and label %q must both be set or both empty", c.name, key, label)
		}
	}
}
