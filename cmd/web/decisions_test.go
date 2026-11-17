package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/decision"
)

// Hand-traced. Exits in order: take +2R/+20u, skip -1R/-10u, none -1R/-10u,
// take -1R/-10u.
//
//	執行 (take):  cum R 2, 1        → R +1, u +10, maxDD R 1, u 10
//	全部 (all):   cum R 2, 1, 0, -1 → R -1, u -10, maxDD R 3, u 30
//	否決 (skip):  cum R -1          → R -1, u -10, maxDD R 1, u 10
func TestDecisionCurvesHandTraced(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	ev := []curveEvent{ // deliberately out of order: the function sorts by exit
		{At: t0.Add(4 * time.Hour), Group: decision.Take, R: -1, U: -10},
		{At: t0.Add(1 * time.Hour), Group: decision.Take, R: 2, U: 20},
		{At: t0.Add(3 * time.Hour), Group: "none", R: -1, U: -10},
		{At: t0.Add(2 * time.Hour), Group: decision.Skip, R: -1, U: -10},
	}
	h := decisionCurves(ev)
	if !h["InU"].(bool) {
		t.Fatal("every event has u: the chart should be in u")
	}
	want := map[string][5]float64{ // N, R, U, maxDD R, maxDD U
		decision.Take: {2, 1, 10, 1, 10},
		"all":         {4, -1, -10, 3, 30},
		decision.Skip: {1, -1, -10, 1, 10},
	}
	for _, c := range h["Lines"].([]*decisionCurve) {
		w := want[c.Key]
		got := [5]float64{float64(c.N), c.R, c.U, c.MaxDDR, c.MaxDDU}
		if got != w {
			t.Errorf("%s: got %v want %v", c.Key, got, w)
		}
	}
	ev[0].U = 0 // one fire without sizing: u is not comparable, chart in R
	if decisionCurves(ev)["InU"].(bool) {
		t.Fatal("an unsized settled event must force the chart to R")
	}
}
