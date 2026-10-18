package main

import (
	"testing"
	"time"
)

func TestHumanSince(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{16 * time.Hour, "16h 0m"},
		{50 * time.Hour, "2d 2h"},
		{-time.Second, ""},
	} {
		if got := humanSince(tc.d); got != tc.want {
			t.Errorf("humanSince(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
