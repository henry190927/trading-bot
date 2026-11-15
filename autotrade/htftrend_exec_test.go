package autotrade

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Per-rule risk_pct (docs/htf_trend_executor_design.md §4.1). Hand-traced:
// equity 400, entry 100, stop 94 (6%) → maxLev floor(0.5/0.06) = 8.
func TestSizeRuleRiskPct(t *testing.T) {
	cfg := Config{RiskPerTradePct: 1.5}
	trend := Rule{Leverage: 20, RiskPct: 0.25}
	fade := Rule{Leverage: 20}

	s := Size(cfg, trend, 100, 94, 400, true)
	// risk 400×0.25% = 1u; notional 1/0.06 = 16.667u; lev min(20, 8) = 8.
	if s.Blocked || !near(s.RiskUSDT, 1) || !near(s.Notional, 1/0.06) || s.Lev != 8 || !near(s.Margin, 1/0.06/8) {
		t.Errorf("rule 0.25%%: %+v, want risk 1u, notional 16.67u at 8x", s)
	}
	if s := Size(cfg, fade, 100, 94, 400, true); !near(s.RiskUSDT, 6) {
		t.Errorf("rule without risk_pct: risk %v, want the book's 1.5%% = 6u", s.RiskUSDT)
	}
	// Book in fixed-margin mode, rule on risk: the rule sizes from equity and
	// an unreadable balance blocks it, as in risk mode.
	fixed := Config{}
	if s := Size(fixed, trend, 100, 94, 400, true); !near(s.RiskUSDT, 1) {
		t.Errorf("fixed-margin book, rule 0.25%%: risk %v, want 1u", s.RiskUSDT)
	}
	if s := Size(fixed, trend, 100, 94, 0, false); !s.Blocked || !strings.Contains(s.Reason, "equity") {
		t.Errorf("rule risk_pct with no equity: %+v, want blocked on equity", s)
	}
}

func TestNeedsEquity(t *testing.T) {
	trend := Rule{Enabled: true, RiskPct: 0.25}
	off := Rule{Enabled: false, RiskPct: 0.25}
	plain := Rule{Enabled: true}
	for _, tc := range []struct {
		cfg  Config
		want bool
	}{
		{Config{Rules: []Rule{plain}}, false},
		{Config{RiskPerTradePct: 1, Rules: []Rule{plain}}, true},
		{Config{Rules: []Rule{plain, trend}}, true}, // the case the old `cfg.RiskPerTradePct > 0` missed
		{Config{Rules: []Rule{plain, off}}, false},  // a disabled rule needs nothing
	} {
		if got := tc.cfg.NeedsEquity(); got != tc.want {
			t.Errorf("NeedsEquity(%+v) = %v, want %v", tc.cfg, got, tc.want)
		}
	}
}

// A StrategyCaps group is its own budget: fades do not see its legs, it does
// not see theirs, and it has its own count. Halt and margin stay book-wide.
func TestStrategyCapsSeparateBudget(t *testing.T) {
	cfg := Config{
		MaxConcurrentTotal: 2, MaxSameSymbolSide: 1, MaxMarginTotalUSDT: 100, DailyLossHaltR: -6,
		StrategyCaps: map[string]StrategyCap{"htf-trend": {Strategies: []string{"htf-trend", "htf-trend-1d"}, MaxConcurrent: 3}},
	}
	fade := func(sym, side string) Leg { return Leg{Symbol: sym, Side: side, Strategy: "engine"} }
	trend := func(sym, side, s string) Leg { return Leg{Symbol: sym, Side: side, Strategy: s} }
	book := func(legs ...Leg) Book {
		return Book{OpenCount: len(legs), OpenMargin: 10 * float64(len(legs)), OpenLegs: legs}
	}

	// Two fades fill the shared cap; a trend fire still opens, and an ETH
	// long trend fire opens alongside an ETH long fade (same-symbol-side is
	// the fades' rule).
	bk := book(fade("ETH", "long"), fade("BTC", "short"))
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "ETH", Side: "long", Margin: 5, Strategy: "htf-trend"}); v.Blocked {
		t.Errorf("trend fire with the shared cap full: %s, want allowed", v.Reason)
	}
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "SOL", Side: "long", Margin: 5, Strategy: "engine"}); !v.Blocked || !strings.HasPrefix(v.Reason, "max-concurrent-total") {
		t.Errorf("third fade: %+v, want max-concurrent-total", v)
	}
	// Trend legs do not use up the fades' slots or same-side cap.
	bk = book(fade("ETH", "long"), trend("BTC", "long", "htf-trend"), trend("ETH", "short", "htf-trend-1d"), trend("SOL", "long", "htf-trend"))
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "ETH", Side: "short", Margin: 5, Strategy: "sweep-reject"}); v.Blocked {
		t.Errorf("second fade with 3 trend legs open: %s, want allowed (1 shared open < 2)", v.Reason)
	}
	// 4h and 1d count together in the group: 3 open = cap.
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "XRP", Side: "long", Margin: 5, Strategy: "htf-trend-1d"}); !v.Blocked || !strings.HasPrefix(v.Reason, "strategy-cap htf-trend") {
		t.Errorf("fourth trend leg: %+v, want strategy-cap htf-trend", v)
	}
	// Within the group 4h and 1d may hold the same symbol and side.
	bk = book(trend("BTC", "long", "htf-trend"))
	if v := CheckCaps(cfg, bk, Candidate{Symbol: "BTC", Side: "long", Margin: 5, Strategy: "htf-trend-1d"}); v.Blocked {
		t.Errorf("BTC long 1d beside BTC long 4h: %s, want allowed", v.Reason)
	}
	// Book-wide guards still apply to the group.
	if v := CheckCaps(cfg, Book{RealizedRToday: -6}, Candidate{Symbol: "BTC", Side: "long", Strategy: "htf-trend"}); !v.Halt {
		t.Error("the daily halt must stop the trend group too")
	}
	if v := CheckCaps(cfg, Book{OpenMargin: 98}, Candidate{Symbol: "BTC", Side: "long", Margin: 5, Strategy: "htf-trend"}); !v.Blocked || !strings.HasPrefix(v.Reason, "max-margin-total") {
		t.Errorf("margin: %+v, want the book-wide margin cap", v)
	}
	// Without StrategyCaps everything is shared, exactly as before.
	plain := cfg
	plain.StrategyCaps = nil
	bk = book(fade("ETH", "long"), fade("BTC", "short"))
	if v := CheckCaps(plain, bk, Candidate{Symbol: "SOL", Side: "long", Margin: 5, Strategy: "htf-trend"}); !v.Blocked {
		t.Error("no StrategyCaps: a trend fire must share max-concurrent-total")
	}
	// Zero MaxConcurrent = unlimited.
	unl := cfg
	unl.StrategyCaps = map[string]StrategyCap{"htf-trend": {}}
	bk = book(trend("A", "long", "htf-trend"), trend("B", "long", "htf-trend"), trend("C", "long", "htf-trend"))
	if v := CheckCaps(unl, bk, Candidate{Symbol: "D", Side: "long", Margin: 5, Strategy: "htf-trend"}); v.Blocked {
		t.Errorf("group cap 0: %s, want unlimited", v.Reason)
	}
	// htf-trend-1d is NOT in a group keyed "htf-trend" with no list.
	if unl.capGroup("htf-trend-1d") != "" || unl.capGroup("htf-trend") != "htf-trend" {
		t.Error("an empty Strategies list must mean the key itself only")
	}
}

