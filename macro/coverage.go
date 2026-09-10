package macro

import (
	"sort"
	"time"
)

// Candidate is one externally-sourced event to test against the curated
// blackout table.
//
// Dependency-free on purpose. The live feed lives in package econcal, which
// fetches over the network; macro has to stay a pure embedded table that the
// engine can consult with no I/O, so the caller does the mapping and macro
// never learns where the candidates came from.
type Candidate struct {
	Name        string
	Country     string
	DatetimeUTC time.Time
}

// Uncovered returns the candidates in [from, from+horizon) whose event time
// falls outside EVERY curated blackout window, soonest first.
//
// Why this exists. The curated table is hand-maintained, and on 2026-09-10 it
// had gone stale without saying so: CPI entries stopped after the July print,
// PPI had exactly one row, and NFP stopped after August. That day an ECB
// refinancing-rate hike (2.40% -> 2.65%) and a US PPI print (0.0% -> 0.4%
// forecast) landed fifteen minutes apart, produced a 1,290-point BTC hourly
// bar and a -4.67% day on XAG, and the dashboard said "no macro events on the
// calendar". That was *true* — neither event was in the table — which is
// precisely the failure. A gate that quietly stops covering things is worse
// than no gate at all, because an empty calendar reads as an all-clear.
//
// So: report the gap rather than guess at the missing rows. The free
// ForexFactory feed is one week wide (nextweek/thismonth 404) and bls.gov
// answers 403, so there is no source in the system for next quarter's release
// dates — inventing them would put wrong hours into something that suppresses
// trading. Naming what the gate does NOT cover needs no such source.
//
// Deliberately reports rather than blocks. Promoting the live feed into the
// gate would let a third party's impact ratings start suppressing real trades
// — a behaviour change, and this feed is already known to misrate central-bank
// speeches (see econcal.IsCentralBankSpeaker). Saying "the gate misses this"
// changes nothing about what fires.
func Uncovered(cands []Candidate, from time.Time, horizon time.Duration) []Candidate {
	if len(cands) == 0 || horizon <= 0 {
		return nil
	}
	from = from.UTC()
	until := from.Add(horizon)
	evts := All()

	var out []Candidate
	for _, c := range cands {
		t := c.DatetimeUTC.UTC()
		if t.Before(from) || !t.Before(until) {
			continue
		}
		covered := false
		for _, e := range evts {
			if e.Contains(t) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DatetimeUTC.Before(out[j].DatetimeUTC)
	})
	return out
}
