package macro

import (
	"strings"
	"testing"
	"time"

	// Embedded so this check does not depend on the host having tzdata. The
	// whole point is to catch a timezone mistake, which it cannot do if
	// LoadLocation fails and the test skips.
	_ "time/tzdata"
)

// releaseHoursET are the Eastern wall-clock times US macro releases land on.
// BLS and BEA publish data at 8:30 a.m. ET; the FOMC decision and minutes go
// out at 2:00 p.m. ET.
var releaseHoursET = map[string]bool{"08:30": true, "14:00": true}

// nonStandardET are events that legitimately sit at another hour, listed by
// name so adding one is a deliberate act rather than a loosened rule.
var nonStandardET = map[string]string{
	"Jackson Hole — Warsh keynote": "10:00", // a scheduled keynote, not a data release
}

// TestCuratedEventTimesAreARealEasternReleaseHour converts every curated
// event back to Eastern time and requires it to land on an actual release
// hour.
//
// This exists because two entries did not. US PCE (October) and US PCE
// (November) were stored as 12:30:00Z, copied from the summer rows — but both
// dates fall after US DST ends on 2026-11-01, so 12:30Z is 07:30 ET, an hour
// before any release happens. 8:30 a.m. ET is 12:30Z under EDT and 13:30Z
// under EST, and the table is written in UTC, so every hand-entered row
// crossing a DST boundary is one copy-paste away from this.
//
// It is not a cosmetic error. A -60/+90 window centred an hour early keeps
// the release covered but cuts post-release protection from 90 minutes to 30:
// with the window at [11:30Z, 14:00Z] and the print at 13:30Z, the 1h bar
// closing at 14:00Z falls on the window's exclusive end and is evaluated
// normally — 30 minutes after the number. That is the bar that matters. On
// 2026-09-10 the hourly bar containing the PPI print moved BTC 1,290 points.
func TestCuratedEventTimesAreARealEasternReleaseHour(t *testing.T) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}

	evts := All()
	if len(evts) == 0 {
		t.Fatal("no curated events loaded")
	}

	for _, e := range evts {
		local := e.DatetimeUTC.In(et)
		hm := local.Format("15:04")

		if want, ok := nonStandardET[e.Name]; ok {
			if hm != want {
				t.Errorf("%s: %s = %s ET, but its listed exception is %s",
					e.Name, e.DatetimeUTC.Format(time.RFC3339), hm, want)
			}
			continue
		}

		if !releaseHoursET[hm] {
			t.Errorf("%s: %s = %s %s (%s ET), which is not a US release hour.\n"+
				"    Data releases are 08:30 ET, FOMC is 14:00 ET.\n"+
				"    8:30 ET = 12:30Z under EDT (2nd Sun Mar - 1st Sun Nov) but 13:30Z under EST.\n"+
				"    If this row was copied from a summer one and its date is in Nov/Dec, add the hour.\n"+
				"    If the event genuinely runs at another time, add it to nonStandardET.",
				e.Name, e.DatetimeUTC.Format(time.RFC3339), local.Format("01/02 15:04"), local.Format("MST"), hm)
		}
	}
}

// TestCuratedBlackoutWindowsMatchTheirEventType pins the windows so a
// hand-added row does not arrive with a window borrowed from the wrong kind
// of event — a PPI row with CPI's +90, say.
func TestCuratedBlackoutWindowsMatchTheirEventType(t *testing.T) {
	// Precedents already in the table, by category.
	want := map[string]struct{ before, after int }{
		"cpi":  {60, 90},
		"nfp":  {60, 90},
		"pce":  {60, 90},
		"ppi":  {30, 60},
		"fomc": {60, 120},
	}

	for _, e := range All() {
		var cat string
		switch {
		case strings.Contains(e.Name, "CPI"):
			cat = "cpi"
		case strings.Contains(e.Name, "PPI"):
			cat = "ppi"
		case strings.Contains(e.Name, "NFP"):
			cat = "nfp"
		case strings.Contains(e.Name, "PCE"):
			cat = "pce"
		case strings.Contains(e.Name, "FOMC Rate Decision"):
			cat = "fomc"
		default:
			continue // minutes, keynotes: one-offs, not pinned here
		}
		w := want[cat]
		if e.BeforeMinutes != w.before || e.AfterMinutes != w.after {
			t.Errorf("%s: window -%d/+%d, but every other %s row uses -%d/+%d",
				e.Name, e.BeforeMinutes, e.AfterMinutes, cat, w.before, w.after)
		}
	}
}
