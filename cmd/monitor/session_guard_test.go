package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autostrat"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/session"
)

// 2026-09-10 is a Thursday, so its cash open is a real one (NextCashOpen
// skips weekends). Derived from session.CashOpen rather than hardcoded as a
// UTC instant, so the fixture stays correct across the DST boundary.
func cashOpenOn(t *testing.T, y int, mo time.Month, d int) time.Time {
	t.Helper()
	ref := time.Date(y, mo, d, 12, 0, 0, 0, time.UTC)
	o := session.CashOpen(ref)
	if o.IsZero() {
		t.Fatalf("CashOpen(%v) returned zero", ref)
	}
	return o
}

func TestAutoSessionApplies(t *testing.T) {
	open := cashOpenOn(t, 2026, 9, 10)
	trig := autostrat.Trigger{Side: "long", Entry: 1700, Stop: 1691.5} // 0.5% stop

	cases := []struct {
		name string
		sym  market.Symbol
		tf   string
		trig autostrat.Trigger
		now  time.Time
		want bool
	}{
		// The case that killed #62 and #67: an entry landing in the run-up
		// to the bell with a stop inside the open bar's travel.
		{"29m before open, 1h rule", market.SNDKUSDT, "1h", trig, open.Add(-29 * time.Minute), true},
		{"1m before open", market.SNDKUSDT, "1h", trig, open.Add(-time.Minute), true},
		// Exactly one bar out is still in scope; a minute past it is not,
		// because the rule gets another look before the bell.
		{"exactly 1h before, 1h rule", market.SNDKUSDT, "1h", trig, open.Add(-time.Hour), true},
		{"61m before, 1h rule", market.SNDKUSDT, "1h", trig, open.Add(-61 * time.Minute), false},
		// The horizon scales with the rule's timeframe.
		{"90m before, 2h rule", market.SNDKUSDT, "2h", trig, open.Add(-90 * time.Minute), true},
		{"90m before, 1h rule", market.SNDKUSDT, "1h", trig, open.Add(-90 * time.Minute), false},
		// Hours after the bell the open bar is finished; nothing to guard.
		{"mid-session", market.SNDKUSDT, "1h", trig, open.Add(4 * time.Hour), false},
		// Crypto and metals follow other sessions entirely.
		{"BTC", market.BTCUSDT, "1h", trig, open.Add(-29 * time.Minute), false},
		{"XAG", market.XAGUSDT, "1h", trig, open.Add(-29 * time.Minute), false},
		// A trigger with no usable geometry cannot be judged.
		{"no entry", market.SNDKUSDT, "1h", autostrat.Trigger{Stop: 1691.5}, open.Add(-29 * time.Minute), false},
		{"no stop", market.SNDKUSDT, "1h", autostrat.Trigger{Entry: 1700}, open.Add(-29 * time.Minute), false},
		// An unrecognised timeframe has no next-reaction granularity, so
		// there is no horizon to apply.
		{"unknown tf", market.SNDKUSDT, "3h", trig, open.Add(-29 * time.Minute), false},
		{"empty tf", market.SNDKUSDT, "", trig, open.Add(-29 * time.Minute), false},
	}
	for _, c := range cases {
		untilOpen, got := autoSessionApplies(c.sym, c.tf, c.trig, c.now)
		if got != c.want {
			t.Errorf("%s: autoSessionApplies(%s, %q) = %v (untilOpen %v), want %v",
				c.name, c.sym, c.tf, got, untilOpen, c.want)
		}
	}
}

// The in-scope branch must report a positive untilOpen, since StopWarning
// prints it ("next cash open in 29m") and a zero would make it claim the
// open is already in progress.
func TestAutoSessionAppliesReportsUntilOpen(t *testing.T) {
	open := cashOpenOn(t, 2026, 9, 10)
	now := open.Add(-29 * time.Minute)
	untilOpen, applies := autoSessionApplies(
		market.SNDKUSDT, "1h",
		autostrat.Trigger{Entry: 1700, Stop: 1691.5}, now)
	if !applies {
		t.Fatal("expected the 29m-before case to be in scope")
	}
	if untilOpen < 28*time.Minute || untilOpen > 30*time.Minute {
		t.Errorf("untilOpen = %v, want ~29m", untilOpen)
	}
}
