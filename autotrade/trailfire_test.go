package autotrade

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
)

// 4h bars for the trail tests: a flat base, a long breakout on bar 4, a run
// up, then a drop through the trail. M=2. Hand-traced in the comments.
func trailFixture() []market.Candle {
	rows := [][4]float64{ // open, high, low, close
		{10, 11, 9, 10},      // 0
		{10, 11, 9, 10},      // 1
		{10, 11, 9, 10},      // 2
		{10, 11, 9.5, 10},    // 3
		{10, 12, 10, 11.5},   // 4  signal: long 11.5, initial stop 10.5
		{11.5, 13, 11, 12.8}, // 5  stop for 5: max(10.5, min L(3..4)=9.5) = 10.5
		{12.8, 14, 12, 13.5}, // 6  stop for 6: max(10.5, min L(4..5)=10) = 10.5
		{13.5, 15, 13, 14.5}, // 7  stop for 7: min L(5..6) = 11
		{14.5, 15, 11, 11.5}, // 8  stop for 8: min L(6..7) = 12 → Low 11 hits: exit 12, +0.5R
		{11.5, 12, 11, 11.8}, // 9
	}
	cs := make([]market.Candle, len(rows))
	for i, r := range rows {
		cs[i] = trailBar(i, r[0], r[1], r[2], r[3])
	}
	return cs
}

func trailFire(cs []market.Candle) PaperFire {
	return PaperFire{Time: cs[4].CloseTime.Add(time.Minute), BarTime: cs[4].CloseTime, Symbol: "BTC", TF: "4h",
		Strategy: "htf-trend", Side: "long", Market: true, Entry: 11.5, Stop: 10.5, TrailM: 2, Margin: 10}
}

func TestEvaluateTrailFireHandTraced(t *testing.T) {
	cs := trailFixture()
	f := trailFire(cs)

	oc := EvaluateTrailFire(f, cs[:8]) // through bar 7: open, stop for bar 8 is 12
	if oc.Status != OutOpen || oc.CurStop != 12 || !oc.Trailed || oc.UnrealR != 3 || oc.NetR != 0 {
		t.Errorf("open = %+v, want open, CurStop 12, trailed, UnrealR (14.5-11.5)/1 = 3", oc)
	}
	oc = EvaluateTrailFire(f, cs)
	if oc.Status != OutStop || oc.ExitPrice != 12 || oc.NetR != 0.5 || !oc.Trailed || !oc.ExitAt.Equal(cs[8].CloseTime) || oc.BarsHeld != 4 {
		t.Errorf("exit = %+v, want stop at 12 (+0.5R), trailed, on bar 8 after 4 bars", oc)
	}
	// Through bar 5 the stop has not left its initial level.
	if oc := EvaluateTrailFire(f, cs[:6]); oc.Status != OutOpen || oc.CurStop != 10.5 || oc.Trailed {
		t.Errorf("early = %+v, want open at the initial 10.5, not trailed", oc)
	}
}

// Refusals: a fire the candles cannot score honestly is Unscored, never a
// guess — and never a looser trail from a short lookback (B7).
func TestEvaluateTrailFireUnscored(t *testing.T) {
	cs := trailFixture()
	f := trailFire(cs)

	// The window starts at the signal bar: the first trail needs bars 3..4.
	if oc := EvaluateTrailFire(f, cs[4:]); oc.Status != OutUnscored {
		t.Errorf("signal bar first in the window: %s, want unscored (lookback of M-1 bars missing)", oc.Status)
	}
	// With one bar of lookback (M=2 needs i >= 1) it scores.
	if oc := EvaluateTrailFire(f, cs[3:]); oc.Status != OutStop || oc.NetR != 0.5 {
		t.Errorf("exact lookback: %+v, want the same +0.5R stop", oc)
	}
	// The signal bar is not in the slice at all.
	g := f
	g.BarTime = cs[4].CloseTime.Add(time.Hour)
	if oc := EvaluateTrailFire(g, cs); oc.Status != OutUnscored {
		t.Errorf("BarTime not a bar close: %s, want unscored", oc.Status)
	}
	// No BarTime and a stale window whose last bar closed days before the fire.
	h := f
	h.BarTime, h.Time = time.Time{}, cs[9].CloseTime.Add(72*time.Hour)
	if oc := EvaluateTrailFire(h, cs); oc.Status != OutUnscored {
		t.Errorf("stale window, no BarTime: %s, want unscored", oc.Status)
	}
	if oc := EvaluateTrailFire(f, nil); oc.Status != OutUnscored {
		t.Errorf("no candles: %s, want unscored", oc.Status)
	}
}

