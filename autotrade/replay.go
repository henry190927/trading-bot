package autotrade

// Historical replay that applies the SAME gates the executor applies.
//
// DedupFires answers "one position per rule": it is faithful to the
// one-position-per-rule constraint and blind to everything else. That was fine
// while every symbol had one or two rules. ETH now has four, and on 2026-09-24
// three of them fired the same bar on the same side — so the page counted three
// positions where MaxSameSymbolSide=1 would have admitted one. Six such clusters
// exist across the log, all ETH (no other symbol carries enough rules to collide),
// worth roughly 3R on a -16R book.
//
// This walks the log forward and asks CheckCaps — the function cmd/monitor gates
// on — at every fire. Reusing that function rather than restating its rules is
// the point: the page cannot drift from what actually blocks a trade, which is
// the principle already stated for BuildBook and not previously applied to the
// scoring path.
//
// WHAT THIS IS A COUNTERFACTUAL OF. It replays the whole history against TODAY'S
// config, because that is the question worth asking — "what would the rule set I
// am running now have produced from these signals". It is NOT a record of what
// the executor did at the time: the caps were looser then (concurrency 2, margin
// 100u, no same-side cap at all), and reconstructing the caps-of-the-day would
// need a config history the .bak snapshots only sample. Say which one a number
// is before quoting it.

import (
	"sort"
	"time"
)

// Blocked is a fire the caps refused, kept rather than dropped: a signal that
// existed and was declined is different from a signal that never fired, and the
// difference is invisible if blocked fires are simply discarded.
type Blocked struct {
	Fire   PaperFire
	Reason string
}

type liveLeg struct {
	symbol, side string
	margin       float64
	until        time.Time // zero = still open at the end of the walk
	r            float64
	resolved     bool
}

// ReplayWithCaps consumes fires in ANY order and returns positions oldest-first
// plus the fires the caps refused.
func ReplayWithCaps(fires []PaperFire, cfg Config, expiryBars, cooldownBars int,
	barDur time.Duration, resolve func(PaperFire) Outcome) ([]Position, []Blocked) {

	ordered := orderForReplay(fires, cfg)

	openUntil := map[string]time.Time{}
	curIdx := map[string]int{}
	var live []liveLeg
	dailyR := map[string]float64{}

	var out []Position
	var blocked []Blocked

	for _, f := range ordered {
		// Retire anything that closed before this fire, banking its R into the
		// UTC day it closed on — the same day boundary the breaker resets on.
		kept := live[:0]
		for _, l := range live {
			if l.resolved && !l.until.IsZero() && !l.until.After(f.Time) {
				dailyR[l.until.UTC().Format("2006-01-02")] += l.r
				continue
			}
			kept = append(kept, l)
		}
		live = kept

		// Gate 1: one position per rule. Unchanged from DedupFires, and it runs
		// first because the executor's own loop skips a rule that already holds
		// a position before it ever reaches the caps.
		key := f.Symbol + "|" + f.Strategy
		if u, ok := openUntil[key]; ok && f.Time.Before(u) {
			if i, ok := curIdx[key]; ok {
				out[i].Absorbed++
			}
			continue
		}

		// Gate 2: the caps, via the executor's own function.
		bk := Book{RealizedRToday: dailyR[f.Time.UTC().Format("2006-01-02")]}
		for _, l := range live {
			bk.OpenCount++
			bk.OpenMargin += l.margin
			bk.OpenLegs = append(bk.OpenLegs, Leg{Symbol: l.symbol, Side: l.side})
		}
		if v := CheckCaps(cfg, bk, Candidate{Symbol: f.Symbol, Side: f.Side, Margin: f.Margin}); v.Blocked {
			blocked = append(blocked, Blocked{Fire: f, Reason: v.Reason})
			continue
		}

		oc := resolve(f)
		out = append(out, Position{Fire: f, Outcome: oc})
		curIdx[key] = len(out) - 1

		l := liveLeg{symbol: f.Symbol, side: f.Side, margin: f.Margin, r: oc.NetR}
		var hold time.Time
		switch oc.Status {
		case OutTP, OutStop:
			hold, l.until, l.resolved = oc.ExitAt, oc.ExitAt, true
			if oc.Status == OutStop && cooldownBars > 0 {
				hold = hold.Add(time.Duration(cooldownBars) * barDur)
			}
		case OutNoFill:
			hold = f.Time.Add(time.Duration(expiryBars) * barDur)
			l.until, l.resolved = hold, true
			l.r = 0
		default: // still open
			hold = f.Time.Add(time.Duration(expiryBars) * barDur)
		}
		openUntil[key] = hold
		live = append(live, l)
	}
	return out, blocked
}

// orderForReplay puts fires oldest-first and, WITHIN one timestamp, back into
// the order cmd/monitor evaluates rules.
//
// This matters because the caps make same-bar fires compete: when three ETH
// rules fire together and only one slot exists, which one takes it is decided
// by `for i := range cfg.Rules`. The log is written in that order, but the page
// reverses the whole log to get oldest-first, which silently reverses each
// timestamp's internal order too — handing the slot to the last rule instead of
// the first. A stable sort on (time, rule index) restores it.
func orderForReplay(fires []PaperFire, cfg Config) []PaperFire {
	rank := map[string]int{}
	for i, r := range cfg.Rules {
		rank[r.Symbol+"|"+r.Strategy] = i
	}
	rankOf := func(f PaperFire) int {
		if i, ok := rank[f.Symbol+"|"+f.Strategy]; ok {
			return i
		}
		return len(cfg.Rules) // a retired rule sorts last; it cannot outrank a live one
	}
	out := append([]PaperFire(nil), fires...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return rankOf(out[i]) < rankOf(out[j])
	})
	return out
}
