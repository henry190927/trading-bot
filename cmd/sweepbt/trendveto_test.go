package main

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// rw is a deterministic random walk of `dur` bars with regime drift.
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

func fixtureFires(t *testing.T) ([]market.Candle, []autotrade.PaperFire) {
	cs := rw(3, 1500, time.Hour)
	atr := alignRight(indicator.ATR(cs, 14), len(cs))
	fires := genSweepFires(cs, atr, "T", 0.0015, 0.15, 2.0, false, false)
	if len(fires) < 20 {
		t.Fatalf("fixture produced only %d fires", len(fires))
	}
	return cs, fires
}

// Inert-param check: -trend-veto and -htf-veto must each change the fire set
// on a fixture, and only by removing fires (never altering stop/target).
func TestTrendVetoChangesFireSet(t *testing.T) {
	cs, fires := fixtureFires(t)
	htf := rw(4, 400, 4*time.Hour)
	for name, veto := range map[string]vetoFn{"same-TF": sameTFVeto(cs), "4h": htfVeto(cs, htf)} {
		kept := applyTrendVeto(fires, veto)
		if len(kept) == len(fires) || len(kept) == 0 {
			t.Errorf("%s veto kept %d of %d fires — inert or total, the A/B would measure nothing", name, len(kept), len(fires))
		}
		all := map[autotrade.PaperFire]bool{}
		for _, f := range fires {
			all[f] = true
		}
		for _, f := range kept {
			if !all[f] {
				t.Errorf("%s veto produced a fire not in the input (stop/target altered?): %+v", name, f)
			}
		}
	}
	if got := applyTrendVeto(fires, func(autotrade.PaperFire) bool { return false }); !reflect.DeepEqual(got, fires) {
		t.Error("a veto that kills nothing must return the fires unchanged")
	}
}

// Look-ahead: the set of fires the veto keeps up to bar k must not depend on
// any bar after k. Truncate the series at k (so future bars do not exist)
// and the kept fires at or before bar k must be identical to the full run.
func TestTrendVetoNoLookAhead(t *testing.T) {
	cs, fires := fixtureFires(t)
	htf := rw(4, 400, 4*time.Hour)
	full := applyTrendVeto(fires, sameTFVeto(cs))
	fullH := applyTrendVeto(fires, htfVeto(cs, htf))
	upTo := func(fs []autotrade.PaperFire, t time.Time) []autotrade.PaperFire {
		var out []autotrade.PaperFire
		for _, f := range fs {
			if !f.Time.After(t) {
				out = append(out, f)
			}
		}
		return out
	}
	// Cut AT each fire's own bar: a read of even one bar past the fire then
	// meets a missing bar, and the decision changes.
	idx := map[time.Time]int{}
	for i, c := range cs {
		idx[c.CloseTime] = i
	}
	for _, f := range fires {
		k := idx[f.Time]
		cut := cs[k].CloseTime
		pre := upTo(fires, cut)
		if got, want := applyTrendVeto(pre, sameTFVeto(cs[:k+1])), upTo(full, cut); !reflect.DeepEqual(got, want) {
			t.Errorf("same-TF, cut at bar %d: %d kept with the future removed vs %d with it — future leak", k, len(got), len(want))
		}
		// HTF: drop every 4h bar that has not closed by the cut bar's open.
		var hcut []market.Candle
		for _, h := range htf {
			if !h.CloseTime.After(cs[k].OpenTime) {
				hcut = append(hcut, h)
			}
		}
		if got, want := applyTrendVeto(pre, htfVeto(cs[:k+1], hcut)), upTo(fullH, cut); !reflect.DeepEqual(got, want) {
			t.Errorf("4h, cut at bar %d: %d kept with unclosed HTF bars removed vs %d with them — future leak", k, len(got), len(want))
		}
	}
}

func TestScoreFiresFeeAndNoDedup(t *testing.T) {
	cs, fires := fixtureFires(t)
	gross := scoreFires(fires, cs, false, 0)
	net := scoreFires(fires, cs, false, 6)
	if len(gross) != len(net) {
		t.Fatalf("fee changed the dedup'd book: %d vs %d positions", len(gross), len(net))
	}
	var g, n float64
	for i := range gross {
		g += gross[i].Outcome.NetR
		n += net[i].Outcome.NetR
	}
	if !(n < g) {
		t.Errorf("6bp fee did not reduce netR: gross %.3f net %.3f", g, n)
	}
	if nd := scoreFires(fires, cs, true, 0); len(nd) != len(fires) || len(nd) <= len(gross) {
		t.Errorf("no-dedup scored %d of %d fires (dedup %d)", len(nd), len(fires), len(gross))
	}
}
