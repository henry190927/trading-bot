package autotrade

import (
	"strings"
	"testing"
)

// Every expectation below is hand-traced against CheckCaps' comparisons before
// being written down, per the standing rule that my "expected" values are the
// thing most likely to be wrong.
func TestCheckCaps(t *testing.T) {
	cfg := func(conc int, margin, halt float64) Config {
		return Config{MaxConcurrentTotal: conc, MaxMarginTotalUSDT: margin, DailyLossHaltR: halt}
	}

	t.Run("a legacy config with zero caps is UNLIMITED, not frozen", func(t *testing.T) {
		// autotrade.json files written before these fields existed unmarshal
		// them to 0. If zero meant "cap of zero" this change would have frozen
		// the executor the moment it shipped.
		v := CheckCaps(cfg(0, 0, 0), Book{OpenCount: 5, OpenMargin: 500, RealizedRToday: -10}, 35)
		if v.Blocked {
			t.Fatalf("zero caps must not block, got %+v", v)
		}
	})

	t.Run("max-concurrent blocks at the cap and allows below it", func(t *testing.T) {
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 2}, 35); !v.Blocked {
			t.Error("2 open with cap 2 must block")
		} else if !strings.Contains(v.Reason, "max-concurrent-total") {
			t.Errorf("reason should name the cap, got %q", v.Reason)
		}
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 1}, 35); v.Blocked {
			t.Error("1 open with cap 2 must be allowed")
		}
		if v := CheckCaps(cfg(2, 0, 0), Book{OpenCount: 3}, 35); !v.Blocked {
			t.Error("over the cap must block too, not just exactly at it")
		}
	})

	t.Run("margin cap counts the candidate and permits landing exactly on it", func(t *testing.T) {
		// 65 + 35 = 100, cap 100 → allowed (a total cap is inclusive).
		if v := CheckCaps(cfg(0, 100, 0), Book{OpenMargin: 65}, 35); v.Blocked {
			t.Errorf("exactly at the cap must be allowed, got %+v", v)
		}
		// 70 + 35 = 105 > 100 → blocked.
		v := CheckCaps(cfg(0, 100, 0), Book{OpenMargin: 70}, 35)
		if !v.Blocked {
			t.Fatal("exceeding the cap must block")
		}
		if !strings.Contains(v.Reason, "max-margin-total") {
			t.Errorf("reason should name the cap, got %q", v.Reason)
		}
		// The candidate's own margin must be included — 0 open with a single
		// oversized order still has to be refused.
		if v := CheckCaps(cfg(0, 100, 0), Book{}, 150); !v.Blocked {
			t.Error("a single order larger than the whole cap must block")
		}
	})

	t.Run("daily-loss halt trips at the threshold and flags Halt", func(t *testing.T) {
		v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: -3.0}, 35)
		if !v.Blocked || !v.Halt {
			t.Fatalf("-3.00R against a -3.0R limit must halt, got %+v", v)
		}
		if !strings.Contains(v.Reason, "daily-loss halt") || !strings.Contains(v.Reason, "00:00 UTC") {
			t.Errorf("reason should name the breaker and its re-arm, got %q", v.Reason)
		}
		if v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: -2.9}, 35); v.Blocked {
			t.Error("-2.9R must not halt against a -3.0R limit")
		}
		if v := CheckCaps(cfg(0, 0, -3), Book{RealizedRToday: +5}, 35); v.Blocked {
			t.Error("a profitable day must not halt")
		}
	})

	t.Run("halt is disabled at zero and by a mis-signed positive value", func(t *testing.T) {
		if v := CheckCaps(cfg(0, 0, 0), Book{RealizedRToday: -100}, 35); v.Blocked {
			t.Error("halt 0 = disabled, must not block even at -100R")
		}
		// A config typo of +3 instead of -3 should disable the breaker, not
		// make it fire on every profitable tick.
		if v := CheckCaps(cfg(0, 0, 3), Book{RealizedRToday: +5}, 35); v.Blocked {
			t.Errorf("a positive halt value must disable, not trip: %+v", v)
		}
	})

	t.Run("halt outranks the sizing caps", func(t *testing.T) {
		// Room on concurrency and margin, but the day is done.
		v := CheckCaps(cfg(5, 500, -3), Book{OpenCount: 0, OpenMargin: 0, RealizedRToday: -4}, 35)
		if !v.Blocked || !v.Halt {
			t.Fatalf("halt must block despite headroom, got %+v", v)
		}
		// When several caps are breached the halt is the reported reason.
		v2 := CheckCaps(cfg(1, 50, -3), Book{OpenCount: 9, OpenMargin: 900, RealizedRToday: -9}, 35)
		if !v2.Halt || !strings.Contains(v2.Reason, "daily-loss halt") {
			t.Errorf("halt should win the reason, got %+v", v2)
		}
	})

	t.Run("an empty book with sane caps is allowed", func(t *testing.T) {
		if v := CheckCaps(cfg(2, 100, -3), Book{}, 35); v.Blocked {
			t.Errorf("fresh book must be allowed, got %+v", v)
		}
	})
}
