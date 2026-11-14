package main

import (
	"math/rand"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

func rw(seed int64, n int, dur time.Duration) []market.Candle {
	r := rand.New(rand.NewSource(seed))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px, drift := 100.0, 0.0
	cs := make([]market.Candle, n)
	for i := range cs {
		if i%150 == 0 {
			drift = (r.Float64() - 0.5) * 0.3
		}
		o := px
		px += drift + r.NormFloat64()*0.5
		hi := max(o, px) + r.Float64()*0.5
		lo := min(o, px) - r.Float64()*0.5
		ot := t0.Add(time.Duration(i) * dur)
		cs[i] = market.Candle{OpenTime: ot, CloseTime: ot.Add(dur), Open: o, High: hi, Low: lo, Close: px}
	}
	return cs
}

func fires(v variant) ([]autotrade.PaperFire, []market.Candle, []market.Candle, []float64, []float64) {
	cs := rw(3, 1200, time.Hour)
	htf := rw(4, 350, 4*time.Hour)
	closes := make([]float64, len(cs))
	for i, c := range cs {
		closes[i] = c.Close
	}
	ema := alignRight(indicator.EMA(closes, 20), len(cs))
	atr := alignRight(indicator.ATR(cs, 14), len(cs))
	return genFires(cs, htf, ema, atr, v, 0.005, 0.012, "T"), cs, htf, ema, atr
}

// Inert-param check: each veto flag must remove some V0 fires — and ONLY
// remove (a kept fire is byte-identical to the V0 fire at that bar, so stop
// and target are untouched).
func TestVetoFlagsChangeFireSet(t *testing.T) {
	base, _, _, _, _ := fires(variant{name: "V0"})
	if len(base) < 50 {
		t.Fatalf("fixture produced only %d fires", len(base))
	}
	in := map[autotrade.PaperFire]bool{}
	for _, f := range base {
		in[f] = true
	}
	for name, v := range map[string]variant{"trend-veto": {nfeVeto: true}, "htf-veto": {htfVeto: true}} {
		got, _, _, _, _ := fires(v)
		if len(got) == len(base) || len(got) == 0 {
			t.Errorf("%s kept %d of %d fires — inert or total", name, len(got), len(base))
		}
		for _, f := range got {
			if !in[f] {
				t.Errorf("%s produced a fire V0 does not have: %+v", name, f)
			}
		}
	}
}

// Look-ahead: generating on the series truncated at bar k (and HTF bars not
// closed by bar k's open removed) must reproduce the full run's fires up to k.
func TestVetoNoLookAhead(t *testing.T) {
	for name, v := range map[string]variant{"trend-veto": {nfeVeto: true}, "htf-veto": {htfVeto: true}} {
		full, cs, htf, ema, atr := fires(v)
		for k := 300; k < len(cs); k += 37 {
			var hcut []market.Candle
			for _, h := range htf {
				if !h.CloseTime.After(cs[k].OpenTime) {
					hcut = append(hcut, h)
				}
			}
			got := genFires(cs[:k+1], hcut, ema[:k+1], atr[:k+1], v, 0.005, 0.012, "T")
			var want []autotrade.PaperFire
			for _, f := range full {
				if !f.Time.After(cs[k].CloseTime) {
					want = append(want, f)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%s cut at %d: %d fires without the future, %d with it", name, k, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s cut at %d: fire %d differs", name, k, i)
				}
			}
		}
	}
}
