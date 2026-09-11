package main

import (
	"strings"
	"testing"

	"myFirstGo/trading-bot/market"

	"github.com/gin-gonic/gin"
)

// tfScores builds the computeAlignment input in biasStripTFs order.
func tfScores(m5, m15, h1, h2, h4 int) []gin.H {
	return []gin.H{
		{"tf": "5m", "score": m5},
		{"tf": "15m", "score": m15},
		{"tf": "1h", "score": h1},
		{"tf": "2h", "score": h2},
		{"tf": "4h", "score": h4},
	}
}

// Every case below is hand-traced against the rule: conflict first (HTF sum
// sign vs LTF sum sign), then dom = sign(total), then breadth
// (broad && agree>against && !1h-dissent), where broad = agree>=3 or an
// unopposed HTF-led pair. Tightened 2026-09-11 — see the three cases at the
// end of the table.
func TestComputeAlignment(t *testing.T) {
	cases := []struct {
		name                   string
		m5, m15, h1, h2, h4    int
		wantState              string
		wantAgree, wantAgainst int
	}{
		// The live BTC read on 2026-09-01 that exposed the bug: one 15m vote,
		// four neutrals. htfSum 0 (no conflict), total -1, agree 1 → weak.
		{"one LTF lean only", 0, -1, 0, 0, 0, "weak-short", 1, 0},
		// The live ETH read the same morning: 5m/15m/1h/2h all -1, 4h neutral.
		// htfSum -1, ltfSum -3 (same sign), total -4, agree 4, htfAgree 1 → strong.
		{"four TFs short incl HTF", -1, -1, -1, -1, 0, "aligned-short", 4, 0},
		// Two scalp TFs alone: htfSum 0, total -2, agree 2, htfAgree 0,
		// agree>=3 false → weak (scalp-only is not a trend).
		{"scalp-only pair", -1, -1, 0, 0, 0, "weak-short", 2, 0},
		// A single 4h vote: total -1, agree 1 → weak.
		{"one HTF lean only", 0, 0, 0, 0, -1, "weak-short", 1, 0},
		// Whole LTF stack agrees, HTFs neutral: agree 3 → strong via agree>=3.
		{"full LTF stack", -1, -1, -1, 0, 0, "aligned-short", 3, 0},
		// HTF-driven: htfSum -3, ltfSum 0, agree 2 (2h,4h), htfAgree 2 → strong.
		{"both HTFs long", 0, 0, 0, 2, 1, "aligned-long", 2, 0},
		// HTF vs LTF opposition — conflict wins regardless of magnitude.
		{"htf up ltf down", -1, -1, -1, 1, 0, "conflict", 0, 0},
		// All neutral: dom 0 → mixed.
		{"all flat", 0, 0, 0, 0, 0, "mixed", 0, 0},
		// Within-LTF split, no HTF: htfSum 0 so no conflict; total -1, dom -1,
		// agree 2 (15m,1h), against 1 (5m), htfAgree 0, agree>=3 false → weak.
		{"ltf split no htf", 1, -1, -1, 0, 0, "weak-short", 2, 1},
		// Heavy dominant pair vs equal-count light opposition: htfSum -3,
		// ltfSum -1 (same sign, no conflict), total -4, agree 2 (1h,2h),
		// against 2 (5m,15m) → agree>against fails → weak.
		{"tied counts heavy dom", 1, 1, -3, -3, 0, "weak-short", 2, 2},
		// Total nets to zero with real votes on both sides inside the LTF
		// group: htfSum 0, ltfSum 0 → dom 0 → mixed.
		{"nets to zero", 1, -1, 0, 0, 0, "mixed", 0, 0},

		// ── Tightened 2026-09-11 ────────────────────────────────────────
		// THE REGRESSION, from the live ETH strip: 15m +2, 1h -1, 4h +1,
		// 5m/2h neutral. The shipped rule read agree 2 with htfAgree 1 and
		// printed "✓ TF 一致偏多 (2/5·反1)". Two things now stop it: the
		// minority is CONTESTED (against 1, so the unopposed clause fails
		// and agree>=3 fails), and the dissenter is 1h.
		{"contested 2 of 5, 1h dissents", 0, 2, -1, 0, 1, "weak-long", 2, 1},
		// The veto on its own. A real majority (agree 3) that 1h objects to
		// is still not actionable: 1h is the only timeframe any shipped edge
		// is validated on (isStructureTF, the daemon, the sweep-reject A/B).
		{"majority but 1h dissents", 1, 1, -1, 1, 0, "weak-long", 3, 1},
		// And a NEUTRAL 1h must not veto — only actual opposition does.
		{"1h neutral does not veto", 1, 1, 0, 1, 0, "aligned-long", 3, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeAlignment(tfScores(tc.m5, tc.m15, tc.h1, tc.h2, tc.h4))
			if s, _ := got["state"].(string); s != tc.wantState {
				t.Fatalf("state = %q, want %q (label %v)", s, tc.wantState, got["label"])
			}
			// agree/against are only reported on the leaning states.
			if tc.wantState == "conflict" || tc.wantState == "mixed" {
				return
			}
			if a, _ := got["agree"].(int); a != tc.wantAgree {
				t.Errorf("agree = %d, want %d", a, tc.wantAgree)
			}
			if a, _ := got["against"].(int); a != tc.wantAgainst {
				t.Errorf("against = %d, want %d", a, tc.wantAgainst)
			}
			label, _ := got["label"].(string)
			if strings.HasPrefix(tc.wantState, "weak-") && !strings.Contains(label, "弱共識") {
				t.Errorf("weak state should not read as 一致: %q", label)
			}
			if strings.HasPrefix(tc.wantState, "aligned-") && !strings.Contains(label, "一致") {
				t.Errorf("aligned label lost its 一致 marker: %q", label)
			}
		})
	}
}

