package signal

import (
	"math"
	"testing"

	"github.com/henry190927/trading-bot/indicator"
)

func vp(poc float64, hvn ...float64) indicator.VolumeProfile {
	return indicator.VolumeProfile{POC: poc, HVN: hvn}
}

// entry 100 / stop 95 → risk 5. Nodes above: 102 (0.4R), 110 (2.0R), 120 (4.0R).
func TestHVNTargetsLongOrdersByDistanceAndComputesR(t *testing.T) {
	got := HVNTargets(vp(110, 102, 110, 120, 98, 90), Long, 100, 95, 0)
	if len(got) != 3 {
		t.Fatalf("got %d targets, want 3 (nodes below entry are stop-side context): %+v", len(got), got)
	}
	want := []struct {
		price, r  float64
		obstacles int
		isPOC     bool
	}{
		{102, 0.4, 0, false},
		{110, 2.0, 1, true},
		{120, 4.0, 2, false},
	}
	for i, w := range want {
		g := got[i]
		if math.Abs(g.Price-w.price) > 1e-9 {
			t.Errorf("target %d price = %v, want %v", i, g.Price, w.price)
		}
		if math.Abs(g.R-w.r) > 1e-9 {
			t.Errorf("target %d R = %v, want %v", i, g.R, w.r)
		}
		if g.Obstacles != w.obstacles {
			t.Errorf("target %d Obstacles = %d, want %d", i, g.Obstacles, w.obstacles)
		}
		if g.IsPOC != w.isPOC {
			t.Errorf("target %d IsPOC = %v, want %v", i, g.IsPOC, w.isPOC)
		}
	}
}

// The mirror, on the trade actually resting on the exchange right now:
// ETH short 2,463 stop 2,495 → risk 32.
func TestHVNTargetsShort(t *testing.T) {
	got := HVNTargets(vp(2400, 2440, 2400, 2350, 2486), Short, 2463, 2495, 0)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 — the 2486 node is above a SHORT entry and is not a target: %+v", len(got), got)
	}
	for i, w := range []struct {
		price, r float64
	}{{2440, 23.0 / 32}, {2400, 63.0 / 32}, {2350, 113.0 / 32}} {
		if math.Abs(got[i].Price-w.price) > 1e-9 || math.Abs(got[i].R-w.r) > 1e-9 {
			t.Errorf("target %d = %v @ %.4fR, want %v @ %.4fR", i, got[i].Price, got[i].R, w.price, w.r)
		}
	}
}

// Obstacles count EVERY nearer node, including ones minR filtered out of the
// list. A node at 0.4R is not worth aiming at but still stalls price, and
// dropping it from the count would make a distant target look clear.
func TestHVNTargetsCountObstaclesIncludingFilteredNodes(t *testing.T) {
	got := HVNTargets(vp(0, 102, 110, 120), Long, 100, 95, 0.5)
	if len(got) != 2 {
		t.Fatalf("minR=0.5 should drop the 0.4R node, leaving 2: %+v", got)
	}
	if got[0].Price != 110 {
		t.Fatalf("first surviving target = %v, want 110", got[0].Price)
	}
	if got[0].Obstacles != 1 {
		t.Errorf("110 has Obstacles = %d, want 1 — the filtered 102 node is still in the way", got[0].Obstacles)
	}
	if got[1].Obstacles != 2 {
		t.Errorf("120 has Obstacles = %d, want 2", got[1].Obstacles)
	}
}

// The POC is ranked by volume and the HVN list is truncated, so the heaviest
// node can be missing from it. Omitting it would undercount every obstacle.
func TestHVNTargetsIncludesPOCMissingFromHVNList(t *testing.T) {
	got := HVNTargets(vp(105, 110, 120), Long, 100, 95, 0)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 with the POC folded in: %+v", len(got), got)
	}
	if got[0].Price != 105 || !got[0].IsPOC {
		t.Errorf("nearest = %v (isPOC %v), want the 105 POC", got[0].Price, got[0].IsPOC)
	}
	if got[1].Price != 110 || got[1].Obstacles != 1 {
		t.Errorf("110 = obstacles %d, want 1 (the POC precedes it)", got[1].Obstacles)
	}
	// And it must not be double-counted when it IS in the list.
	dup := HVNTargets(vp(110, 110, 120), Long, 100, 95, 0)
	if len(dup) != 2 {
		t.Errorf("POC already in HVN → %d targets, want 2 (no duplicate): %+v", len(dup), dup)
	}
}

