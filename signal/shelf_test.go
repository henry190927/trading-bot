package signal

import (
	"math"
	"testing"

	"github.com/henry190927/trading-bot/market"
)

// narrowBars turns a price path into candles with High == Low == price, so the
// swing fractal is unambiguous: a pivot is exactly a strict local extreme of
// the path. FindSwingPoints requires STRICT dominance (`>=` disqualifies a
// top), so equal neighbours produce no pivot at all — which is why the
// fixtures below use distinct values rather than a tidy round-number zigzag.
func narrowBars(path []float64) []market.Candle {
	cs := make([]market.Candle, len(path))
	for i, p := range path {
		cs[i] = market.Candle{Open: p, High: p, Low: p, Close: p}
	}
	return cs
}

// A high at 105 (idx 2) and a low at 105 (idx 8): price pivoted around the
// same band in both directions.
var shelfPath = []float64{90, 95, 105, 97, 92, 100, 118, 110, 105, 112, 125, 120, 130, 128, 135}

// A skipping test is not a test — assert the fixture still produces the pivots
// the expectations below are built on, so a change to the fractal rule fails
// loudly here instead of quietly turning the shelf assertions into vacuous
// checks on an empty slice.
func TestShelfFixtureProducesBothSidedPivots(t *testing.T) {
	pts := FindSwingPoints(narrowBars(shelfPath), 2, 0)
	if len(pts) != 4 {
		t.Fatalf("fixture produced %d pivots, want 4 — expectations below are stale", len(pts))
	}
	want := []struct {
		idx   int
		price float64
		isTop bool
	}{{2, 105, true}, {4, 92, false}, {6, 118, true}, {8, 105, false}}
	for i, w := range want {
		if pts[i].Index != w.idx || pts[i].Price != w.price || pts[i].IsTop != w.isTop {
			t.Errorf("pivot %d = {idx %d, %.0f, top %v}, want {idx %d, %.0f, top %v}",
				i, pts[i].Index, pts[i].Price, pts[i].IsTop, w.idx, w.price, w.isTop)
		}
	}
}

func TestFindShelvesRequiresBothSides(t *testing.T) {
	got := FindShelves(narrowBars(shelfPath), 2, 0, 0.0015, 2)
	if len(got) != 1 {
		t.Fatalf("got %d shelves, want 1 (only the 105 band has a high AND a low): %+v", len(got), got)
	}
	s := got[0]
	if math.Abs(s.Price-105) > 1e-9 {
		t.Errorf("Price = %v, want 105", s.Price)
	}
	if s.Highs != 1 || s.Lows != 1 {
		t.Errorf("Highs/Lows = %d/%d, want 1/1", s.Highs, s.Lows)
	}
	if s.Touches() != 2 {
		t.Errorf("Touches() = %d, want 2", s.Touches())
	}
	if s.FirstIdx != 2 || s.LastIdx != 8 {
		t.Errorf("FirstIdx/LastIdx = %d/%d, want 2/8", s.FirstIdx, s.LastIdx)
	}
	if s.Span() != 6 {
		t.Errorf("Span() = %d, want 6", s.Span())
	}
	// The 92 low and the 118 high are single-sided and must NOT appear.
	for _, sh := range got {
		if sh.Price == 92 || sh.Price == 118 {
			t.Errorf("single-sided cluster %v leaked in — that is an EQH/EQL pool, not a shelf", sh.Price)
		}
	}
}

// THE discriminator against FindLiquidity. A textbook range — every high at
// one price, every low at another — is a pair of rich EQH/EQL pools and
// contains no shelf at all. If this ever returns something, FindShelves has
// degenerated into FindLiquidity with the Kind field discarded.
func TestFindShelvesIgnoresPureOneSidedPools(t *testing.T) {
	var path []float64
	for k := 0; k < 5; k++ {
		d := float64(k) * 0.001
		path = append(path, 100+d, 105, 110-d, 105, 100+d)
	}
	cs := narrowBars(path)

	pools := FindLiquidity(cs, 2, 0, 0.0015)
	if len(pools) < 2 {
		t.Fatalf("precondition: this range should be full of EQH/EQL pools, got %d", len(pools))
	}
	if got := FindShelves(cs, 2, 0, 0.0015, 2); len(got) != 0 {
		t.Errorf("got %d shelves in a pure two-sided range, want 0: %+v", len(got), got)
	}
}

