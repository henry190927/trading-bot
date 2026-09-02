package main

import (
	"testing"
	"time"
)

// The breaker must announce once per UTC day and re-arm on the date rolling.
func TestHaltStateAnnounce(t *testing.T) {
	var h haltState
	d1 := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	d1later := time.Date(2026, 9, 2, 23, 59, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 3, 0, 1, 0, 0, time.UTC)

	if !h.announce(d1) {
		t.Fatal("first trip of the day must announce")
	}
	if h.announce(d1) || h.announce(d1later) {
		t.Error("repeat trips the same UTC day must stay silent (no 60s spam)")
	}
	if !h.announce(d2) {
		t.Error("a new UTC day must announce again — this is the re-arm")
	}
}

func TestCapStr(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "∞"}, // unset/disabled must not read as a cap of zero
		{2, "2"},
		{100, "100"},
		{-3, "-3"},
	} {
		if got := capStr(tc.in); got != tc.want {
			t.Errorf("capStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
