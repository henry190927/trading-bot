package autotrade

import (
	"strings"
	"testing"
)

// Every expectation below is hand-traced against CheckCaps' comparisons before
// being written down, per the standing rule that my "expected" values are the
// thing most likely to be wrong.
//
// The third argument became a Candidate on 2026-09-10 when the same-symbol
// same-direction cap needed the symbol and side. The cases below pass only a
// Margin, so their Side is empty — which is also the halt-probe shape, and is
// exactly why every candidate-specific rule has to skip on an empty Side.
func TestCheckCaps(t *testing.T) {
	cfg := func(conc int, margin, halt float64) Config {
		return Config{MaxConcurrentTotal: conc, MaxMarginTotalUSDT: margin, DailyLossHaltR: halt}
	}

	t.Run("a legacy config with zero caps is UNLIMITED, not frozen", func(t *testing.T) {
		// autotrade.json files written before these fields existed unmarshal
		// them to 0. If zero meant "cap of zero" this change would have frozen
		// the executor the moment it shipped.
		v := CheckCaps(cfg(0, 0, 0), Book{OpenCount: 5, OpenMargin: 500, RealizedRToday: -10}, Candidate{Margin: 35})
		if v.Blocked {
			t.Fatalf("zero caps must not block, got %+v", v)
		}
	})

	t.Run("max-concurrent blocks at the cap and allows below it", func(t *testing.T) {
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 2}, Candidate{Margin: 35}); !v.Blocked {
			t.Error("2 open with cap 2 must block")
		} else if !strings.Contains(v.Reason, "max-concurrent-total") {
			t.Errorf("reason should name the cap, got %q", v.Reason)
		}
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 1}, Candidate{Margin: 35}); v.Blocked {
			t.Error("1 open with cap 2 must be allowed")
		}
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 3}, Candidate{Margin: 35}); !v.Blocked {
			t.Error("over the cap must block too, not just exactly at it")
		}
	})

	t.Run("margin cap counts the candidate and permits landing exactly on it", func(t *testing.T) {
		// 65 + 35 = 100, cap 100 → allowed (a total cap is inclusive).
		if v := CheckCaps(cfg(0, 100, 0), Book{OpenMargin: 65}, Candidate{Margin: 35}); v.Blocked {
			t.Errorf("exactly at the cap must be allowed, got %+v", v)
		}
		// 70 + 35 = 105 > 100 → blocked.
		v := CheckCaps(cfg(0, 100, 0), Book{OpenMargin: 70}, Candidate{Margin: 35})
		if !v.Blocked {
			t.Fatal("exceeding the cap must block")
		}
		if !strings.Contains(v.Reason, "max-margin-total") {
			t.Errorf("reason should name the cap, got %q", v.Reason)
		}
		// The candidate's own margin must be included — 0 open with a single
		// oversized order still has to be refused.
		if v := CheckCaps(cfg(0, 100, 0), Book{}, Candidate{Margin: 150}); !v.Blocked {
			t.Error("a single order larger than the whole cap must block")
		}
	})

	t.Run("daily-loss halt trips at the threshold and flags Halt", func(t *testing.T) {
		v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: -3.0}, Candidate{Margin: 35})
		if !v.Blocked || !v.Halt {
			t.Fatalf("-3.00R against a -3.0R limit must halt, got %+v", v)
		}
		if !strings.Contains(v.Reason, "daily-loss halt") || !strings.Contains(v.Reason, "00:00 UTC") {
			t.Errorf("reason should name the breaker and its re-arm, got %q", v.Reason)
		}
		if v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: -2.9}, Candidate{Margin: 35}); v.Blocked {
			t.Error("-2.9R must not halt against a -3.0R limit")
		}
		if v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: +5}, Candidate{Margin: 35}); v.Blocked {
			t.Error("a profitable day must not halt")
		}
	})

	t.Run("halt is disabled at zero and by a mis-signed positive value", func(t *testing.T) {
		if v := CheckCaps(cfg(0, 0, 0), Book{RealizedRToday: -100}, Candidate{Margin: 35}); v.Blocked {
			t.Error("halt 0 = disabled, must not block even at -100R")
		}
		// A config typo of +3 instead of -3 should disable the breaker, not
		// make it fire on every profitable tick.
		if v := CheckCaps(cfg(0, 0, 3), Book{RealizedRToday: +5}, Candidate{Margin: 35}); v.Blocked {
			t.Errorf("a positive halt value must disable, not trip: %+v", v)
		}
	})

	t.Run("halt outranks the sizing caps", func(t *testing.T) {
		// Room on concurrency and margin, but the day is done.
		v := CheckCaps(cfg(5, 500, -3), Book{OpenCount: 0, OpenMargin: 0, RealizedRToday: -4}, Candidate{Margin: 35})
		if !v.Blocked || !v.Halt {
			t.Fatalf("halt must block despite headroom, got %+v", v)
		}
		// When several caps are breached the halt is the reported reason.
		v2 := CheckCaps(cfg(1, 50, -3), Book{OpenCount: 9, OpenMargin: 900, RealizedRToday: -9}, Candidate{Margin: 35})
		if !v2.Halt || !strings.Contains(v2.Reason, "daily-loss halt") {
			t.Errorf("halt should win the reason, got %+v", v2)
		}
	})

	t.Run("an empty book with sane caps is allowed", func(t *testing.T) {
		if v := CheckCaps(cfg(2, 100, -3), Book{}, Candidate{Margin: 35}); v.Blocked {
			t.Errorf("fresh book must be allowed, got %+v", v)
		}
	})
}

