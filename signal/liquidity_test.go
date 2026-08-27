package signal

import "testing"

func TestClusterSwings(t *testing.T) {
	// three highs: 2531.0 & 2530.5 are within 0.15% (≈3.8pt) → one EQH pool of 2;
	// 2505 is >0.15% away → alone, not a pool.
	highs := []SwingPoint{
		{Index: 10, Price: 2531.0, IsTop: true},
		{Index: 22, Price: 2530.5, IsTop: true},
		{Index: 30, Price: 2505.0, IsTop: true},
	}
	pools := clusterSwings(highs, EQH, 0.0015)
	if len(pools) != 1 {
		t.Fatalf("want 1 EQH pool, got %d: %+v", len(pools), pools)
	}
	p := pools[0]
	if p.Touches != 2 {
		t.Errorf("touches = %d, want 2", p.Touches)
	}
	if p.Price < 2530 || p.Price > 2531 {
		t.Errorf("pool price = %.2f, want ~2530.75", p.Price)
	}
	if p.LastIdx != 22 {
		t.Errorf("lastIdx = %d, want 22", p.LastIdx)
	}
	// a lone swing makes no pool
	if got := clusterSwings([]SwingPoint{{Price: 100, IsTop: true}}, EQH, 0.0015); got != nil {
		t.Errorf("single swing should yield no pool, got %+v", got)
	}
}

func TestNearestLiquidity(t *testing.T) {
	levels := []LiquidityLevel{
		{Kind: EQL, Price: 2415},
		{Kind: EQL, Price: 2460},
		{Kind: EQH, Price: 2505},
		{Kind: EQH, Price: 2531},
	}
	above, below := NearestLiquidity(levels, 2490)
	if above == nil || above.Price != 2505 {
		t.Errorf("above = %+v, want EQH 2505", above)
	}
	if below == nil || below.Price != 2460 {
		t.Errorf("below = %+v, want EQL 2460", below)
	}
	// price above everything → no EQH above
	if a, _ := NearestLiquidity(levels, 2600); a != nil {
		t.Errorf("want no EQH above 2600, got %+v", a)
	}
}
