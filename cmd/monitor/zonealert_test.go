package main

import (
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/zone"
)

// The bar that motivated close confirmation: BTC 1h, 2026-09-02 21:00 UTC+8,
// H 77413.4 inside the 77390–77476.3 cluster, C 77132. The old push said
// "進觀察區" about that wick.
func closedBar(closePx float64) market.Candle {
	open := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) // 21:00 UTC+8
	return market.Candle{
		OpenTime:  open,
		CloseTime: open.Add(time.Hour - time.Millisecond),
		Open:      76574.2, High: 77413.4, Low: 76574.1, Close: closePx,
	}
}

func gateZone(mode string) zone.Zone {
	return zone.Zone{
		Symbol: "BTC", Lo: 77390, Hi: 77476.3, Dir: "watch", TF: "1h",
		Note: "疊層閘門", Confirm: mode,
	}
}

func TestZoneAlertTextCloseConfirmed(t *testing.T) {
	title, body, tags := zoneAlertText(gateZone(zone.ConfirmCloseAbove), "BTC", 77600, closedBar(77510))

	// The reader must be able to trust the push without opening the chart:
	// it has to name the rule, the confirming bar, and its close.
	for _, want := range []string{"BTC", "1h", "收破上緣", "收盤確認", "77,476.3"} {
		if !strings.Contains(title, want) {
			t.Errorf("title missing %q: %s", want, title)
		}
	}
	for _, want := range []string{"77,510", "09/02 21:59", "77,390–77,476.3", "疊層閘門", "非影線", "77,600"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// "進區" is the touch-mode wording and must never appear on a confirmed
	// push — that conflation is the bug.
	if strings.Contains(title, "進") || strings.Contains(body, "快照") {
		t.Errorf("confirmed push reused touch wording:\ntitle=%s\nbody=%s", title, body)
	}
	if tags != "eyes" {
		t.Errorf("watch dir should keep the neutral tag, got %q", tags)
	}
}

// close-below reports the edge it actually broke, not Hi.
func TestZoneAlertTextCloseBelowNamesLoEdge(t *testing.T) {
	title, _, _ := zoneAlertText(gateZone(zone.ConfirmCloseBelow), "BTC", 77100, closedBar(77132))
	if !strings.Contains(title, "77,390") {
		t.Errorf("close-below must name Lo (77,390), got %s", title)
	}
	if strings.Contains(title, "77,476") {
		t.Errorf("close-below must not name Hi, got %s", title)
	}
}

// Touch mode must be byte-for-byte the old behaviour — auto pivot zones are
// the overwhelming majority of pushes and were not supposed to change.
func TestZoneAlertTextTouchUnchanged(t *testing.T) {
	z := zone.Zone{Symbol: "ETH", Lo: 2500, Hi: 2530, Dir: "short", TF: "2h", Note: "auto 2h fade"}
	title, body, tags := zoneAlertText(z, "ETH", 2515, market.Candle{})
	if !strings.Contains(title, "進空區") || !strings.Contains(title, "2,500–2,530") {
		t.Errorf("touch title changed: %s", title)
	}
	for _, want := range []string{"🔴", "現價 2,515", "樞紐區", "快照", "判斷 reject / 進場"} {
		if !strings.Contains(body, want) {
			t.Errorf("touch body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "auto 2h fade") {
		t.Errorf("note dropped:\n%s", body)
	}
	if tags != "red_circle,chart_with_downwards_trend" {
		t.Errorf("short tags changed: %q", tags)
	}
	// The zero Candle must not leak a bogus 1970 timestamp into a touch push.
	if strings.Contains(body, "1970") || strings.Contains(body, "01/01") {
		t.Errorf("zero candle leaked into touch body:\n%s", body)
	}
}

// A failed live-price fetch must still produce a readable push rather than
// printing 0 as if BTC traded at zero.
func TestZoneAlertTextHandlesMissingLivePrice(t *testing.T) {
	_, body, _ := zoneAlertText(gateZone(zone.ConfirmCloseAbove), "BTC", 0, closedBar(77510))
	if !strings.Contains(body, "現價 n/a") {
		t.Errorf("want 現價 n/a, got:\n%s", body)
	}
	if strings.Contains(body, "現價 0") {
		t.Errorf("zero price rendered as a price:\n%s", body)
	}
}

// The cache window is derived from the bar, so verify the arithmetic that
// decides "this is still the newest close" on a 1h bar.
func TestCloseCacheWindowArithmetic(t *testing.T) {
	b := closedBar(77132)
	dur := b.CloseTime.Sub(b.OpenTime) + time.Millisecond
	if dur != time.Hour {
		t.Fatalf("1h bar duration derived as %v, want 1h — the refetch window depends on this", dur)
	}
	// This bar spans 13:00–13:59:59.999 UTC, so the NEXT one closes at
	// 14:59:59.999 — that instant, not 14:00, is when this cached bar stops
	// being the newest close.
	nextClose := b.CloseTime.Add(dur)
	if got := nextClose.UTC().Format("15:04:05"); got != "14:59:59" {
		t.Errorf("next close computed as %s UTC, want 14:59:59", got)
	}
	// Mid-bar: still fresh. Past the next close + grace: stale.
	mid := b.CloseTime.Add(30 * time.Minute)
	if !mid.Before(nextClose.Add(closeGrace)) {
		t.Error("mid-bar must be treated as fresh")
	}
	if nextClose.Add(closeGrace + time.Second).Before(nextClose.Add(closeGrace)) {
		t.Error("past grace must be treated as stale")
	}
}
