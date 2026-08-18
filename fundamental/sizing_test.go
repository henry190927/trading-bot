package fundamental

import "testing"

func r(label string, rich bool) Rating { return Rating{Label: label, Rich: rich} }

func TestSuggestSizing(t *testing.T) {
	cases := []struct {
		name    string
		side    string
		rating  Rating
		factor  float64
		label   string
		aligned string
	}{
		{"long+buy full", "long", r("buy", false), 1.0, "full", "aligned"},
		{"long+hold half", "long", r("hold", false), 0.5, "half", "neutral"},
		{"long+rich small conflict", "long", r("hold", true), 0.25, "small", "conflict"},
		{"long+avoid skip", "long", r("avoid", false), 0.0, "skip", "conflict"},
		{"short+avoid full", "short", r("avoid", false), 1.0, "full", "aligned"},
		{"short+rich full tailwind", "short", r("hold", true), 1.0, "full", "aligned"},
		{"short+hold half", "short", r("hold", false), 0.5, "half", "neutral"},
		{"short+buy small conflict", "short", r("buy", false), 0.25, "small", "conflict"},
		{"flat neutral", "flat", r("buy", false), -1, "neutral", "neutral"},
		{"empty side neutral", "", r("buy", false), -1, "neutral", "neutral"},
		{"long+unknown neutral", "long", r("unknown", false), -1, "neutral", "neutral"},
	}
	for _, c := range cases {
		h := SuggestSizing(c.side, c.rating)
		if h.Factor != c.factor || h.Label != c.label || h.Aligned != c.aligned {
			t.Errorf("%s: got {%.2f %s %s}, want {%.2f %s %s}", c.name,
				h.Factor, h.Label, h.Aligned, c.factor, c.label, c.aligned)
		}
	}
}
