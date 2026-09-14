package autotrade

import "sort"

// RPerTrade is realized R divided by the number of positions that actually
// settled. The denominator is tp+stop, NOT Total: open, pending and no-fill
// positions have not contributed to NetR, so counting them would dilute the
// figure toward zero and make a losing rule look milder than it is.
func (s Summary) RPerTrade() float64 {
	resolved := s.TP + s.Stop
	if resolved == 0 {
		return 0
	}
	return s.NetR / float64(resolved)
}

// Group is one slice of the position book — one strategy, one symbol, one side
// — folded with the SAME Summarize the whole-book total uses, so a group and
// the total can never be two different arithmetics.
type Group struct {
	Key string
	Summary
}

// Breakdown partitions positions by key(fire) and summarises each part.
//
// A PARTITION, not a sample: every position lands in exactly one group, so the
// group NetRs sum to the book's NetR and the group Totals sum to len(ps). That
// invariant is the point. The page's fires table is capped at a display depth,
// and a breakdown scraped off that capped table silently describes only the
// newest slice while reading like it describes the book.
//
// Groups come back sorted by NetR descending — winners first, bleeders last —
// with ties broken by key so the order is stable across refreshes.
func Breakdown(ps []Position, key func(PaperFire) string) []Group {
	idx := map[string][]Outcome{}
	for _, p := range ps {
		k := key(p.Fire)
		if k == "" {
			k = "(unset)"
		}
		idx[k] = append(idx[k], p.Outcome)
	}
	out := make([]Group, 0, len(idx))
	for k, outs := range idx {
		out = append(out, Group{Key: k, Summary: Summarize(outs)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetR != out[j].NetR {
			return out[i].NetR > out[j].NetR
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// The three cuts the panel shows. Kept here rather than in the handler so the
// same keys are available to the gate/backtest tooling that will A/B them.
func ByStrategy(f PaperFire) string { return f.Strategy }
func BySymbol(f PaperFire) string   { return f.Symbol }
func BySide(f PaperFire) string     { return f.Side }
