// Package shipgate decides whether an A/B result is allowed to ship.
//
// Until now the gate lived in prose — "prove +R across 60/90/120d" in a
// harness comment, applied by hand at the end of each run. Two results on
// 2026-09-03/04 showed what that costs:
//
//   - SUI 1h passed StructMomentum's gate by beating the MR baseline in all
//     three windows, while being NEGATIVE in all three. It won only because MR
//     was catastrophic there. "Better than a disaster" is not an edge, and a
//     purely relative gate cannot say so.
//   - pivot-zone-fade's CONFIRM arm beat TOUCH in every window and every
//     nearby parameter, at -0.063 R/trade. Also not shippable.
//
// Both times the number had to be annotated by hand. That is the job of the
// gate, not of whoever remembers to write the caveat.
//
// The other correction is that PASS/FAIL is the wrong shape. StructMomentum on
// 2h fired 3-14 times per window; calling n=3 a window loss is reading noise.
// Too-small samples are UNDECIDED — a state that must not be lumped in with
// FAIL, because the two lead to opposite actions (gather more data vs stop).
package shipgate

import (
	"fmt"
	"sort"
	"strings"
)

// Window is one backtest window's result for one arm.
type Window struct {
	Days    int
	NetR    float64
	Trades  int
	WinRate float64 // percent, 0..100; informational
}

// RPerTrade is netR normalised by trade count — the comparable figure when
// arms take very different numbers of trades (SM fires ~1/3 as often as MR).
func (w Window) RPerTrade() float64 {
	if w.Trades == 0 {
		return 0
	}
	return w.NetR / float64(w.Trades)
}

// Arm is one candidate: a strategy or variant on one (symbol, timeframe).
type Arm struct {
	Name    string
	Windows []Window
}

// Result is the tri-state verdict.
type Result string

const (
	Pass      Result = "PASS"
	Fail      Result = "FAIL"
	Undecided Result = "UNDECIDED"
)

// Criteria is the gate. Default() is the practiced gate plus the absolute
// floor this package exists to add.
type Criteria struct {
	// MinWindows is how many windows must be present. Fewer is UNDECIDED, not
	// FAIL: a symbol listed 62 days ago (ONDS) can only produce one window,
	// which is a data problem, not a verdict.
	MinWindows int

	// MinTradesPerWindow is the sample-size floor. Below it a window cannot
	// contribute evidence either way.
	MinTradesPerWindow int

	// ThinTradesWarn adds a NOTE (not a failure) when the thinnest window is
	// below it. A PASS on 14 trades and a PASS on 75 are not the same claim,
	// and the difference should be visible in the verdict rather than left to
	// whoever reads the table.
	ThinTradesWarn int

	// MinNetRPerWindow requires netR above this in EVERY window. Aggregate is
	// deliberately not used — it hides a losing window.
	MinNetRPerWindow float64

	// MinRPerTradeMedian is the ABSOLUTE viability floor: the median
	// per-window R/trade must clear it regardless of how the baseline did.
	// This is the criterion whose absence let SUI 1h and CONFIRM through.
	MinRPerTradeMedian float64

	// RequireBeatBaselineEveryWindow additionally demands the arm beat the
	// supplied baseline in every window. Only applies when a baseline is
	// given; a brand-new strategy has none.
	RequireBeatBaselineEveryWindow bool
}

// Default is the gate as it should have been all along.
//
// MinRPerTradeMedian is 0 rather than something comfortably positive on
// purpose: the point is to exclude arms that LOSE money, not to invent a
// profitability bar nobody agreed to. Raise it deliberately, per decision.
//
// MinTradesPerWindow = 8 is the WEAKEST NUMBER HERE and should be treated as
// provisional. It was calibrated against the eight (symbol, TF) verdicts
// reached by hand on 2026-09-03/04, where the thinnest window of a case I was
// willing to decide ran 9-14 trades and the thinnest of a case I called
// undecided ran 3-6. Eight separates those clusters, and that is the entire
// justification — fitting a threshold to eight hand judgements is itself a
// mild overfit, and n=9 remains genuinely thin evidence.
//
// The mitigation is ThinTradesWarn rather than a higher floor: raising the
// floor to 15 would have marked SOL 1h UNDECIDED, and SOL 1h is the strongest
// StructMomentum result in the codebase (MR loses -19 to -21R there across
// 36-75 baseline trades). Refusing to decide that would be worse than
// deciding it with a visible caveat.
func Default() Criteria {
	return Criteria{
		MinWindows:                     3,
		MinTradesPerWindow:             8,
		ThinTradesWarn:                 20,
		MinNetRPerWindow:               0,
		MinRPerTradeMedian:             0,
		RequireBeatBaselineEveryWindow: true,
	}
}