// Same-symbol same-direction cap. Every expectation hand-traced against the
// loop in CheckCaps: it counts legs where BOTH Symbol and Side match the
// candidate, then blocks when that count reaches the cap.
func TestCheckCapsSameSymbolSide(t *testing.T) {
	side := func(n int) Config { return Config{MaxSameSymbolSide: n} }
	ethShort := Book{OpenCount: 1, OpenLegs: []Leg{{Symbol: "ETH", Side: "short"}}}

	t.Run("zero is UNLIMITED — an existing autotrade.json keeps today's behaviour", func(t *testing.T) {
		bk := Book{OpenCount: 2, OpenLegs: []Leg{
			{Symbol: "ETH", Side: "short"}, {Symbol: "ETH", Side: "short"},
		}}
		if v := CheckCaps(side(0), bk, Candidate{Symbol: "ETH", Side: "short", Margin: 35}); v.Blocked {
			t.Errorf("cap 0 must not block, got %q", v.Reason)
		}
	})

	t.Run("cap 1 refuses a repeat of the same opinion", func(t *testing.T) {
		v := CheckCaps(side(1), ethShort, Candidate{Symbol: "ETH", Side: "short", Margin: 35})
		if !v.Blocked {
			t.Fatal("a second ETH short must be refused")
		}
		if !strings.Contains(v.Reason, "max-same-symbol-side") {
			t.Errorf("reason should name the cap, got %q", v.Reason)
		}
		if !strings.Contains(v.Reason, "ETH") || !strings.Contains(v.Reason, "short") {
			t.Errorf("reason should name WHAT is already open, got %q", v.Reason)
		}
	})

	t.Run("the opposite side is allowed — a hedge is not a repeat", func(t *testing.T) {
		if v := CheckCaps(side(1), ethShort, Candidate{Symbol: "ETH", Side: "long", Margin: 35}); v.Blocked {
			t.Errorf("ETH long against an open ETH short must be allowed, got %q", v.Reason)
		}
	})

	t.Run("a different symbol is allowed", func(t *testing.T) {
		if v := CheckCaps(side(1), ethShort, Candidate{Symbol: "BTC", Side: "short", Margin: 35}); v.Blocked {
			t.Errorf("BTC short against an open ETH short must be allowed, got %q", v.Reason)
		}
	})

	t.Run("the halt probe (empty Side) skips this rule", func(t *testing.T) {
		// cmd/monitor calls CheckCaps once per tick with a zero Candidate just
		// to read the breaker. If this rule fired on it, the whole executor
		// would stop the moment any leg was open.
		if v := CheckCaps(side(1), ethShort, Candidate{}); v.Blocked {
			t.Errorf("zero Candidate must not be blocked, got %q", v.Reason)
		}
	})

	t.Run("an unscoreable leg has no Side and matches nothing", func(t *testing.T) {
		// BuildBook records Leg{Symbol} with no Side for a fire it could not
		// score: it holds a slot, but its direction is not trustworthy enough
		// to refuse a candidate on.
		bk := Book{OpenCount: 1, OpenLegs: []Leg{{Symbol: "ETH"}}}
		if v := CheckCaps(side(1), bk, Candidate{Symbol: "ETH", Side: "short", Margin: 35}); v.Blocked {
			t.Errorf("an unscoreable ETH leg must not block an ETH short, got %q", v.Reason)
		}
	})

	t.Run("cap 2 permits the second and refuses the third", func(t *testing.T) {
		one := Book{OpenCount: 1, OpenLegs: []Leg{{Symbol: "ETH", Side: "short"}}}
		if v := CheckCaps(side(2), one, Candidate{Symbol: "ETH", Side: "short", Margin: 35}); v.Blocked {
			t.Errorf("second ETH short under cap 2 must be allowed, got %q", v.Reason)
		}
		two := Book{OpenCount: 2, OpenLegs: []Leg{
			{Symbol: "ETH", Side: "short"}, {Symbol: "ETH", Side: "short"},
		}}
		if v := CheckCaps(side(2), two, Candidate{Symbol: "ETH", Side: "short", Margin: 35}); !v.Blocked {
			t.Error("third ETH short under cap 2 must be refused")
		}
	})

	t.Run("order: this reason outranks the global concurrent cap", func(t *testing.T) {
		// Both would fire. The specific reason is the useful one — "already
		// short ETH" tells the operator something "1 open >= cap 1" does not.
		cfg := Config{MaxSameSymbolSide: 1, MaxConcurrentTotal: 1}
		v := CheckCaps(cfg, ethShort, Candidate{Symbol: "ETH", Side: "short", Margin: 35})
		if !v.Blocked {
			t.Fatal("must block")
		}
		if !strings.Contains(v.Reason, "max-same-symbol-side") {
			t.Errorf("want the same-side reason, got %q", v.Reason)
		}
	})

	t.Run("the daily-loss halt still outranks everything", func(t *testing.T) {
		cfg := Config{MaxSameSymbolSide: 1, DailyLossHaltR: -3}
		bk := ethShort
		bk.RealizedRToday = -3.0
		v := CheckCaps(cfg, bk, Candidate{Symbol: "ETH", Side: "short", Margin: 35})
		if !v.Halt {
			t.Fatalf("halt must win, got %+v", v)
		}
	})
}
