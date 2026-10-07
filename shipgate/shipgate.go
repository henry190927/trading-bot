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
	"math"
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
//
// Windows are the practiced 60/90/120d runs and they are NESTED — the 60d sits
// inside the 90d sits inside the 120d. Buckets are DISJOINT periods covering
// the same history, and they exist because the nested windows cannot tell a
// durable edge from one good month. A month that happens to be strong appears
// in all three windows at once and reads as three confirmations.
type Arm struct {
	Name    string
	Windows []Window

	// Repeated Days values in EITHER slice are jitter readings of the same
	// window, taken by shifting the window's end by a day or two. They
	// collapse to a median, and a criterion that holds for the median but
	// not for every reading is reported as fragile rather than as a pass.
	//
	// Buckets are disjoint periods in any order, each Window's Days field
	// carrying the END OFFSET in days rather than a length (so a 30-day
	// bucket ending 90 days ago is Days: 90). Optional; supply them and the
	// breadth test runs.
	Buckets []Window
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

	// RequireBucketBreadth downgrades a PASS to UNDECIDED when the disjoint
	// buckets show the whole result came from one of them — specifically when
	// removing the single best bucket takes the total to zero or below.
	//
	// UNDECIDED and not FAIL on purpose. One good period plus noise is not
	// evidence that an edge exists, but it is not evidence that it does not
	// either, and the existing semantics for "the data cannot settle this"
	// is UNDECIDED (see MinTradesPerWindow). A FAIL would read as "this rule
	// loses money", which is a different and unsupported claim.
	RequireBucketBreadth bool

	// MinBuckets is how many disjoint buckets the breadth test needs. Below
	// it the test is skipped with a note rather than guessed at.
	MinBuckets int

	// RequireSignRobust downgrades a PASS to UNDECIDED when a window clears
	// MinNetRPerWindow at its median but not at every jitter reading.
	//
	// This is the sharpest available test because it is the gate's OWN
	// criterion, not an invented tolerance: if shifting the window by a day
	// moves netR across zero, then "netR is positive in every window" is a
	// statement about when the backtest was run.
	RequireSignRobust bool

	// MinBucketTrades is the sample floor for the MEDIAN bucket. A disjoint
	// bucket is a quarter the span of the longest window, so a low-frequency
	// rule can land three trades in one and the drop-best test then measures
	// noise rather than concentration — and would flag nearly everything.
	// Median rather than minimum: one thin bucket among healthy ones does not
	// invalidate the comparison.
	MinBucketTrades int
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
		RequireSignRobust:              true,
		RequireBucketBreadth:           true,
		MinBuckets:                     4,
		MinBucketTrades:                8,
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

	// Jittered is true when any window carried more than one reading.
	Jittered bool

	// Fragile lists windows whose verdict depends on where the window ends.
	Fragile []string

	spread  map[int]span
	bSpread map[int]span

	// Breadth, set only when buckets were supplied and numerous enough.
	Buckets      int
	BucketTotal  float64 // summed netR across the disjoint buckets
	BestBucketR  float64 // the single strongest bucket
	DropBestR    float64 // BucketTotal − BestBucketR
	Concentrated bool    // DropBestR <= 0: one bucket carries the result
}