// A trail fire older than the read depth stays in the book; a fixed-bracket
// one does not, and with no trail fire the read equals ReadFires.
func TestReadFiresWithOpenTrails(t *testing.T) {
	t.Setenv("AUTOTRADE_PAPER_LOG", filepath.Join(t.TempDir(), "paper.jsonl"))
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	AppendFire(PaperFire{Time: at(0), Symbol: "BTC", Strategy: "htf-trend", TrailM: 10})
	AppendFire(PaperFire{Time: at(1), Symbol: "ETH", Strategy: "engine"})
	AppendFire(PaperFire{Time: at(2), Symbol: "SOL", Strategy: "htf-trend", TrailM: 10})
	AppendFire(PaperFire{Time: at(3), Symbol: "SOL", Strategy: "htf-trend", TrailM: 10})
	for i := 0; i < 3; i++ {
		AppendFire(PaperFire{Time: at(10 + i), Symbol: "XRP", Strategy: "vp-node"})
	}
	got := ReadFiresWithOpenTrails(3)
	var keys []string
	for _, f := range got {
		keys = append(keys, f.Symbol+"@"+f.Time.Format("15"))
	}
	// Window = the 3 XRP rows; then SOL's NEWEST (03h, not 02h) and BTC's;
	// ETH engine is fixed-bracket and stays out.
	if want := "XRP@12 XRP@11 XRP@10 SOL@03 BTC@00"; strings.Join(keys, " ") != want {
		t.Errorf("read = %s, want %s", strings.Join(keys, " "), want)
	}
	if f, ok := NewestFire("SOL", "htf-trend"); !ok || !f.Time.Equal(at(3)) {
		t.Errorf("NewestFire(SOL) = %v %v, want the 03h row", f.Time, ok)
	}
	if _, ok := NewestFire("SOL", "htf-trend-1d"); ok {
		t.Error("NewestFire found a rule that never fired")
	}
	if a, b := ReadFires(10), ReadFiresWithOpenTrails(10); len(a) != len(b) {
		t.Errorf("whole log inside the depth: %d vs %d rows, want equal", len(a), len(b))
	}
}

func TestTrailStrategyNames(t *testing.T) {
	for name, tf := range map[string]string{"htf-trend": "4h", "htf-trend-1d": "1d", "engine": ""} {
		if TrailStrategyTF(name) != tf || IsTrailStrategy(name) != (tf != "") {
			t.Errorf("%s: TF %q trail %v", name, TrailStrategyTF(name), IsTrailStrategy(name))
		}
	}
	if u := (Config{Rules: []Rule{{Strategy: "htf-trend"}, {Strategy: "htf-trend-1d"}}}).UnknownStrategies(); len(u) != 0 {
		t.Errorf("trail strategies reported unknown: %v", u)
	}
}
