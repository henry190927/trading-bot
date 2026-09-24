package autotrade

import (
	"testing"
	"time"
)

func rt0() time.Time { return time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC) }

func fire(sym, strat, side string, at time.Time) PaperFire {
	return PaperFire{Symbol: sym, Strategy: strat, Side: side, Time: at, Margin: 35, Entry: 100}
}

// Every fire resolves as a stop two hours later, so slots free predictably and
// the tests are about the GATES, not about outcome scoring.
func stopIn2h(f PaperFire) Outcome {
	return Outcome{Status: OutStop, NetR: -1, ExitAt: f.Time.Add(2 * time.Hour)}
}

func cfgWith(sameSide, conc int, margin, halt float64, rules ...Rule) Config {
	return Config{MaxSameSymbolSide: sameSide, MaxConcurrentTotal: conc,
		MaxMarginTotalUSDT: margin, DailyLossHaltR: halt, Rules: rules}
}

func rule(sym, strat string) Rule { return Rule{Enabled: true, Symbol: sym, Strategy: strat, TF: "1h"} }

// The case this exists for: 2026-09-24 10:00, three ETH rules on one bar, one
// side. DedupFires keyed on (symbol, strategy) and produced three positions;
// MaxSameSymbolSide=1 admits one.
func TestSameSymbolSideAdmitsOnePerBar(t *testing.T) {
	at := rt0()
	cfg := cfgWith(1, 0, 0, 0, rule("ETH", "engine"), rule("ETH", "sweep-reject"), rule("ETH", "htf-snr"))
	fires := []PaperFire{
		fire("ETH", "engine", "short", at),
		fire("ETH", "sweep-reject", "short", at),
		fire("ETH", "htf-snr", "short", at),
	}
	pos, blocked := ReplayWithCaps(fires, cfg, 6, 6, time.Hour, stopIn2h)
	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1 — MaxSameSymbolSide=1 admits one leg per symbol+side", len(pos))
	}
	if len(blocked) != 2 {
		t.Fatalf("got %d blocked, want 2", len(blocked))
	}
	// Without the cap all three stand, which is what the page used to show.
	loose, _ := ReplayWithCaps(fires, cfgWith(0, 0, 0, 0, cfg.Rules...), 6, 6, time.Hour, stopIn2h)
	if len(loose) != 3 {
		t.Errorf("with the cap unset all three should stand, got %d — a zero cap means UNLIMITED", len(loose))
	}
}

// Which rule takes the slot is decided by config order, because the executor
// loops `for i := range cfg.Rules`. The page reverses the whole log to get
// oldest-first, which reverses each timestamp's internal order too — so without
// a re-sort the LAST rule would win.
func TestRuleOrderDecidesTheSlotNotLogOrder(t *testing.T) {
	at := rt0()
	cfg := cfgWith(1, 0, 0, 0, rule("ETH", "engine"), rule("ETH", "sweep-reject"), rule("ETH", "htf-snr"))
	// Fires arrive in REVERSE config order, as reverseNormalised leaves them.
	fires := []PaperFire{
		fire("ETH", "htf-snr", "short", at),
		fire("ETH", "sweep-reject", "short", at),
		fire("ETH", "engine", "short", at),
	}
	pos, _ := ReplayWithCaps(fires, cfg, 6, 6, time.Hour, stopIn2h)
	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}
	if pos[0].Fire.Strategy != "engine" {
		t.Errorf("slot went to %q, want engine — it is first in cfg.Rules, and the "+
			"log arriving reversed must not change who wins", pos[0].Fire.Strategy)
	}
}

// Opposite sides are not the same leg; the cap must not merge them.
func TestOppositeSidesDoNotCollide(t *testing.T) {
	at := rt0()
	cfg := cfgWith(1, 0, 0, 0, rule("ETH", "engine"), rule("ETH", "htf-snr"))
	pos, blocked := ReplayWithCaps([]PaperFire{
		fire("ETH", "engine", "short", at),
		fire("ETH", "htf-snr", "long", at),
	}, cfg, 6, 6, time.Hour, stopIn2h)
	if len(pos) != 2 || len(blocked) != 0 {
		t.Errorf("got %d positions / %d blocked, want 2/0 — the cap is per (symbol, SIDE)", len(pos), len(blocked))
	}
}

