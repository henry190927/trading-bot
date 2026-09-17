package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
)

// chartLiquidityPools used to run its own Klines(sym, tf, 300) fetch — a call
// byte-identical to the one scanOne had already made in the same request. The
// duplicate was removed and the function made pure, which is what lets these
// tests exist at all: before, nothing here was reachable without a live
// exchange, and the package had zero coverage of it.
//
// The fetch was not pointless, though. It existed to guarantee a FIXED-SIZE
// input, because pools computed over different window lengths differ and the
// chart's lines would jump. That guarantee is now a caller contract, so
// TestChartLiquidityPools_WindowLengthChangesPools below pins the failure it
// prevents, and the parameter is a symbolView rather than a slice so the wrong
// series cannot be passed in the first place.

// liqFixture builds n hourly bars flat at 100 (high 100.5 / low 99.5) with
// deliberate, isolated extremes:
//
//	bars  40,  80  high 103.0  -> EQH pool  +3.0%  (inside the 5% window)
//	bars 120, 160  low   97.0  -> EQL pool  -3.0%  (inside the 5% window)
//	bars 200, 240  high 130.0  -> +30%, must be filtered out
//
// Extremes are isolated because a strength-2 swing pivot needs two lower bars
// on each side; adjacent equal highs register as neither.
func liqFixture(n int) []market.Candle {
	cs := make([]market.Candle, 0, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		c := market.Candle{
			OpenTime: base.Add(time.Duration(i) * time.Hour),
			Open:     100, High: 100.5, Low: 99.5, Close: 100, Volume: 10,
		}
		switch i {
		case 40, 80:
			c.High = 103.0
		case 120, 160:
			c.Low = 97.0
		case 200, 240:
			c.High = 130.0
		}
		cs = append(cs, c)
	}
	return cs
}

func findPool(pools []signal.LiquidityLevel, kind signal.LiquidityKind, price float64) *signal.LiquidityLevel {
	for i := range pools {
		if pools[i].Kind == kind && pools[i].Price == price {
			return &pools[i]
		}
	}
	return nil
}

func TestChartLiquidityPools_FindsBothSides(t *testing.T) {
	pools, px := chartLiquidityPools(symbolView{Candles: liqFixture(300)})

	if px != 100 {
		t.Fatalf("price = %v, want 100 (last close of the fixture)", px)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools, want 2 (EQH 103 + EQL 97); pools=%+v", len(pools), pools)
	}
	eqh := findPool(pools, signal.EQH, 103.0)
	if eqh == nil {
		t.Fatalf("EQH 103 missing; pools=%+v", pools)
	}
	if eqh.Touches != 2 {
		t.Errorf("EQH 103 touches = %d, want 2 (bars 40 and 80)", eqh.Touches)
	}
	eql := findPool(pools, signal.EQL, 97.0)
	if eql == nil {
		t.Fatalf("EQL 97 missing; pools=%+v", pools)
	}
	if eql.Touches != 2 {
		t.Errorf("EQL 97 touches = %d, want 2 (bars 120 and 160)", eql.Touches)
	}
}

// The +30% pair must not reach the chart: pools are filtered to within ~5% of
// price so the layer stays readable.
func TestChartLiquidityPools_FiltersPoolsBeyond5Pct(t *testing.T) {
	pools, _ := chartLiquidityPools(symbolView{Candles: liqFixture(300)})
	if p := findPool(pools, signal.EQH, 130.0); p != nil {
		t.Errorf("EQH 130 (+30%%) should have been filtered out, got %+v", *p)
	}
}

// capFixture adds six more EQH pairs between +0.6% and +2.1%, isolated and
// >0.15% apart so each clusters separately. With the fixture's own 103 pair
// that is seven EQH pools inside the 5% window, against a cap of four a side.
func capFixture() []market.Candle {
	cs := liqFixture(300)
	for k, v := range []float64{100.6, 100.9, 101.2, 101.5, 101.8, 102.1} {
		cs[10+k*40].High = v
		cs[30+k*40].High = v
	}
	return cs
}

func TestChartLiquidityPools_CapsFourPerSide(t *testing.T) {
	pools, px := chartLiquidityPools(symbolView{Candles: capFixture()})

	var above, below int
	for _, p := range pools {
		if p.Price >= px {
			above++
		} else {
			below++
		}
	}
	if above != 4 {
		t.Errorf("pools above price = %d, want 4 (the cap); pools=%+v", above, pools)
	}
	if below != 1 {
		t.Errorf("pools below price = %d, want 1 (only EQL 97); pools=%+v", below, pools)
	}
	// The four kept must be the four NEAREST, not an arbitrary four.
	for _, want := range []float64{100.6, 100.9, 101.2, 101.5} {
		if findPool(pools, signal.EQH, want) == nil {
			t.Errorf("EQH %.1f is among the four nearest and should be kept; pools=%+v", want, pools)
		}
	}
	for _, notWant := range []float64{101.8, 102.1, 103.0} {
		if p := findPool(pools, signal.EQH, notWant); p != nil {
			t.Errorf("EQH %.1f is farther than the four nearest and should be dropped, got %+v", notWant, *p)
		}
	}
}

func TestChartLiquidityPools_ShortSeriesReturnsNil(t *testing.T) {
	pools, px := chartLiquidityPools(symbolView{Candles: liqFixture(29)})
	if pools != nil || px != 0 {
		t.Errorf("29 bars: got (%+v, %v), want (nil, 0) — below the 30-bar floor", pools, px)
	}
	pools, px = chartLiquidityPools(symbolView{})
	if pools != nil || px != 0 {
		t.Errorf("nil series: got (%+v, %v), want (nil, 0)", pools, px)
	}
}

// The caller contract, pinned.
//
// The same series truncated to a shorter window yields a DIFFERENT pool set:
// at 150 bars the equal lows at bars 120 and 160 are only half present, so the
// EQL pool vanishes while the EQH pool survives. Feed this function the
// handler's display candles — whose length is whatever ?limit= asked for — and
// the liquidity lines would appear and disappear as the chart reloaded at
// different limits. That is why the parameter is a symbolView: its Candles are
// always scanOne's fixed Klines(sym, tf, 300).
func TestChartLiquidityPools_WindowLengthChangesPools(t *testing.T) {
	full, _ := chartLiquidityPools(symbolView{Candles: liqFixture(300)})
	short, _ := chartLiquidityPools(symbolView{Candles: liqFixture(150)})

	if findPool(full, signal.EQL, 97.0) == nil {
		t.Fatal("precondition: EQL 97 must be present at 300 bars")
	}
	if findPool(short, signal.EQL, 97.0) != nil {
		t.Error("precondition: EQL 97 must be absent at 150 bars (bar 160 is out of range)")
	}
	if len(full) == len(short) {
		t.Errorf("window length must change the pool set, got %d at both 300 and 150 bars", len(full))
	}
}
