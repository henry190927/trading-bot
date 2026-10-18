package autotrade

import (
	"math"
	"strings"
	"testing"
)

// Expected values are hand-traced from Size's arithmetic before being written:
// stopFrac = |entry-stop|/entry, maxLev = floor(0.5/stopFrac),
// risk = equity*pct/100, notional = risk/stopFrac, margin = notional/lev.

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestMaxLevForStop(t *testing.T) {
	cases := []struct {
		frac float64
		want int
	}{
		{0.005, 100}, // 0.5/0.005 = 100 exactly — the epsilon keeps it from flooring to 99
		{0.008, 62},  // 62.5
		{0.004, 125}, // the 125x threshold
		{0.0186, 26}, // SOL sweep-reject's widest observed stop: 26.88
		{0.0385, 12}, // SUI's widest: 12.99
		{0.6, 0},     // nothing is safe
		{0, 0},       // no stop
	}
	for _, c := range cases {
		if got := MaxLevForStop(c.frac); got != c.want {
			t.Errorf("MaxLevForStop(%v) = %d, want %d", c.frac, got, c.want)
		}
	}
}

func TestSizeRiskMode(t *testing.T) {
	cfg := Config{RiskPerTradePct: 1}
	rule := Rule{MarginUSDT: 35, Leverage: 125}

	t.Run("stop-bound leverage: 1% stop caps 125x down to 50x", func(t *testing.T) {
		s := Size(cfg, rule, 100, 99, 400, true)
		// risk 4u; notional 4/0.01 = 400u; lev min(125, 50) = 50; margin 8u; qty 4
		if s.Blocked || s.Lev != 50 || !near(s.RiskUSDT, 4) || !near(s.Notional, 400) ||
			!near(s.Margin, 8) || !near(s.Qty, 4) || !near(s.Equity, 400) {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("rule-bound leverage: a 0.2% stop allows 250x, the rule's 125x wins", func(t *testing.T) {
		s := Size(cfg, rule, 100, 99.8, 400, true)
		// notional 4/0.002 = 2000u; lev 125; margin 16u; qty 20
		if s.Blocked || s.Lev != 125 || !near(s.Notional, 2000) || !near(s.Margin, 16) || !near(s.Qty, 20) {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("short side sizes the same as long", func(t *testing.T) {
		long, short := Size(cfg, rule, 100, 99, 400, true), Size(cfg, rule, 100, 101, 400, true)
		if long.Lev != short.Lev || !near(long.Qty, short.Qty) || !near(long.Margin, short.Margin) {
			t.Fatalf("long %+v short %+v", long, short)
		}
	})
	t.Run("margin_usdt is not read in risk mode", func(t *testing.T) {
		a := Size(cfg, Rule{MarginUSDT: 35, Leverage: 125}, 100, 99, 400, true)
		b := Size(cfg, Rule{MarginUSDT: 999, Leverage: 125}, 100, 99, 400, true)
		if !near(a.Qty, b.Qty) || !near(a.Margin, b.Margin) {
			t.Fatalf("margin_usdt leaked into sizing: %+v vs %+v", a, b)
		}
	})
	t.Run("a stop too wide for even 1x is blocked", func(t *testing.T) {
		s := Size(cfg, rule, 100, 40, 400, true) // 60%: maxLev 0
		if !s.Blocked || !strings.Contains(s.Reason, "liq-guard") {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("unreadable equity blocks instead of falling back to fixed margin", func(t *testing.T) {
		for _, eq := range []struct {
			v     float64
			known bool
		}{{0, false}, {400, false}, {0, true}} {
			s := Size(cfg, rule, 100, 99, eq.v, eq.known)
			if !s.Blocked || !strings.Contains(s.Reason, "equity") {
				t.Fatalf("equity %+v: got %+v", eq, s)
			}
		}
	})
	t.Run("the stop is always inside half the liquidation distance, and the loss at the stop is the risk", func(t *testing.T) {
		for _, stop := range []float64{99.9, 99.6, 99.2, 98.5, 97, 94, 80} {
			s := Size(cfg, rule, 100, stop, 400, true)
			if s.Blocked {
				t.Fatalf("stop %v unexpectedly blocked: %s", stop, s.Reason)
			}
			liqPct := 100 / float64(s.Lev)
			if s.StopPct > LiqStopMaxFrac*liqPct+1e-9 {
				t.Errorf("stop %v: %.3f%% stop vs %.3f%% liquidation at %dx", stop, s.StopPct, liqPct, s.Lev)
			}
			// The quantity that is fixed is the LOSS, not the margin: qty x
			// stop distance must be the 4u risk whatever the stop width.
			if loss := s.Qty * math.Abs(100-stop); !near(loss, s.RiskUSDT) || !near(s.Margin*float64(s.Lev), s.Notional) {
				t.Errorf("stop %v: loss %.4f vs risk %.4f, margin*lev %.4f vs notional %.4f",
					stop, loss, s.RiskUSDT, s.Margin*float64(s.Lev), s.Notional)
			}
		}
	})
}

func TestSizeFixedMarginMode(t *testing.T) {
	cfg := Config{} // RiskPerTradePct 0: the pre-2026-10-16 sizing
	rule := Rule{MarginUSDT: 35, Leverage: 125}

	t.Run("a stop inside the guard keeps margin x leverage exactly", func(t *testing.T) {
		s := Size(cfg, rule, 100, 99.7, 0, false) // 0.3%: maxLev 166 >= 125
		// notional 35*125 = 4375; qty 43.75; risk 4375*0.003 = 13.125
		if s.Blocked || s.Lev != 125 || !near(s.Notional, 4375) || !near(s.Qty, 43.75) ||
			!near(s.Margin, 35) || !near(s.RiskUSDT, 13.125) {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("a stop past the guard is blocked, not quietly re-levered", func(t *testing.T) {
		s := Size(cfg, rule, 100, 99, 0, false) // 1%: maxLev 50 < 125
		if !s.Blocked || !strings.Contains(s.Reason, "liq-guard") || !strings.Contains(s.Reason, "max 50x") {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("fixed mode does not need equity", func(t *testing.T) {
		if s := Size(cfg, rule, 100, 99.7, 0, false); s.Blocked {
			t.Fatalf("got %+v", s)
		}
	})
}

func TestSizeNoStop(t *testing.T) {
	for _, c := range [][2]float64{{100, 0}, {0, 99}, {100, 100}} {
		if s := Size(Config{RiskPerTradePct: 1}, Rule{Leverage: 10}, c[0], c[1], 400, true); !s.Blocked {
			t.Errorf("entry %v stop %v: want blocked, got %+v", c[0], c[1], s)
		}
	}
}
