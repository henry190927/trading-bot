package indicator

import (
	"math"
	"testing"
)

// vpFrom builds a profile whose bin i spans [100+i, 101+i), mid 100.5+i.
func vpFrom(vols ...float64) VolumeProfile {
	vp := VolumeProfile{}
	for i, v := range vols {
		lo := 100 + float64(i)
		vp.Bins = append(vp.Bins, VolumeBin{PriceLow: lo, PriceHigh: lo + 1, PriceMid: lo + 0.5, Volume: v})
	}
	return vp
}

func raw() NodeOpts {
	o := DefaultNodeOpts()
	o.Smooth = 1
	return o
}

func assertNodes(t *testing.T, got, want []Node) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d nodes %+v, want %d %+v", len(got), got, len(want), want)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	for i := range want {
		g, w := got[i], want[i]
		if !near(g.Low, w.Low) || !near(g.High, w.High) || !near(g.POC, w.POC) || !near(g.Volume, w.Volume) {
			t.Errorf("node %d = %+v, want %+v", i, g, w)
		}
	}
}

// Two clearly separate mountains: peaks at bins 3 (10) and 9 (8), valley 0 at bin 6.
//
//	bin 3, E 4.0:   left 2 (5) in, 1 (1) out · right 4 (6) in, 5 (1) out → bins 2..4
//	bin 9, E 3.2:   left 8 (2) out · right 10 (4) in, 11 (0) out           → bins 9..10
func TestFindNodesTwoMountains(t *testing.T) {
	vp := vpFrom(0, 1, 5, 10, 6, 1, 0, 0.5, 2, 8, 4, 0)
	assertNodes(t, FindNodes(vp, raw()), []Node{
		{Low: 102, High: 105, POC: 103.5, Volume: 21},
		{Low: 109, High: 111, POC: 109.5, Volume: 12},
	})
}

// One mountain, two peaks: peaks at bins 2 (8) and 5 (9), valley 6 at bin 3.
// 6 / min(8, 9) = 0.75 >= 0.5 → one node. Edges: bin 1 (2) < 0.4×8, bin 6 (3) < 0.4×9.
func TestFindNodesTwoPeaksOneMountain(t *testing.T) {
	vp := vpFrom(0, 2, 8, 6, 7, 9, 3, 0)
	assertNodes(t, FindNodes(vp, raw()), []Node{
		{Low: 102, High: 106, POC: 105.5, Volume: 30},
	})
}

// The same shape on both sides of D = 0.5; only the valley bin changes.
func TestFindNodesValleyDepthThreshold(t *testing.T) {
	// valley 6 / min(10, 9) = 0.667 → not deep enough → merged into one node.
	assertNodes(t, FindNodes(vpFrom(0, 10, 6, 9, 0), raw()), []Node{
		{Low: 101, High: 104, POC: 101.5, Volume: 25},
	})
	// valley 4 / 9 = 0.444 → deep enough → two nodes.
	// Bin 2 (4) passes 0.4×10, but it is the shared valley, so neither node takes it.
	assertNodes(t, FindNodes(vpFrom(0, 10, 4, 9, 0), raw()), []Node{
		{Low: 101, High: 102, POC: 101.5, Volume: 10},
		{Low: 103, High: 104, POC: 103.5, Volume: 9},
	})
}

// A local bump below ProminencePct × max is not a peak: bin 4 (2) < 0.25×10.
func TestFindNodesIgnoresSmallBump(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(0, 10, 2, 1, 2, 0), raw()), []Node{
		{Low: 101, High: 102, POC: 101.5, Volume: 10},
	})
}

// The first and last bin can be peaks (outside the profile counts as -Inf).
//
//	bin 0 (9), E 3.6: right 1 (4) in, 2 is the valley → bins 0..1
//	bin 5 (8), E 3.2: left 4 (5) in, 3 (1) out         → bins 4..5
func TestFindNodesPeaksAtProfileEdges(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(9, 4, 0, 1, 5, 8), raw()), []Node{
		{Low: 100, High: 102, POC: 100.5, Volume: 13},
		{Low: 104, High: 106, POC: 105.5, Volume: 13},
	})
}

// One noisy bin (2 at bin 4) splits a mountain in raw form; Smooth 3 heals it.
//
//	raw:      0    4    8    10   2    9    8    4    0
//	smoothed: 2    4    7.33 6.67 7    6.33 7    4    2
//
// Smoothed peaks 2, 4, 6; valleys 6.67/7 and 6.33/7 are both >= 0.5 → one node.
// Edges: bin 1 (4) >= 0.4×7.33, bin 0 (2) out; bin 7 (4) >= 0.4×7, bin 8 (2) out.
// POC and Volume come from raw volumes: bin 3 (10), sum of bins 1..7 = 45.
func TestFindNodesSmoothingMergesNoise(t *testing.T) {
	vols := []float64{0, 4, 8, 10, 2, 9, 8, 4, 0}

	assertNodes(t, FindNodes(vpFrom(vols...), DefaultNodeOpts()), []Node{
		{Low: 101, High: 108, POC: 103.5, Volume: 45},
	})

	// Without smoothing the valley 2 / min(10, 9) = 0.22 splits it in two.
	assertNodes(t, FindNodes(vpFrom(vols...), raw()), []Node{
		{Low: 101, High: 104, POC: 103.5, Volume: 22},
		{Low: 105, High: 108, POC: 105.5, Volume: 21},
	})
}

func TestFindNodesEmpty(t *testing.T) {
	if got := FindNodes(VolumeProfile{}, DefaultNodeOpts()); len(got) != 0 {
		t.Errorf("empty profile: got %+v, want none", got)
	}
	if got := FindNodes(vpFrom(0, 0, 0), DefaultNodeOpts()); len(got) != 0 {
		t.Errorf("zero volume: got %+v, want none", got)
	}
}

// The valley is compared with the LOWER of the two peaks: 3 / min(10, 4) = 0.75 → merged.
// (Against the higher peak it would be 0.3 and split, which is the wrong rule.)
func TestFindNodesValleyUsesLowerPeak(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(0, 10, 3, 4, 0), raw()), []Node{
		{Low: 101, High: 104, POC: 101.5, Volume: 17},
	})
}

// Each side extends against ITS OWN end peak. Peaks 1 (10) and 3 (5) merge
// (valley 4 / 5 = 0.8). Going right from bin 3: bin 4 (2) >= 0.4×5 = 2 → in.
// (Against the left peak, 0.4×10 = 4, bin 4 would be wrongly left out.)
func TestFindNodesEdgeUsesEndPeak(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(0, 10, 4, 5, 2, 0), raw()), []Node{
		{Low: 101, High: 105, POC: 101.5, Volume: 21},
	})
}

// A flat top: bins 1 and 2 both hold 5. The rule (>= left, > right) makes
// bin 2 the peak, and the node still covers both bins.
func TestFindNodesFlatTop(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(0, 5, 5, 0), raw()), []Node{
		{Low: 101, High: 103, POC: 101.5, Volume: 10},
	})
}

// Edge bins average only the bins that exist.
//
//	raw:      9   9   0   0   0
//	smoothed: 9   6   3   0   0     (bin 0 = (9+9)/2, not (9+9)/3)
//
// Peak at bin 0, E 3.6: bin 1 (6) in, bin 2 (3) out. POC tie 9/9 → lowest index.
func TestFindNodesSmoothingAtProfileEdge(t *testing.T) {
	assertNodes(t, FindNodes(vpFrom(9, 9, 0, 0, 0), DefaultNodeOpts()), []Node{
		{Low: 100, High: 102, POC: 100.5, Volume: 18},
	})
}