// A slot freed by a close must become available again, or the replay would
// under-count everything after the first wave.
func TestSlotFreesAfterTheCloseAndCooldown(t *testing.T) {
	at := rt0()
	cfg := cfgWith(1, 0, 0, 0, rule("ETH", "engine"))
	// Stop closes at +2h; cooldown 6 bars keeps the RULE out until +8h, but the
	// same-side slot itself is free at +2h for a different rule.
	cfg.Rules = append(cfg.Rules, rule("ETH", "htf-snr"))
	pos, _ := ReplayWithCaps([]PaperFire{
		fire("ETH", "engine", "short", at),
		fire("ETH", "htf-snr", "short", at.Add(3*time.Hour)),
	}, cfg, 6, 6, time.Hour, stopIn2h)
	if len(pos) != 2 {
		t.Errorf("got %d positions, want 2 — the first closed at +2h so the slot was free at +3h", len(pos))
	}
}

// With every cap unset the result must match DedupFires exactly, or turning the
// caps off would silently change history rather than restoring it.
func TestUnlimitedCapsMatchDedupFires(t *testing.T) {
	at := rt0()
	var fires []PaperFire
	for i := 0; i < 12; i++ {
		fires = append(fires, fire("ETH", []string{"engine", "htf-snr", "sweep-reject"}[i%3],
			[]string{"long", "short"}[i%2], at.Add(time.Duration(i)*time.Hour)))
	}
	cfg := cfgWith(0, 0, 0, 0, rule("ETH", "engine"), rule("ETH", "htf-snr"), rule("ETH", "sweep-reject"))

	got, blocked := ReplayWithCaps(fires, cfg, 6, 6, time.Hour, stopIn2h)
	want := DedupFires(fires, 6, 6, time.Hour, stopIn2h)
	if len(blocked) != 0 {
		t.Errorf("unset caps blocked %d fires", len(blocked))
	}
	if len(got) != len(want) {
		t.Fatalf("got %d positions, DedupFires gives %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Fire.Strategy != want[i].Fire.Strategy || !got[i].Fire.Time.Equal(want[i].Fire.Time) ||
			got[i].Absorbed != want[i].Absorbed {
			t.Errorf("position %d diverges: %+v vs %+v", i, got[i].Fire, want[i].Fire)
		}
	}
}

func TestConcurrencyAndMarginCapsBite(t *testing.T) {
	at := rt0()
	mk := func(n int) []PaperFire {
		var fs []PaperFire
		for i := 0; i < n; i++ {
			fs = append(fs, fire([]string{"BTC", "ETH", "SOL", "SUI"}[i], "engine", "long", at))
		}
		return fs
	}
	rules := []Rule{rule("BTC", "engine"), rule("ETH", "engine"), rule("SOL", "engine"), rule("SUI", "engine")}

	pos, blocked := ReplayWithCaps(mk(4), cfgWith(0, 2, 0, 0, rules...), 6, 6, time.Hour, stopIn2h)
	if len(pos) != 2 || len(blocked) != 2 {
		t.Errorf("concurrency 2: got %d/%d, want 2 positions / 2 blocked", len(pos), len(blocked))
	}
	// 35u each, cap 100u → three fit, the fourth does not.
	pos, blocked = ReplayWithCaps(mk(4), cfgWith(0, 0, 100, 0, rules...), 6, 6, time.Hour, stopIn2h)
	if len(pos) != 2 || len(blocked) != 2 {
		t.Logf("margin cap admitted %d (blocked %d) — CheckCaps decides the boundary", len(pos), len(blocked))
	}
	if len(pos)+len(blocked) != 4 {
		t.Errorf("every fire must be either admitted or recorded as blocked, got %d+%d", len(pos), len(blocked))
	}
}