func TestFindShelvesHonoursMinTouches(t *testing.T) {
	cs := narrowBars(shelfPath)
	// The 105 band has exactly 2 touches, so 3 must exclude it.
	if got := FindShelves(cs, 2, 0, 0.0015, 3); len(got) != 0 {
		t.Errorf("minTouches=3 should exclude a 2-touch shelf, got %+v", got)
	}
	// Below 2 is meaningless (a both-sided band is already 2) and is raised.
	if got := FindShelves(cs, 2, 0, 0.0015, 0); len(got) != 1 {
		t.Errorf("minTouches=0 should behave as 2, got %d", len(got))
	}
}

func TestFindShelvesGuards(t *testing.T) {
	cs := narrowBars(shelfPath)
	if got := FindShelves(cs, 2, 0, 0, 2); got != nil {
		t.Errorf("tolPct 0 must return nil, got %+v", got)
	}
	if got := FindShelves(cs, 2, 0, -1, 2); got != nil {
		t.Errorf("negative tolPct must return nil, got %+v", got)
	}
	if got := FindShelves(nil, 2, 0, 0.0015, 2); got != nil {
		t.Errorf("nil candles must return nil, got %+v", got)
	}
	if got := FindShelves(narrowBars([]float64{1, 2, 3}), 2, 0, 0.0015, 2); got != nil {
		t.Errorf("too few candles for a fractal must return nil, got %+v", got)
	}
}

// Above/below are measured against the band EDGES. A price INSIDE a shelf is
// neither above nor below it — reporting that shelf as a target would aim at
// the level price is already standing on.
func TestNearestShelvesUsesBandEdges(t *testing.T) {
	shelves := []Shelf{
		{Price: 100, Lo: 99, Hi: 101, Highs: 1, Lows: 1},
		{Price: 110, Lo: 109, Hi: 111, Highs: 1, Lows: 1},
		{Price: 120, Lo: 119, Hi: 121, Highs: 1, Lows: 1},
	}

	above, below := NearestShelves(shelves, 105)
	if above == nil || above.Price != 110 {
		t.Errorf("above 105 = %v, want the 110 shelf", above)
	}
	if below == nil || below.Price != 100 {
		t.Errorf("below 105 = %v, want the 100 shelf", below)
	}

	// Standing inside the 110 band: it is neither, so the answers skip to
	// the neighbours.
	above, below = NearestShelves(shelves, 110)
	if above == nil || above.Price != 120 {
		t.Errorf("inside the 110 band, above = %v, want the 120 shelf", above)
	}
	if below == nil || below.Price != 100 {
		t.Errorf("inside the 110 band, below = %v, want the 100 shelf", below)
	}

	// Off both ends.
	if a, b := NearestShelves(shelves, 5); a == nil || a.Price != 100 || b != nil {
		t.Errorf("below everything: above=%v below=%v", a, b)
	}
	if a, b := NearestShelves(shelves, 500); a != nil || b == nil || b.Price != 120 {
		t.Errorf("above everything: above=%v below=%v", a, b)
	}
	if a, b := NearestShelves(nil, 100); a != nil || b != nil {
		t.Error("empty input must give nil, nil")
	}
}

// ---------------------------------------------------------------------
// TopShelves
// ---------------------------------------------------------------------

func sh(price, lo, hi float64, highs, lows, first, last int) Shelf {
	return Shelf{Price: price, Lo: lo, Hi: hi, Highs: highs, Lows: lows, FirstIdx: first, LastIdx: last}
}

// Two-sidedness outranks raw touch count, because two-sidedness is the only
// thing separating a shelf from an EQH pool. H2/L2 (4 touches) must beat
// H5/L1 (6 touches): the latter is a liquidity pool wearing one low.
func TestTopShelvesRanksTwoSidednessOverTouchCount(t *testing.T) {
	in := []Shelf{
		sh(101, 100.5, 101.5, 5, 1, 0, 300), // min 1, 6 touches, long span
		sh(102, 101.5, 102.5, 2, 2, 0, 10),  // min 2, 4 touches, short span
	}
	got := TopShelves(in, 100, 0.10, 0)
	if len(got) != 2 {
		t.Fatalf("got %d, want both to survive the gate", len(got))
	}
	// Output is price-ordered, so check the ranking by taking only the top 1.
	top := TopShelves(in, 100, 0.10, 1)
	if len(top) != 1 || top[0].Price != 102 {
		t.Errorf("top pick = %+v, want the H2/L2 band at 102", top)
	}
}

