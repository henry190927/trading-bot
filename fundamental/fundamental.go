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
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/henry190927/trading-bot/earnings/finnhub"
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
	Rich          bool     `json:"rich"`           // strong quality but VERY expensive → trim/sell-into-strength candidate
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
		if val < 25 {
			// Very expensive despite strong quality — flag for trimming /
			// selling into strength (the closest thing to a spot sell signal
			// from valuation alone).
			r.Rich = true
			r.Notes = append(r.Notes, fmt.Sprintf("strong quality (%.0f) but VERY rich (val %.0f) — trim / sell into strength", quality, val))
		} else {
			r.Notes = append(r.Notes, fmt.Sprintf("strong quality (%.0f) but rich valuation (val %.0f) — priced in", quality, val))
		}
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

// --- Board: precomputed universe scan (consumer B auto-scan) ---------------

// BoardEntry is one rated symbol persisted to fundamentals.json by the daily
// scan, carrying the display metrics alongside the Rating so the web page
// needs no live API call.
type BoardEntry struct {
	Rating
	PrevLabel    string  `json:"prev_label,omitempty"` // label from the previous scan → downgrade detection
	PE           float64 `json:"pe"`
	PS           float64 `json:"ps"`
	RevGrowthYoY float64 `json:"rev_growth_yoy"`
	NetMargin    float64 `json:"net_margin"`
	DebtToEquity float64 `json:"debt_to_equity"`
}

// Board is the whole scored universe written by cmd/fundamental-scan and read
// by the /fundamentals page.
type Board struct {
	UpdatedUTC string       `json:"updated_utc"`
	Entries    []BoardEntry `json:"entries"`
}

// LabelRank orders labels best→worst for sorting and downgrade detection:
// buy(0) < hold(1) < avoid(2) < unknown(3). A move to a HIGHER rank than the
// previous scan is a downgrade (the fundamental sell signal).
func LabelRank(label string) int {
	switch label {
	case "buy":
		return 0
	case "hold":
		return 1
	case "avoid":
		return 2
	default: // unknown / ""
		return 3
	}
}

// Counts tallies entries by label for the page header.
func (b Board) Counts() map[string]int {
	m := map[string]int{}
	for _, e := range b.Entries {
		m[e.Label]++
	}
	return m
}

// --- F1/F2 → technical sizing overlay (consumer A cooperation) -------------

// SizingHint is how the SLOW fundamental layer modulates a FAST technical
// setup on a stock. It never generates a trade and never enters the engine
// (which stays closed-bar / backtest-pure) — it's a discretionary sizing +
// direction-permission overlay applied at the decision surface:
//   F1 quality  = direction permission (don't full-size a long into a
//                 deteriorating company; green-light a short on one)
//   F2 valuation = conviction/size (expensive fades a long, tailwinds a short)
type SizingHint struct {
	Factor    float64 `json:"factor"`    // 1.0 full · 0.5 half · 0.25 small · 0 skip · <0 = n/a
	Label     string  `json:"label"`     // "full" | "half" | "small" | "skip" | "neutral"
	Aligned   string  `json:"aligned"`   // "aligned" | "conflict" | "neutral"
	Rationale string  `json:"rationale"`
}

// effectiveLabel folds the Rich flag into a 5-way label.
func (r Rating) effectiveLabel() string {
	if r.Label == "hold" && r.Rich {
		return "rich"
	}
	return r.Label
}

// SuggestSizing combines a technical side ("long"/"short"/"flat"/"") with the
// fundamental rating. Unknown fundamentals or a flat technical read → neutral
// (size on the technical alone; the overlay abstains).
func SuggestSizing(techSide string, r Rating) SizingHint {
	side := strings.ToLower(strings.TrimSpace(techSide))
	if side != "long" && side != "short" {
		return SizingHint{Factor: -1, Label: "neutral", Aligned: "neutral", Rationale: "no directional technical setup"}
	}
	eff := r.effectiveLabel()
	if eff == "unknown" || eff == "" {
		return SizingHint{Factor: -1, Label: "neutral", Aligned: "neutral", Rationale: "no fundamentals — size on the technical read alone"}
	}
	switch side {
	case "long":
		switch eff {
		case "buy":
			return SizingHint{1.0, "full", "aligned", "fundamentals (buy) support the long — full size ok"}
		case "hold":
			return SizingHint{0.5, "half", "neutral", "solid but priced in (hold) — don't full-size a long"}
		case "rich":
			return SizingHint{0.25, "small", "conflict", "very expensive (rich) — a long is chasing; small size or wait for a pullback"}
		case "avoid":
			return SizingHint{0.0, "skip", "conflict", "weak fundamentals (avoid) — a long fights the slow layer; skip or scalp only"}
		}
	case "short":
		switch eff {
		case "avoid":
			return SizingHint{1.0, "full", "aligned", "weak fundamentals (avoid) support the short — full size ok"}
		case "rich":
			return SizingHint{1.0, "full", "aligned", "expensive (rich) — shorting into strength has a fundamental tailwind"}
		case "hold":
			return SizingHint{0.5, "half", "neutral", "fairly-valued quality (hold) — half size on the short"}
		case "buy":
			return SizingHint{0.25, "small", "conflict", "strong, cheap company (buy) — a short fights fundamentals; small size or skip"}
		}
	}
	return SizingHint{Factor: -1, Label: "neutral", Aligned: "neutral", Rationale: ""}
}

// LoadBoard reads a fundamentals.json board from disk (written by
// cmd/fundamental-scan). Returns an error if missing/unparseable.
func LoadBoard(path string) (*Board, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Board
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// Find returns the board entry for a bare ticker (case-insensitive), if present.
func (b *Board) Find(ticker string) (*BoardEntry, bool) {
	tk := strings.ToUpper(strings.TrimSpace(ticker))
	for i := range b.Entries {
		if b.Entries[i].Symbol == tk {
			return &b.Entries[i], true
		}
	}
	return nil, false
}