// The bug had teeth because computeRange keys isRange off the alignment state:
// a phantom trend disabled the range read-aid in a flat market.
func TestComputeRangeEngagesOnWeakAlignment(t *testing.T) {
	// 40 bars spanning 100..200 so lo=100, hi=200; price 150 → pos 50 (middle).
	candles := make([]market.Candle, 40)
	for i := range candles {
		candles[i].Low, candles[i].High = 140, 160
	}
	candles[5].Low, candles[6].High = 100, 200

	for _, tc := range []struct {
		state       string
		wantIsRange bool
	}{
		{"weak-short", true},
		{"weak-long", true},
		{"mixed", true},
		{"conflict", true},
		{"aligned-short", false},
		{"aligned-long", false},
	} {
		rg := computeRange(candles, 150, tc.state)
		if rg == nil {
			t.Fatalf("%s: nil range", tc.state)
		}
		if got, _ := rg["isRange"].(bool); got != tc.wantIsRange {
			t.Errorf("%s: isRange = %v, want %v", tc.state, got, tc.wantIsRange)
		}
		if z, _ := rg["zone"].(string); z != "middle" {
			t.Errorf("%s: zone = %q, want middle", tc.state, z)
		}
		act, _ := rg["action"].(string)
		if tc.wantIsRange && !strings.Contains(act, "中間別做") {
			t.Errorf("%s: action = %q, want the don't-trade-the-middle advice", tc.state, act)
		}
		if !tc.wantIsRange && !strings.Contains(act, "趨勢中") {
			t.Errorf("%s: action = %q, want the trend advice", tc.state, act)
		}
	}
}

// A downgrade has to say WHY, or the operator sees 弱共識 and cannot tell
// whether breadth was thin or the 1h lens objected — two different reads with
// two different responses.
func TestAlignmentLabelNamesTheOneHourVeto(t *testing.T) {
	// agree 3 / against 1, downgraded purely by the 1h veto.
	got := computeAlignment(tfScores(1, 1, -1, 1, 0))
	label, _ := got["label"].(string)
	if !strings.Contains(label, "1h 反對") {
		t.Errorf("label = %q, want it to name the 1h dissent", label)
	}
	// A thin-breadth downgrade must NOT claim a 1h veto that did not happen.
	got = computeAlignment(tfScores(-1, -1, 0, 0, 0))
	label, _ = got["label"].(string)
	if strings.Contains(label, "1h 反對") {
		t.Errorf("label = %q claims a 1h veto, but 1h was neutral", label)
	}
}