func TestEvaluateTrailFireLive(t *testing.T) {
	cs := trailFixture()
	f := trailFire(cs)
	if oc := EvaluateTrailFireLive(f, cs[:8], 13); oc.Status != OutOpen || oc.UnrealR != 1.5 || oc.ExitPrice != 13 {
		t.Errorf("live above the trail: %+v, want open, UnrealR 1.5 marked to 13", oc)
	}
	// Through the TRAILED stop (12), above the initial (10.5): resolves now.
	if oc := EvaluateTrailFireLive(f, cs[:8], 11.9); oc.Status != OutStop || oc.ExitPrice != 12 || oc.NetR != 0.5 {
		t.Errorf("live through the trail: %+v, want stop at 12, +0.5R", oc)
	}
	// A closed exit is not un-settled by the live price.
	if oc := EvaluateTrailFireLive(f, cs, 20); oc.Status != OutStop || oc.NetR != 0.5 {
		t.Errorf("settled + live spike: %+v, want the closed-bar stop", oc)
	}
}

func TestResolveDispatch(t *testing.T) {
	cs := trailFixture()
	fixed := PaperFire{Time: cs[4].CloseTime.Add(time.Minute), Side: "long", Market: true, Entry: 11.5, Stop: 10.5, TP: 13.5}
	if a, b := Resolve(fixed, cs, 6), EvaluateFire(fixed, cs, 6); a != b {
		t.Errorf("fixed bracket through Resolve = %+v, EvaluateFire = %+v", a, b)
	}
	if a, b := ResolveLive(fixed, cs, 6, 12), EvaluateFireLive(fixed, cs, 6, 12); a != b {
		t.Errorf("fixed bracket through ResolveLive = %+v, EvaluateFireLive = %+v", a, b)
	}
	tr := trailFire(cs)
	if a, b := Resolve(tr, cs, 6), EvaluateTrailFire(tr, cs); a != b {
		t.Errorf("trail through Resolve = %+v, EvaluateTrailFire = %+v", a, b)
	}
	// The same fire without TrailM is the P0 guard, not a take-profit at 0.
	tr.TrailM = 0
	if oc := Resolve(tr, cs, 6); oc.Status != OutOpen || oc.NetR != 0 {
		t.Errorf("TP 0 without TrailM = %+v, want held open", oc)
	}
}

// Every trail exit is a stop order, so tp/(tp+stop) read 0% for a profitable
// trend follower. Wins are NetR > 0.
func TestWinRateCountsProfitableStops(t *testing.T) {
	s := Summarize([]Outcome{
		{Status: OutStop, NetR: 2.5, Trailed: true},
		{Status: OutStop, NetR: -0.4, Trailed: true},
		{Status: OutStop, NetR: -1},
		{Status: OutTP, NetR: 2},
		{Status: OutOpen, UnrealR: 3},
	})
	if s.WinRate != 0.5 || s.Stop != 3 || s.TP != 1 || s.NetR != 3.1 {
		t.Errorf("Summarize = %+v, want WR 2/4, 3 stops, 1 tp, NetR 3.1", s)
	}
}

