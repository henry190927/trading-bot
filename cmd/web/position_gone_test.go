package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/alert"
	"github.com/henry190927/trading-bot/journal"
)

// prev {1:1, 2:3}; active 1,2,3; only 2 present →
// 1 absent again → 2; 2 present → dropped; 3 absent for the first time → 1.
func TestNextMisses(t *testing.T) {
	got := nextMisses(map[int]int{1: 1, 2: 3, 9: 5}, []int{1, 2, 3}, map[int]bool{2: true})
	want := map[int]int{1: 2, 3: 1} // 9 is no longer active, so it is gone too
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// One empty read is not "gone"; the second consecutive one is, dated from the
// first; a read that finds the position clears it.
func TestSetGoneNeedsTwoReads(t *testing.T) {
	var s server
	tr := []journal.Trade{{ID: 93, Symbol: "SUI", Side: "long"}}
	t1, t2 := time.Unix(1000, 0), time.Unix(1030, 0)

	s.setGone(tr, map[int]bool{}, t1)
	if g, known := s.goneSnapshot(); len(g) != 0 || !known {
		t.Fatalf("after one miss: %v known=%v, want none, known", g, known)
	}
	s.setGone(tr, map[int]bool{}, t2)
	g, _ := s.goneSnapshot()
	if len(g) != 1 || g[0].ID != 93 || !g[0].Since.Equal(t1) {
		t.Fatalf("after two misses: %+v, want #93 since the first miss", g)
	}
	s.markGoneUnknown() // a failed read keeps the verdict but says so
	if g, known := s.goneSnapshot(); len(g) != 1 || known {
		t.Fatalf("after a failed read: %v known=%v", g, known)
	}
	s.setGone(tr, map[int]bool{93: true}, t2.Add(30*time.Second))
	if g, _ := s.goneSnapshot(); len(g) != 0 {
		t.Fatalf("position back: %v, want none", g)
	}
}

func TestPositionGoneAlert(t *testing.T) {
	in := healthy()
	in.gone = []goneTrade{{ID: 93, Symbol: "SUI", Side: "long"}}
	firing, held := evaluateAlerts(in)
	if got := keys(firing)["position_gone:93"]; got != alert.Warning {
		t.Fatalf("firing %v, want position_gone:93 as a warning", firing)
	}
	if len(held) != 0 {
		t.Errorf("held %v with a successful read", held)
	}
	if !strings.Contains(firing[0].Title, "#93 SUI 多") {
		t.Errorf("title %q", firing[0].Title)
	}

	// A failed positions read must hold the prefix, so open incidents are
	// not resolved by an outage.
	in.gone, in.goneKnown = nil, false
	_, held = evaluateAlerts(in)
	if !reflect.DeepEqual(held, []string{"position_gone:"}) {
		t.Errorf("held %v, want [position_gone:]", held)
	}

	push := func(kind string) bool {
		return alertPushes(alert.Change{Kind: kind, Alert: alert.Alert{Key: "position_gone:93", Severity: alert.Warning}})
	}
	if !push("fire") || push("resolve") {
		t.Errorf("pushes: fire=%v resolve=%v, want fire only", push("fire"), push("resolve"))
	}
}