// Verdict explains the decision. Reasons lists EVERY failed criterion, not
// just the first — a result that fails on both absolute and relative grounds
// should say so, because fixing one would not save it.
type Verdict struct {
	Result    Result
	Arm       string
	Reasons   []string
	Notes     []string
	MedRPT    float64
	Windows   int
	MinTrades int
}

func (v Verdict) String() string {
	s := fmt.Sprintf("%-9s %-28s medR/t %+0.3f  min-n %d", v.Result, v.Arm, v.MedRPT, v.MinTrades)
	if len(v.Reasons) > 0 {
		s += "  — " + strings.Join(v.Reasons, "; ")
	}
	for _, n := range v.Notes {
		s += "\n           note: " + n
	}
	return s
}

// Evaluate applies c to arm, optionally against a baseline.
//
// Order matters: UNDECIDED is checked FIRST. An arm with three-trade windows
// has not failed, it has not been measured, and reporting FAIL there would
// retire a strategy on noise.
func Evaluate(arm Arm, baseline *Arm, c Criteria) Verdict {
	v := Verdict{Arm: arm.Name, Windows: len(arm.Windows)}

	if len(arm.Windows) < c.MinWindows {
		v.Result = Undecided
		v.Reasons = append(v.Reasons, fmt.Sprintf("only %d window(s), need %d", len(arm.Windows), c.MinWindows))
		return v
	}

	minTrades := arm.Windows[0].Trades
	var rpts []float64
	for _, w := range arm.Windows {
		if w.Trades < minTrades {
			minTrades = w.Trades
		}
		rpts = append(rpts, w.RPerTrade())
	}
	v.MinTrades = minTrades
	v.MedRPT = median(rpts)

	if minTrades < c.MinTradesPerWindow {
		v.Result = Undecided
		v.Reasons = append(v.Reasons,
			fmt.Sprintf("thinnest window has %d trades, need %d — too few to decide either way", minTrades, c.MinTradesPerWindow))
		return v
	}
	if c.ThinTradesWarn > 0 && minTrades < c.ThinTradesWarn {
		v.Notes = append(v.Notes,
			fmt.Sprintf("thinnest window is %d trades (below %d) — this verdict rests on thin evidence",
				minTrades, c.ThinTradesWarn))
	}

	// ── absolute criteria ────────────────────────────────────────────────
	for _, w := range arm.Windows {
		if w.NetR <= c.MinNetRPerWindow {
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("%dd netR %+.2f not above %+.2f", w.Days, w.NetR, c.MinNetRPerWindow))
		}
	}
	if v.MedRPT <= c.MinRPerTradeMedian {
		v.Reasons = append(v.Reasons,
			fmt.Sprintf("median R/trade %+.3f not above %+.3f — LOSES money in absolute terms", v.MedRPT, c.MinRPerTradeMedian))
	}

	// ── relative criterion ───────────────────────────────────────────────
	if baseline != nil && c.RequireBeatBaselineEveryWindow {
		byDays := map[int]Window{}
		for _, w := range baseline.Windows {
			byDays[w.Days] = w
		}
		beat := 0
		compared := 0
		for _, w := range arm.Windows {
			b, ok := byDays[w.Days]
			if !ok {
				continue
			}
			compared++
			if w.NetR > b.NetR {
				beat++
			}
		}
		if compared == 0 {
			v.Notes = append(v.Notes, "baseline supplied but no matching windows — relative criterion not applied")
		} else if beat < compared {
			v.Reasons = append(v.Reasons,
				fmt.Sprintf("beat the baseline in only %d of %d windows", beat, compared))
		} else if len(v.Reasons) > 0 {
			// The exact trap: relative criterion satisfied, absolute not.
			v.Notes = append(v.Notes,
				"beat the baseline in EVERY window but still fails on absolute grounds — 'better than a disaster' is not an edge")
		}
	}

	if len(v.Reasons) == 0 {
		v.Result = Pass
	} else {
		v.Result = Fail
	}
	return v
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	m := len(c) / 2
	if len(c)%2 == 1 {
		return c[m]
	}
	return (c[m-1] + c[m]) / 2
}