// Slot holding for trail fires in every replay: open holds until it exits
// (not for `expiry` bars, B10), Unscored holds (fail-safe, not free), and a
// stop frees the slot after the FIRE's cooldown on its own TF (B6), not the
// panel's max-across-rules × 1h.
func TestTrailFireSlots(t *testing.T) {
	cs := trailFixture()
	f := trailFire(cs)
	bar := func(i int) time.Time { return cs[i].CloseTime.Add(time.Minute) }
	refire := func(i int) PaperFire { g := f; g.Time, g.BarTime = bar(i), cs[i].CloseTime; return g }

	// Open at the end of cs[:8]: a fire 30h later (> 6 × 1h expiry) is absorbed.
	open := func(g PaperFire) Outcome { return EvaluateTrailFire(g, cs[:8]) }
	ps, _ := ReplayWithCaps([]PaperFire{f, refire(7)}, Config{}, 6, 6, time.Hour, open)
	if len(ps) != 1 || ps[0].Absorbed != 1 {
		t.Errorf("open trail fire: %d positions (absorbed %d), want 1 holding its slot past expiry", len(ps), ps[0].Absorbed)
	}
	if dd := DedupFires([]PaperFire{f, refire(7)}, 6, 6, time.Hour, open); len(dd) != 1 {
		t.Errorf("DedupFires: %d positions, want 1", len(dd))
	}

	// Stopped on bar 8 with CooldownBars 0: a fire on bar 9 is a new position,
	// even with the panel's cooldown of 6 (× 1h) — and a fixed-bracket fire
	// in the same spot would still be absorbed by it.
	all := func(g PaperFire) Outcome { return Resolve(g, cs, 6) }
	ps, _ = ReplayWithCaps([]PaperFire{f, refire(9)}, Config{}, 6, 6, time.Hour, all)
	if len(ps) != 2 {
		t.Errorf("after a trail stop with cooldown 0: %d positions, want 2", len(ps))
	}
	// CooldownBars 2 on 4h: bar 9's fire (4h after the exit) is inside it.
	f1, g1 := f, refire(9)
	f1.CooldownBars, g1.CooldownBars = 2, 2
	if ps, _ = ReplayWithCaps([]PaperFire{f1, g1}, Config{}, 6, 0, time.Hour, all); len(ps) != 1 {
		t.Errorf("trail cooldown 2 × 4h: %d positions, want 1 (absorbed)", len(ps))
	}

	// Unscored (the window misses the lookback) holds its slot and counts open.
	short := func(g PaperFire) Outcome { return EvaluateTrailFire(g, cs[4:]) }
	ps, _ = ReplayWithCaps([]PaperFire{f, refire(9)}, Config{MaxConcurrentTotal: 5}, 6, 6, time.Hour, short)
	if len(ps) != 1 || ps[0].Outcome.Status != OutUnscored {
		t.Errorf("unscored trail fire: %+v, want one held position", ps)
	}
	if !HoldsSlot(f, Outcome{Status: OutUnscored}) || HoldsSlot(PaperFire{}, Outcome{Status: OutUnscored}) {
		t.Error("HoldsSlot: unscored must hold for a trail fire and free for a fixed bracket")
	}

	// BuildBook counts an open trail fire and an unscored one as open.
	newest := []PaperFire{f}
	bk, _ := BuildBook(newest, func(string, string) []market.Candle { return cs[:8] }, cs[7].CloseTime, 6)
	if bk.OpenCount != 1 || len(bk.OpenLegs) != 1 || bk.OpenLegs[0].Side != "long" {
		t.Errorf("open trail fire: book %+v, want 1 open long leg", bk)
	}
	bk, _ = BuildBook(newest, func(string, string) []market.Candle { return cs[4:] }, cs[9].CloseTime, 6)
	if bk.OpenCount != 1 {
		t.Errorf("unscored trail fire: book open %d, want 1 (fail-safe)", bk.OpenCount)
	}
	bk, _ = BuildBook(newest, func(string, string) []market.Candle { return cs }, cs[9].CloseTime, 6)
	if bk.OpenCount != 0 || bk.RealizedRToday != 0.5 {
		t.Errorf("settled trail fire: book %+v, want 0 open and +0.5R booked today", bk)
	}
}

// A fixed-bracket row written after this change serialises exactly as before:
// the new fields are omitted at zero, and an old row decodes to TrailM 0.
func TestPaperFireTrailFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(PaperFire{Symbol: "BTC", Side: "long", Entry: 1, Stop: 0.9, TP: 1.2})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"trail_m", "bar_time", "cooldown_bars"} {
		if strings.Contains(string(b), k) {
			t.Errorf("zero %s serialised: %s", k, b)
		}
	}
	var old PaperFire
	if err := json.Unmarshal([]byte(`{"time":"2026-09-01T00:00:00Z","symbol":"ETH","strategy":"engine","side":"short","entry":1,"stop":1.1,"tp":0.8}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.IsTrail() || !old.BarTime.IsZero() {
		t.Errorf("old row decoded as %+v, want a fixed bracket with no BarTime", old)
	}
	nw := trailFire(trailFixture())
	b, _ = json.Marshal(nw)
	var back PaperFire
	if err := json.Unmarshal(b, &back); err != nil || back.TrailM != 2 || !back.BarTime.Equal(nw.BarTime) {
		t.Errorf("trail row round trip = %+v (%v), want TrailM 2 and the bar time", back, err)
	}
}