func TestHVNTargetsGuards(t *testing.T) {
	for _, tc := range []struct {
		name              string
		side              Side
		entry, stop, minR float64
		profile           indicator.VolumeProfile
	}{
		{"flat side", Flat, 100, 95, 0, vp(0, 110)},
		{"zero risk", Long, 100, 100, 0, vp(0, 110)},
		{"no entry", Long, 0, 95, 0, vp(0, 110)},
		{"no nodes ahead", Long, 100, 95, 0, vp(0, 90, 80)},
		{"empty profile", Long, 100, 95, 0, indicator.VolumeProfile{}},
		{"everything filtered", Long, 100, 95, 99, vp(0, 102, 110)},
	} {
		if got := HVNTargets(tc.profile, tc.side, tc.entry, tc.stop, tc.minR); got != nil {
			t.Errorf("%s: want nil, got %+v", tc.name, got)
		}
	}
	// Non-positive node prices are skipped rather than producing a target
	// at or below zero.
	if got := HVNTargets(vp(0, 0, -5, 110), Long, 100, 95, 0); len(got) != 1 || got[0].Price != 110 {
		t.Errorf("junk node prices leaked: %+v", got)
	}
}

// The clean target is the one worth pre-filling: further nodes may show more R
// but each obstacle is a place the move can end.
func TestFirstCleanHVNTarget(t *testing.T) {
	ts := HVNTargets(vp(0, 102, 110, 120), Long, 100, 95, 0)
	clean := FirstCleanHVNTarget(ts)
	if clean == nil || clean.Price != 102 {
		t.Errorf("clean target = %v, want 102", clean)
	}

	// When minR removes the only unobstructed node, there is no clean target —
	// and saying so is the point. Every remaining candidate has something in
	// the way, which is exactly the situation a trader should be told about
	// rather than handed the least-bad number.
	filtered := HVNTargets(vp(0, 102, 110, 120), Long, 100, 95, 0.5)
	if c := FirstCleanHVNTarget(filtered); c != nil {
		t.Errorf("want nil clean target, got %v", c)
	}
	if FirstCleanHVNTarget(nil) != nil {
		t.Error("nil input must give nil")
	}
}

// BuildVolumeProfile returned the same price twice in HVN on live BTC 1h
// (77708.067). A repeated price would count as two obstacles for everything
// behind it when it is one level — and Obstacles is the entire point of this
// function, so an inflated count is not cosmetic.
func TestHVNTargetsDedupesRepeatedNodes(t *testing.T) {
	dup := HVNTargets(vp(0, 110, 110, 120), Long, 100, 95, 0)
	if len(dup) != 2 {
		t.Fatalf("got %d targets from a list with a duplicate, want 2: %+v", len(dup), dup)
	}
	if dup[1].Price != 120 || dup[1].Obstacles != 1 {
		t.Errorf("120 has Obstacles = %d, want 1 — the duplicated 110 was counted twice", dup[1].Obstacles)
	}
}

// Clear air is a FINDING, not missing data. On live BTC the plan was LONG at
// 79,464 while every node sat at 77.7-78.2k, so there was nothing above at
// all — a UI that renders an empty target list as "no data" hides that.
func TestNodesAheadDistinguishesClearAirFromNoProfile(t *testing.T) {
	profile := vp(77708.07, 77708.07, 78009.33, 78159.96)

	ahead, total := NodesAhead(profile, Long, 79464)
	if ahead != 0 {
		t.Errorf("ahead = %d, want 0 (every node is below a long entry at 79,464)", ahead)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 distinct nodes (the POC duplicates one HVN)", total)
	}

	// Same profile, short from the same price: now everything is ahead.
	ahead, _ = NodesAhead(profile, Short, 79464)
	if ahead != 3 {
		t.Errorf("short ahead = %d, want 3", ahead)
	}

	// No profile at all is the genuinely empty case.
	if a, tot := NodesAhead(indicator.VolumeProfile{}, Long, 100); a != 0 || tot != 0 {
		t.Errorf("empty profile → %d/%d, want 0/0", a, tot)
	}
	// Flat side has no profit direction.
	if a, _ := NodesAhead(profile, Flat, 79464); a != 0 {
		t.Errorf("flat side ahead = %d, want 0", a)
	}
}