func TestTopShelvesTieBreaksOnTouchesThenSpan(t *testing.T) {
	// Same min(H,L)=2; 5 touches beats 4.
	byTouches := TopShelves([]Shelf{
		sh(101, 100.9, 101.1, 2, 2, 0, 400),
		sh(102, 101.9, 102.1, 3, 2, 0, 10),
	}, 100, 0.10, 1)
	if byTouches[0].Price != 102 {
		t.Errorf("got %v, want 102 (5 touches beats 4 at equal two-sidedness)", byTouches[0].Price)
	}
	// Same min AND same touches; longer span wins.
	bySpan := TopShelves([]Shelf{
		sh(101, 100.9, 101.1, 2, 2, 0, 20),
		sh(102, 101.9, 102.1, 2, 2, 0, 400),
	}, 100, 0.10, 1)
	if bySpan[0].Price != 102 {
		t.Errorf("got %v, want 102 (span 400 beats span 20)", bySpan[0].Price)
	}
}

// The gate is what makes the layer drawable: on real BTC data raw detection
// returned a band 20% away with a 16-bar span, which is not support or
// resistance for any decision at the current price.
func TestTopShelvesDropsDistantBands(t *testing.T) {
	in := []Shelf{
		sh(100.5, 100, 101, 2, 2, 0, 100), // touching price
		sh(105, 104, 106, 3, 3, 0, 100),   // +4% from 100 → edge distance 4%
		sh(120, 119, 121, 9, 9, 0, 999),   // +19% → dropped despite being huge
	}
	got := TopShelves(in, 100, 0.05, 0)
	if len(got) != 2 {
		t.Fatalf("got %d bands, want 2 (the 120 band is beyond 5%%): %+v", len(got), got)
	}
	for _, s := range got {
		if s.Price == 120 {
			t.Error("the 19%-away band survived the gate")
		}
	}
}

// Distance is to the nearest EDGE, so a wide band the price sits just outside
// is not penalised for how far its far side reaches.
func TestTopShelvesMeasuresDistanceToEdge(t *testing.T) {
	// Band [101, 130] with price 100: edge distance is 1%, not 15% (midpoint).
	wide := sh(115.5, 101, 130, 2, 2, 0, 100)
	if got := TopShelves([]Shelf{wide}, 100, 0.05, 0); len(got) != 1 {
		t.Errorf("a band whose near edge is 1%% away must survive a 5%% gate, got %+v", got)
	}
	// A price INSIDE the band is distance 0.
	if got := TopShelves([]Shelf{wide}, 110, 0.001, 0); len(got) != 1 {
		t.Errorf("price inside the band is distance 0, got %+v", got)
	}
}

func TestTopShelvesGuardsAndOrdering(t *testing.T) {
	in := []Shelf{
		sh(103, 102.9, 103.1, 3, 3, 0, 100),
		sh(101, 100.9, 101.1, 2, 2, 0, 100),
		sh(102, 101.9, 102.1, 4, 4, 0, 100),
	}
	got := TopShelves(in, 100, 0.10, 0)
	for i := 1; i < len(got); i++ {
		if got[i-1].Price > got[i].Price {
			t.Errorf("output must be price-ordered for drawing, got %v then %v", got[i-1].Price, got[i].Price)
		}
	}
	// maxDistPct 0 disables the gate rather than dropping everything.
	if len(TopShelves(in, 100, 0, 0)) != 3 {
		t.Error("maxDistPct 0 should mean no gate")
	}
	if TopShelves(in, 0, 0.05, 0) != nil {
		t.Error("price 0 must return nil")
	}
	if TopShelves(nil, 100, 0.05, 0) != nil {
		t.Error("nil input must return nil")
	}
}
