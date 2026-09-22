package main

import (
	"strings"
	"testing"
	"time"
)

// XRP as it actually stood on 2026-09-21: 3217 @ 1.4810, 100x cross, no stop,
// six hours in, equity 593.08.
func xrpAlert(naked time.Duration, nag int, mark float64) nakedAlert {
	return nakedAlert{
		Short: "XRP", Side: "long", Qty: 3217, Entry: 1.4810, Mark: mark,
		Leverage: 100, MarginMode: "cross", Equity: 593.08,
		Naked: naked, Nag: nag, Journalled: true,
	}
}

// The whole point: consecutive pushes must not look the same. 132 identical
// ones went out over six hours and the position stayed naked.
func TestConsecutiveAlertsDiffer(t *testing.T) {
	a := xrpAlert(6*time.Hour+12*time.Minute, 13, 1.4958)
	b := xrpAlert(6*time.Hour+27*time.Minute, 14, 1.5219)

	if a.Title() == b.Title() {
		t.Errorf("titles identical: %q — the title is the line a collapsed "+
			"notification shows, so two pushes differing only below it are two identical pushes", a.Title())
	}
	if a.Body() == b.Body() {
		t.Error("bodies identical")
	}
	// The escalating facts have to be IN the title, not merely somewhere.
	for _, want := range []string{"6h12m", "第 13 次"} {
		if !strings.Contains(a.Title(), want) {
			t.Errorf("title %q missing %q", a.Title(), want)
		}
	}
}

func TestAlertCarriesTheDecisionNumbers(t *testing.T) {
	a := xrpAlert(6*time.Hour+12*time.Minute, 13, 1.5219)
	body := a.Body()
	// Exposure at the MARK, not at entry: 3217 x 1.5219 = 4896u. Reporting the
	// entry notional keeps saying 4764u after the position has run.
	for _, want := range []string{"4896u", "100x", "cross", "mark 1.5219", "+131.58u"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "距爆倉") {
		t.Errorf("body missing the liquidation distance:\n%s", body)
	}
	if !strings.Contains(body, "stop = 0") {
		t.Errorf("a journalled-but-stopless position must say why this is its only alarm:\n%s", body)
	}
}

// Mark and equity are separate reads that can fail. The alarm must still go —
// suppressing it because a secondary lookup timed out is the worst outcome.
func TestAlertDegradesWithoutMarkOrEquity(t *testing.T) {
	a := nakedAlert{Short: "XRP", Side: "long", Qty: 3217, Entry: 1.4810,
		Leverage: 100, MarginMode: "cross", Naked: time.Hour, Nag: 2}
	title, body := a.Title(), a.Body()
	if title == "" || body == "" {
		t.Fatal("alert must still render with no mark and no equity")
	}
	if strings.Contains(body, "距爆倉") {
		t.Error("liquidation distance printed with no equity — that would be a fabricated number")
	}
	if strings.Contains(body, "mark") || strings.Contains(body, "浮動") {
		t.Error("mark-derived lines printed with no mark")
	}
	// Falls back to the entry notional: 3217 x 1.4810 = 4764u.
	if !strings.Contains(body, "4764u") {
		t.Errorf("body should still size the position from entry:\n%s", body)
	}
}

func TestKillDistanceMatchesTheRealLiquidation(t *testing.T) {
	// NEAR 2026-09-18: short 1335 @ 3.535, equity 192.12 — liquidated at 3.677.
	a := nakedAlert{Short: "NEAR", Side: "short", Qty: 1335, Entry: 3.535, Mark: 3.535,
		Leverage: 100, MarginMode: "cross", Equity: 192.1184, Naked: time.Hour, Nag: 1}
	got := a.killPrice()
	if got < 3.670 || got > 3.686 {
		t.Errorf("kill price estimate %v, want within a few ticks of the actual 3.677", got)
	}
	if f := a.killFrac(); f < 0.040 || f > 0.042 {
		t.Errorf("kill distance %v, want ~4.07%%", f)
	}
}

func TestShortDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{time.Hour + 2*time.Minute, "1h02m"},
		{9*time.Hour + 18*time.Minute, "9h18m"},
	} {
		if got := shortDur(c.d); got != c.want {
			t.Errorf("shortDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// A short's liquidation is ABOVE the mark. Getting the sign wrong would print
// a reassuring number in the exact situation the alarm exists for.
func TestShortLiquidationIsAbove(t *testing.T) {
	a := nakedAlert{Short: "X", Side: "short", Qty: 100, Entry: 10, Mark: 10,
		Equity: 200, Naked: time.Hour, Nag: 1}
	if a.killPrice() <= a.Mark {
		t.Errorf("short kill price %v must be above the mark %v", a.killPrice(), a.Mark)
	}
	if a.unrealised() != 0 {
		t.Errorf("flat short should show 0 unrealised, got %v", a.unrealised())
	}
	b := a
	b.Mark = 9 // a short in profit
	if b.unrealised() <= 0 {
		t.Errorf("short at 9 from 10 should be in profit, got %v", b.unrealised())
	}
}