func (v Verdict) String() string {
	s := fmt.Sprintf("%-9s %-28s medR/t %+0.3f  min-n %d", v.Result, v.Arm, v.MedRPT, v.MinTrades)
	if len(v.Reasons) > 0 {
		s += "  — " + strings.Join(v.Reasons, "; ")
	}
	if v.Jittered {
		var parts []string
		for _, d := range sortedKeys(v.spread) {
			sp := v.spread[d]
			if sp.N > 1 {
				parts = append(parts, fmt.Sprintf("%dd %+.2f..%+.2f", d, sp.Lo, sp.Hi))
			}
		}
		if len(parts) > 0 {
			s += "\n           jitter: " + strings.Join(parts, " · ")
		}
	}
	if v.Buckets > 0 {
		flag := "spread"
		if v.Concentrated {
			flag = "CONCENTRATED"
		}
		s += fmt.Sprintf("\n           breadth: %d buckets, total %+.2f, best %+.2f, without it %+.2f — %s",
			v.Buckets, v.BucketTotal, v.BestBucketR, v.DropBestR, flag)
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
	// Jitter readings collapse before anything is judged, so every criterion
	// below sees one figure per window and the spread is kept to one side for
	// the robustness test.
	windows, spread := collapse(arm.Windows)
	buckets, bSpread := collapse(arm.Buckets)
	arm.Windows, arm.Buckets = windows, buckets

	v := Verdict{Arm: arm.Name, Windows: len(arm.Windows)}
	for _, sp := range spread {
		if sp.N > 1 {
			v.Jittered = true
		}
	}
	v.spread, v.bSpread = spread, bSpread

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

	// ── robustness to where the window ends ─────────────────────────────
	if c.RequireSignRobust {
		for _, w := range arm.Windows {
			sp, ok := spread[w.Days]
			if !ok || sp.N < 2 {
				continue
			}
			// Only interesting when the median clears the bar and a reading
			// does not: that is a PASS that depends on the run date.
			if w.NetR > c.MinNetRPerWindow && sp.Lo <= c.MinNetRPerWindow {
				v.Fragile = append(v.Fragile,
					fmt.Sprintf("%dd spans %+.2f..%+.2f across %d readings", w.Days, sp.Lo, sp.Hi, sp.N))
			}
		}
	}

	// ── breadth over disjoint buckets ────────────────────────────────────
	// Runs regardless of the verdict so the table always shows WHERE the R
	// came from, but it can only change a PASS. A FAIL is already decided,
	// and concentration is not a second reason to reject it.
	if c.RequireBucketBreadth {
		switch {
		case len(arm.Buckets) == 0:
			v.Notes = append(v.Notes, "no disjoint buckets supplied — breadth untested, so a PASS here rests on nested windows that share most of their data")
		case len(arm.Buckets) < c.MinBuckets:
			v.Notes = append(v.Notes,
				fmt.Sprintf("only %d disjoint bucket(s), need %d — breadth untested", len(arm.Buckets), c.MinBuckets))
		default:
			var bt []float64
			for _, b := range arm.Buckets {
				bt = append(bt, float64(b.Trades))
			}
			if med := median(bt); med < float64(c.MinBucketTrades) {
				v.Notes = append(v.Notes,
					fmt.Sprintf("median bucket holds %.0f trades (below %d) — breadth untested, the buckets are too thin to tell concentration from noise",
						med, c.MinBucketTrades))
				break
			}
			v.Buckets = len(arm.Buckets)
			best := math.Inf(-1)
			for _, b := range arm.Buckets {
				v.BucketTotal += b.NetR
				if b.NetR > best {
					best = b.NetR
				}
			}
			v.BestBucketR = best
			v.DropBestR = v.BucketTotal - best
			v.Concentrated = v.DropBestR <= 0

			// The concentration call moved on its own once, so it only
			// counts when it survives the bucket jitter. Lo and Hi are
			// deliberately pessimistic — the readings are correlated and
			// never all land at an extreme together — which means the call
			// is kept only where it is robust to more than really happens.
			if agree, tested := concentrationHolds(arm.Buckets, bSpread, v.Concentrated); tested && !agree {
				v.Notes = append(v.Notes,
					"the concentration call flips within the bucket jitter — breadth left untested rather than decided on a reading")
				v.Concentrated = false
			}
		}
	}

	switch {
	case len(v.Reasons) > 0:
		v.Result = Fail
	case len(v.Fragile) > 0:
		v.Result = Undecided
		v.Reasons = append(v.Reasons,
			fmt.Sprintf("passes at the median but not at every window end — %s", strings.Join(v.Fragile, "; ")))
	case v.Concentrated:
		v.Result = Undecided
		v.Reasons = append(v.Reasons,
			fmt.Sprintf("passes the windows, but %d disjoint buckets total %+.2fR and drop to %+.2fR without the best one — the edge is one period, not a trend",
				v.Buckets, v.BucketTotal, v.DropBestR))
	default:
		v.Result = Pass
	}
	return v
}

// concentrationHolds re-runs the drop-the-best test with every bucket at the
// bottom of its jitter range and again at the top. tested is false when no
// bucket carried more than one reading.
func concentrationHolds(bs []Window, sp map[int]span, want bool) (agree, tested bool) {
	at := func(pick func(span) float64) bool {
		var total, best float64
		best = math.Inf(-1)
		for _, b := range bs {
			r := b.NetR
			if s, ok := sp[b.Days]; ok && s.N > 1 {
				r = pick(s)
			}
			total += r
			if r > best {
				best = r
			}
		}
		return total-best <= 0
	}
	for _, b := range bs {
		if s, ok := sp[b.Days]; ok && s.N > 1 {
			tested = true
			break
		}
	}
	if !tested {
		return true, false
	}
	lo := at(func(s span) float64 { return s.Lo })
	hi := at(func(s span) float64 { return s.Hi })
	return lo == want && hi == want, true
}

// span is the min and max of a window's jitter readings.
type span struct {
	Lo, Hi float64
	N      int
}

// collapse groups windows sharing a Days label into one, using the MEDIAN of
// the readings rather than the mean: a single re-sequenced run should not drag
// the figure the gate judges on.
func sortedKeys(m map[int]span) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func collapse(ws []Window) ([]Window, map[int]span) {
	byDays := map[int][]Window{}
	var order []int
	for _, w := range ws {
		if _, seen := byDays[w.Days]; !seen {
			order = append(order, w.Days)
		}
		byDays[w.Days] = append(byDays[w.Days], w)
	}
	sort.Ints(order)

	out := make([]Window, 0, len(order))
	sp := map[int]span{}
	for _, d := range order {
		g := byDays[d]
		var rs, ts, wrs []float64
		lo, hi := g[0].NetR, g[0].NetR
		for _, w := range g {
			rs = append(rs, w.NetR)
			ts = append(ts, float64(w.Trades))
			wrs = append(wrs, w.WinRate)
			if w.NetR < lo {
				lo = w.NetR
			}
			if w.NetR > hi {
				hi = w.NetR
			}
		}
		out = append(out, Window{Days: d, NetR: median(rs), Trades: int(median(ts)), WinRate: median(wrs)})
		sp[d] = span{Lo: lo, Hi: hi, N: len(g)}
	}
	return out, sp
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
