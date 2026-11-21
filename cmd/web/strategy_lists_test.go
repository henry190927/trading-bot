package main

import (
	"testing"

	"github.com/henry190927/trading-bot/autotrade"
)

// The web keeps two strategy lists of its own: knownStrategies (which codes
// get a translated label) and scanStrategies (which ones /scan synthesizes).
// autostrat's dispatch test keeps autotrade.KnownStrategies honest; this one
// keeps the web's lists tied to it. vp-node had a dictionary entry but was
// missing from knownStrategies, so it rendered as "Other".
func TestWebStrategyListsCoverKnownStrategies(t *testing.T) {
	label := map[string]bool{}
	for _, s := range knownStrategies {
		label[s] = true
	}
	scan := map[string]bool{}
	for _, s := range scanStrategies {
		scan[s] = true
	}
	known := map[string]bool{}
	for _, s := range autotrade.KnownStrategies {
		known[s] = true
	}

	for _, s := range autotrade.KnownStrategies {
		if !label[s] {
			t.Errorf("%q is dispatchable but has no label in knownStrategies — it renders as \"Other\"", s)
		}
		_, excluded := scanExcluded[s]
		switch {
		case scan[s] && excluded:
			t.Errorf("%q is both in scanStrategies and scanExcluded", s)
		case !scan[s] && !excluded:
			t.Errorf("%q is dispatchable but neither in scanStrategies nor scanExcluded — decide whether /scan synthesizes it", s)
		}
	}
	for _, s := range scanStrategies {
		if !known[s] {
			t.Errorf("scanStrategies lists %q, which EvalAutoTrigger cannot dispatch", s)
		}
	}
	for s := range scanExcluded {
		if !known[s] {
			t.Errorf("scanExcluded lists %q, which is not a known strategy", s)
		}
	}
}
