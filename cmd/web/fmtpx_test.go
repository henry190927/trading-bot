package main

import "testing"

func TestFmtPx(t *testing.T) {
	for v, want := range map[float64]string{
		83699.8115: "83,699.81",
		2737.6057:  "2,737.61",
		101.99:     "101.99",
		100:        "100.00",
		1.5303:     "1.5303",
		0.6921:     "0.6921",
		0.0619236:  "0.061924",
		0:          "-",
		-4310.8647: "-4,310.86",
		1234567.5:  "1,234,567.50",
	} {
		if got := fmtPx(v); got != want {
			t.Errorf("fmtPx(%v) = %q, want %q", v, got, want)
		}
	}
}
