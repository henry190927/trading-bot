package main

import (
	"testing"

	"myFirstGo/trading-bot/autotrade"
)

// scanRR replaced an inline calculation that assigned `risk` from the LONG
// formula and then reassigned it inside the short branch. It was correct, but
// only by accident of statement order, so the arithmetic is pinned here.
func TestScanRR(t *testing.T) {
	for _, tc := range []struct {
		name            string
		side            string
		entry, stop, tp float64
		want            float64
	}{
		// The ETH htf-snr row the live page showed: 2428.83 / 2410.5428 / 2465.4044.
		{"real long row", "long", 2428.83, 2410.5428, 2465.4044, 2.0},
		{"long 1:1", "long", 100, 90, 110, 1.0},
		{"long 1:3", "long", 100, 90, 130, 3.0},
		// Short: risk is ABOVE entry and reward BELOW it — the case the
		// original inline version had to special-case.
		{"short 1:1", "short", 100, 110, 90, 1.0},
		{"short 1:2", "short", 100, 110, 80, 2.0},
		{"short real-ish", "short", 2500, 2530, 2410, 3.0},
		// A stop on the wrong side is not a 0-risk infinite-RR trade; it is
		// unusable input and must report 0 rather than +Inf or NaN.
		{"long stop above entry", "long", 100, 110, 130, 0},
		{"short stop below entry", "short", 100, 90, 80, 0},
		{"zero risk", "long", 100, 100, 130, 0},
		// A target on the losing side yields a NEGATIVE ratio, which is
		// information (the trigger is malformed), not something to hide.
		{"long tp below entry", "long", 100, 90, 95, -0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanRR(tc.side, tc.entry, tc.stop, tc.tp)
			if diff := got - tc.want; diff > 0.01 || diff < -0.01 {
				t.Errorf("scanRR(%s, %g, %g, %g) = %.4f, want %.2f",
					tc.side, tc.entry, tc.stop, tc.tp, got, tc.want)
			}
		})
	}
}

func rule(sym, strat, tf string, stopPct float64) autotrade.Rule {
	return autotrade.Rule{Enabled: true, Symbol: sym, Strategy: strat, TF: tf, Side: "auto", StopPct: stopPct, CooldownBars: 6}
}

// The gap this build closed: the page evaluated only the configured rules, so
// most of the (symbol x strategy) grid was invisible — including every XAU and
// LINK combination, because neither symbol has a rule at all.
func TestScanGridCoversEveryCombination(t *testing.T) {
	cfg := autotrade.Config{Rules: []autotrade.Rule{
		rule("BTC", "range-edge", "1h", 0.5),
		rule("ETH", "engine", "1h", 0.4),
		rule("XAG", "engine", "2h", 0.4),
	}}
	grid := scanGrid(cfg)

	want := 0
	for _, short := range uiSymbols {
		if _, ok := webScanSymbol(short); ok {
			want += len(scanStrategies)
		}
	}
	if len(grid) != want {
		t.Fatalf("grid = %d cells, want %d (every resolvable symbol x %d strategies)", len(grid), want, len(scanStrategies))
	}

	seen := map[string]int{}
	armed := 0
	for _, c := range grid {
		seen[c.rule.Symbol+"|"+c.rule.Strategy]++
		if c.armed {
			armed++
		}
	}
	if armed != 3 {
		t.Errorf("armed = %d, want 3 (only the configured rules)", armed)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times — a combination must be evaluated exactly once", k, n)
		}
	}
	// The specific symbols that used to vanish.
	for _, sym := range []string{"XAU", "LINK"} {
		for _, strat := range scanStrategies {
			if seen[sym+"|"+strat] != 1 {
				t.Errorf("%s/%s missing from the grid", sym, strat)
			}
		}
	}
}

// A configured rule must be used VERBATIM — its TF and StopPct are what the
// daemon will actually act on, so the scan has to evaluate the real thing.
func TestScanGridKeepsConfiguredRulesVerbatim(t *testing.T) {
	cfg := autotrade.Config{Rules: []autotrade.Rule{rule("XAG", "engine", "2h", 0.77)}}
	for _, c := range scanGrid(cfg) {
		if c.rule.Symbol == "XAG" && c.rule.Strategy == "engine" {
			if !c.armed {
				t.Error("the configured rule must be marked armed")
			}
			if c.rule.TF != "2h" {
				t.Errorf("TF = %q, want the configured 2h (not the 1h default)", c.rule.TF)
			}
			if c.rule.StopPct != 0.77 {
				t.Errorf("StopPct = %v, want the configured 0.77", c.rule.StopPct)
			}
			return
		}
	}
	t.Fatal("configured XAG/engine row missing")
}

// Synthesized rows borrow the strategy's real stop settings rather than
// inventing numbers — range-edge is the one strategy that reads StopPct, so a
// wrong value there would produce a wrong stop on every unarmed range-edge row.
func TestScanGridSynthesizedBorrowsStrategyTemplate(t *testing.T) {
	cfg := autotrade.Config{Rules: []autotrade.Rule{rule("BTC", "range-edge", "1h", 0.9)}}
	found := false
	for _, c := range scanGrid(cfg) {
		if c.rule.Strategy != "range-edge" || c.rule.Symbol == "BTC" {
			continue
		}
		found = true
		if c.armed {
			t.Errorf("%s/range-edge should be unarmed", c.rule.Symbol)
		}
		if c.rule.StopPct != 0.9 {
			t.Errorf("%s StopPct = %v, want the template's 0.9", c.rule.Symbol, c.rule.StopPct)
		}
		if !c.rule.Enabled {
			t.Errorf("%s must be Enabled or EvalAutoTrigger would be evaluating a disabled rule", c.rule.Symbol)
		}
	}
	if !found {
		t.Fatal("no synthesized range-edge rows")
	}
}

// A strategy with NO configured rule anywhere still has to be scannable.
func TestScanGridStrategyWithNoTemplate(t *testing.T) {
	cfg := autotrade.Config{Rules: []autotrade.Rule{rule("BTC", "engine", "1h", 0.4)}}
	for _, c := range scanGrid(cfg) {
		if c.rule.Strategy == "htf-snr" {
			if c.rule.TF == "" {
				t.Error("a template-less strategy must still get a TF, or the eval fetches nothing")
			}
			if c.rule.CooldownBars == 0 {
				t.Error("want a fallback cooldown")
			}
			return
		}
	}
	t.Fatal("htf-snr never appeared")
}

// Disabled rules must not count as armed — the whole point of the flag.
func TestScanGridIgnoresDisabledRules(t *testing.T) {
	r := rule("BTC", "range-edge", "1h", 0.5)
	r.Enabled = false
	grid := scanGrid(autotrade.Config{Rules: []autotrade.Rule{r}})
	for _, c := range grid {
		if c.rule.Symbol == "BTC" && c.rule.Strategy == "range-edge" && c.armed {
			t.Error("a disabled rule must not be reported as armed")
		}
	}
}
