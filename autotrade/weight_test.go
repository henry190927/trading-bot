package autotrade

import (
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

func TestRWeight(t *testing.T) {
	for _, tc := range []struct {
		fire    float64
		book, w float64
	}{
		{0, 1.5, 1}, // every fade fire: no override → exactly 1
		{0.25, 1.5, 1.0 / 6},
		{0.25, 0, 1}, // fixed-margin book: nothing to normalise to
		{1.5, 1.5, 1},
	} {
		if got := (PaperFire{RiskPct: tc.fire}).RWeight(tc.book); !near(got, tc.w) {
			t.Errorf("RWeight(fire %v, book %v) = %v, want %v", tc.fire, tc.book, got, tc.w)
		}
	}
}

// The breaker sums book units: three −1R stops of a 0.25% rule are −0.5R at a
// 1.5% book, not −3R — which against a −3R halt is the difference between
// halting every rule for the day and not.
func TestBreakerWeightsMixedRisk(t *testing.T) {
	cs := trailFixture()
	now := cs[9].CloseTime
	var fires []PaperFire // newest-first
	for i, sym := range []string{"ETH", "SOL", "XRP"} {
		f := PaperFire{Time: cs[5].CloseTime.Add(time.Duration(i) * time.Minute), Symbol: sym, TF: "4h", Strategy: "htf-trend",
			Side: "long", Market: true, Entry: 13, Stop: 12.5, TP: 0, TrailM: 2, RiskPct: 0.25, BarTime: cs[5].CloseTime}
		fires = append([]PaperFire{f}, fires...)
	}
	candles := func(string, string) []market.Candle { return cs }
	// Each: signal bar 5, entry 13, stop 12.5. Stop for bar 6 =
	// max(12.5, min L(4..5) = 10) = 12.5; bar 6 Low 12 hits it at
	// min(12.5, open 12.8) = 12.5 → −1R, settled the same UTC day as now.
	raw, _, _ := BuildBookWeighted(fires, candles, now, 0, 0)
	if !near(raw.RealizedRToday, -3) {
		t.Fatalf("unweighted = %v, want -3 (three -1R stops)", raw.RealizedRToday)
	}
	w, _, _ := BuildBookWeighted(fires, candles, now, 0, 1.5)
	if !near(w.RealizedRToday, -0.5) {
		t.Errorf("weighted = %v, want 3 × -1/6 = -0.5", w.RealizedRToday)
	}
	cfg := Config{DailyLossHaltR: -3, RiskPerTradePct: 1.5}
	if CheckCaps(cfg, w, Candidate{}).Halt {
		t.Error("the halt tripped on 0.25% stops counted as full R")
	}
	// ReplayWithCaps banks the same units into its daily breaker: a fade
	// that would be refused at -3R raw is admitted at -0.5R.
	oldest := []PaperFire{fires[2], fires[1], fires[0]}
	fade := PaperFire{Time: cs[9].CloseTime.Add(time.Minute), Symbol: "BTC", TF: "4h", Strategy: "engine", Side: "long", Market: true, Entry: 11.8, Stop: 11, TP: 13}
	resolve := func(f PaperFire) Outcome { return Resolve(f, cs, 6) }
	_, blocked := ReplayWithCaps(append(oldest, fade), cfg, 6, 0, time.Hour, resolve)
	if len(blocked) != 0 {
		t.Errorf("weighted replay blocked %+v", blocked)
	}
	cfg.RiskPerTradePct = 0 // fixed margin: weights are 1, the -3R halts
	_, blocked = ReplayWithCaps(append(oldest, fade), cfg, 6, 0, time.Hour, resolve)
	if len(blocked) != 1 || !strings.HasPrefix(blocked[0].Reason, "daily-loss halt") {
		t.Errorf("unweighted replay: %+v, want the fade halted", blocked)
	}
	// Panel figures: normalised for cross-rule sums, flagged as mixed.
	ps := []Position{{Fire: fires[0], Outcome: Outcome{Status: OutStop, NetR: -1}}, {Fire: fade, Outcome: Outcome{Status: OutTP, NetR: 2}}}
	norm, mixed := NormaliseR(ps, 1.5)
	if !mixed || !near(norm[0].Outcome.NetR, -1.0/6) || norm[1].Outcome.NetR != 2 || ps[0].Outcome.NetR != -1 {
		t.Errorf("NormaliseR = %+v mixed %v (input must stay unscaled)", norm, mixed)
	}
	if _, mixed := NormaliseR(ps[1:], 1.5); mixed {
		t.Error("no override anywhere must not be flagged mixed")
	}
}

// With a non-zero book pct, the pinned fixed-bracket log (no risk_pct
// anywhere) must still give the golden figures.
func TestFixedBracketPinnedUnderWeights(t *testing.T) {
	cs := pinCandles()
	fires := pinFires(cs)
	newest := make([]PaperFire, len(fires))
	for i, f := range fires {
		newest[len(fires)-1-i] = f
	}
	candles := func(sym, tf string) []market.Candle {
		if sym == "SOL" {
			return cs[:200]
		}
		return cs
	}
	now := cs[len(cs)-1].CloseTime
	a, _ := BuildBook(newest, candles, now, 6)
	b, _, _ := BuildBookWeighted(newest, candles, now, 6, 1.5)
	if a.RealizedRToday != b.RealizedRToday || a.OpenCount != b.OpenCount {
		t.Errorf("weights moved a fixed-bracket book: %+v vs %+v", a, b)
	}
	ps, _ := ReplayWithCaps(fires, Config{}, 6, 6, time.Hour, func(f PaperFire) Outcome { return Resolve(f, candles(f.Symbol, f.TF), 6) })
	norm, mixed := NormaliseR(ps, 1.5)
	if mixed {
		t.Error("fixed-bracket log flagged as mixed risk")
	}
	for i := range ps {
		if ps[i].Outcome != norm[i].Outcome {
			t.Fatalf("position %d normalised: %+v vs %+v", i, ps[i].Outcome, norm[i].Outcome)
		}
	}
}

// Same-side cap within a group: counts legs across symbols, ignores the other
// side and legs outside the group; 0 = off.
func TestStrategyCapsSameSide(t *testing.T) {
	cfg := Config{StrategyCaps: map[string]StrategyCap{"htf-trend": {Strategies: []string{"htf-trend", "htf-trend-1d"}, MaxSameSide: 2}}}
	legs := []Leg{
		{Symbol: "BTC", Side: "long", Strategy: "htf-trend"},
		{Symbol: "ETH", Side: "long", Strategy: "htf-trend-1d"},
		{Symbol: "SOL", Side: "short", Strategy: "htf-trend"},
		{Symbol: "XRP", Side: "long", Strategy: "engine"}, // outside the group
	}
	bk := Book{OpenCount: len(legs), OpenLegs: legs}
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "SUI", Side: "long", Strategy: "htf-trend"}); !v.Blocked || !strings.HasPrefix(v.Reason, "strategy-same-side htf-trend: 2 long") {
		t.Errorf("third group long: %+v, want strategy-same-side", v)
	}
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "SUI", Side: "short", Strategy: "htf-trend"}); v.Blocked {
		t.Errorf("second group short: %s, want allowed", v.Reason)
	}
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "SUI", Side: "long", Strategy: "engine"}); v.Blocked {
		t.Errorf("a fade long is not in the group: %s", v.Reason)
	}
	off := Config{StrategyCaps: map[string]StrategyCap{"htf-trend": {Strategies: []string{"htf-trend", "htf-trend-1d"}}}}
	if v := CheckCaps(off, bk, Candidate{Symbol: "SUI", Side: "long", Strategy: "htf-trend"}); v.Blocked {
		t.Errorf("MaxSameSide 0: %s, want off", v.Reason)
	}
	shared, groups := CapUsage(cfg, bk)
	if shared != 1 || groups["htf-trend"] != 3 {
		t.Errorf("CapUsage = %d, %v, want 1 shared, 3 in htf-trend", shared, groups)
	}
}
