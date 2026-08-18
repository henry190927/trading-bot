// Package fundamental scores a company's F1 (quality) and F2 (valuation)
// fundamentals into a spot buy/hold/avoid rating — consumer B of the earnings
// data layer (docs/fundamental_f3_earnings_spec.md). It is a STANDALONE spot /
// buy-hold indicator: no engine, no bias, no backtest, and it scales to many
// symbols. It intentionally does NOT feed the technical trading system.
//
// Philosophy (the quality-value matrix):
//   - weak fundamentals            → AVOID (a cheap bad company is a value trap)
//   - strong quality + fair price  → BUY
//   - strong quality + expensive   → HOLD (great company, already priced in)
//   - otherwise                    → HOLD
//
// Every sub-factor ABSTAINS when its metric is missing (0), so a sparse Finnhub
// response lowers confidence rather than scoring absent data as bad.
package fundamental

import (
	"fmt"

	"myFirstGo/trading-bot/earnings/finnhub"
)

// Rating is the spot indicator's verdict for one symbol.
type Rating struct {
	Symbol        string   `json:"symbol"`
	Label         string   `json:"label"` // "buy" | "hold" | "avoid" | "unknown"
	Quality       float64  `json:"quality"`        // F1 composite 0-100
	Valuation     float64  `json:"valuation"`      // F2 composite 0-100 (higher = cheaper)
	Growth        float64  `json:"growth"`         // F1 sub
	Profitability float64  `json:"profitability"`  // F1 sub
	BalanceSheet  float64  `json:"balance_sheet"`  // F1 sub
	Confidence    string   `json:"confidence"`     // "high" | "med" | "low"
	Notes         []string `json:"notes"`
}

// thresholds: (cutoff, score) descending — first cutoff the value meets wins.
type band struct {
	min   float64
	score float64
}

// scoreHigh: higher metric value is better (growth, margins).
func scoreHigh(v float64, bands []band) (float64, bool) {
	if v == 0 {
		return 0, false // missing → abstain
	}
	for _, b := range bands {
		if v >= b.min {
			return b.score, true
		}
	}
	return 10, true // below all cutoffs
}

// scoreLow: lower metric value is better (P/E, P/S, debt/equity). A non-positive
// value (e.g. negative-earnings P/E) abstains rather than scoring as "cheap".
func scoreLow(v float64, bands []band) (float64, bool) {
	if v <= 0 {
		return 0, false
	}
	for _, b := range bands {
		if v <= b.min {
			return b.score, true
		}
	}
	return 15, true
}

func avg(vals ...float64) (float64, int) {
	sum, n := 0.0, 0
	for _, v := range vals {
		if v >= 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// collect gathers present sub-scores (score, ok) into a slice for averaging.
func collect(pairs ...struct {
	s  float64
	ok bool
}) []float64 {
	var out []float64
	for _, p := range pairs {
		if p.ok {
			out = append(out, p.s)
		}
	}
	return out
}

func mean(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs)), true
}

type sub struct {
	s  float64
	ok bool
}

// Score turns fundamentals into a Rating.
func Score(m finnhub.Metrics) Rating {
	r := Rating{Symbol: m.Symbol}

	// --- F1 growth ---
	gRev := sub{}
	gRev.s, gRev.ok = scoreHigh(m.RevGrowthYoY, []band{{25, 100}, {10, 75}, {0.0001, 50}})
	gEPS := sub{}
	gEPS.s, gEPS.ok = scoreHigh(m.EPSGrowthYoY, []band{{25, 100}, {10, 75}, {0.0001, 50}})
	growth, gOK := mean(collect(gRev, gEPS))

	// --- F1 profitability ---
	pNet := sub{}
	pNet.s, pNet.ok = scoreHigh(m.NetMargin, []band{{20, 100}, {10, 75}, {0.0001, 50}})
	pROE := sub{}
	pROE.s, pROE.ok = scoreHigh(m.ROE, []band{{20, 100}, {10, 75}, {0.0001, 50}})
	prof, pOK := mean(collect(pNet, pROE))

	// --- F1 balance sheet ---
	bCur := sub{}
	bCur.s, bCur.ok = scoreHigh(m.CurrentRatio, []band{{1.5, 100}, {1.0, 60}, {0.0001, 30}})
	bDebt := sub{}
	bDebt.s, bDebt.ok = scoreLow(m.DebtToEquity, []band{{0.5, 100}, {1.0, 70}, {2.0, 40}})
	bal, bOK := mean(collect(bCur, bDebt))

	// --- F2 valuation (higher score = cheaper) ---
	vPE := sub{}
	vPE.s, vPE.ok = scoreLow(m.PE, []band{{15, 100}, {25, 70}, {40, 40}})
	vPS := sub{}
	vPS.s, vPS.ok = scoreLow(m.PS, []band{{3, 100}, {6, 70}, {10, 40}})
	val, vOK := mean(collect(vPE, vPS))

	r.Growth, r.Profitability, r.BalanceSheet = round1(growth), round1(prof), round1(bal)
	quality, qN := avg(dashIf(growth, gOK), dashIf(prof, pOK), dashIf(bal, bOK))
	r.Quality = round1(quality)
	r.Valuation = round1(val)

	// --- confidence from data completeness (how many of the 4 dimensions present) ---
	present := boolToInt(gOK) + boolToInt(pOK) + boolToInt(bOK) + boolToInt(vOK)
	switch {
	case present >= 4:
		r.Confidence = "high"
	case present >= 2:
		r.Confidence = "med"
	default:
		r.Confidence = "low"
	}

	// --- rating: quality-value matrix ---
	switch {
	case qN == 0 || !vOK:
		r.Label = "unknown"
		r.Notes = append(r.Notes, "insufficient fundamentals to rate")
	case quality < 40:
		r.Label = "avoid"
		r.Notes = append(r.Notes, fmt.Sprintf("weak quality (%.0f/100) — value-trap risk", quality))
	case quality >= 60 && val >= 40:
		r.Label = "buy"
		r.Notes = append(r.Notes, fmt.Sprintf("strong quality (%.0f) at a fair price (val %.0f)", quality, val))
	case quality >= 60 && val < 40:
		r.Label = "hold"
		r.Notes = append(r.Notes, fmt.Sprintf("strong quality (%.0f) but rich valuation (val %.0f) — priced in", quality, val))
	default:
		r.Label = "hold"
		r.Notes = append(r.Notes, fmt.Sprintf("mixed: quality %.0f / valuation %.0f", quality, val))
	}
	return r
}

// dashIf returns the value if ok, else -1 (avg skips negatives).
func dashIf(v float64, ok bool) float64 {
	if !ok {
		return -1
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
