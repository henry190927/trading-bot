#!/usr/bin/env python3
"""Add a row to the curated macro calendar (macro/events.json).

    scripts/add_macro_event.py --type cpi --date 2026-11-10
    scripts/add_macro_event.py --type nfp --date 2026-12-04 --dry-run
    scripts/add_macro_event.py --type fomc --date 2026-12-09 --label "FOMC Rate Decision (December)"

Look the dates up on the BLS / BEA release schedules and pass them as plain
calendar dates. Everything else — the UTC timestamp, the blackout window, the
label — is derived, because each of those is a way to get this file wrong.

Why not just edit the JSON
--------------------------
The file stores UTC, but releases are scheduled in Eastern time, so the
correct UTC timestamp for the SAME 8:30 a.m. release changes by an hour when
US DST flips (2nd Sunday of March / 1st Sunday of November).

That is not hypothetical. US PCE (October) and US PCE (November) were both
entered as 12:30:00Z, copied from the summer rows, and both dates are after
DST ends on 2026-11-01 — making them 07:30 ET, an hour before any release
happens. The window still covered the print, but post-release protection fell
from 90 minutes to 30, which is exactly the bar that moves: on 2026-09-10 the
hourly bar containing the PPI print moved BTC 1,290 points.

This script derives the timestamp from the Eastern release hour, so the DST
question never reaches the operator. macro/events_time_test.go checks the
result independently.

After running
-------------
The JSON is go:embed'ed, so it takes a rebuild:

    go test ./macro/ && make deploy-monitor && make deploy-web

Until then the live gate still runs the old table.
"""

import argparse
import calendar
import datetime as dt
import json
import os
import sys
from collections import OrderedDict
from zoneinfo import ZoneInfo

ET = ZoneInfo("America/New_York")

# Release hour in EASTERN time, plus the blackout window each event type uses
# in the existing table. Windows are precedents, not opinions — see the rows
# already in macro/events.json.
TYPES = {
    #            ET time   -before  +after   label template
    "cpi": (("08:30"), 60, 90, "US CPI ({month})"),
    "ppi": (("08:30"), 30, 60, "US PPI ({month})"),
    "nfp": (("08:30"), 60, 90, "US NFP ({month})"),
    "pce": (("08:30"), 60, 90, "US PCE ({month})"),
    "fomc": (("14:00"), 60, 120, None),  # label required: meeting month, not data month
    "minutes": (("14:00"), 30, 90, None),
}

DEFAULT_PATH = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "macro", "events.json"
)


def previous_month_name(d: dt.date) -> str:
    """CPI released in November reports October. The table labels rows by the
    data month, not the release month — 'US CPI (July)' went out 2026-08-12."""
    y, m = (d.year, d.month - 1) if d.month > 1 else (d.year - 1, 12)
    return calendar.month_name[m] + ("" if y == d.year else f" {y}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--type", required=True, choices=sorted(TYPES),
                    help="event type; sets the ET release hour and the blackout window")
    ap.add_argument("--date", required=True, help="release date, YYYY-MM-DD (from the BLS/BEA schedule)")
    ap.add_argument("--label", help="event name; auto-derived for data releases, required for fomc/minutes")
    ap.add_argument("--path", default=DEFAULT_PATH, help=f"default {DEFAULT_PATH}")
    ap.add_argument("--dry-run", action="store_true", help="print the row and exit without writing")
    args = ap.parse_args()

    try:
        day = dt.date.fromisoformat(args.date)
    except ValueError as exc:
        print(f"ERROR: --date {args.date!r}: {exc}", file=sys.stderr)
        return 1

    et_hhmm, before, after, label_tmpl = TYPES[args.type]
    hh, mm = (int(x) for x in et_hhmm.split(":"))

    # The one calculation worth having a script for.
    local = dt.datetime(day.year, day.month, day.day, hh, mm, tzinfo=ET)
    utc = local.astimezone(dt.timezone.utc)

    label = args.label
    if not label:
        if label_tmpl is None:
            print(f"ERROR: --label is required for --type {args.type} "
                  f"(it names the meeting, which this cannot infer)", file=sys.stderr)
            return 1
        label = label_tmpl.format(month=previous_month_name(day))

    row = OrderedDict([
        ("name", label),
        ("datetime_utc", utc.strftime("%Y-%m-%dT%H:%M:%SZ")),
        ("blackout_before_min", before),
        ("blackout_after_min", after),
    ])

    tpe = dt.timezone(dt.timedelta(hours=8))
    win_lo = utc - dt.timedelta(minutes=before)
    win_hi = utc + dt.timedelta(minutes=after)
    print(f"  {label}")
    print(f"    release   {local:%Y-%m-%d %H:%M} {local.tzname()}  =  {row['datetime_utc']}"
          f"  =  {utc.astimezone(tpe):%m/%d %H:%M} TPE")
    print(f"    blackout  -{before}/+{after}  =  "
          f"{win_lo.astimezone(tpe):%m/%d %H:%M} – {win_hi.astimezone(tpe):%H:%M} TPE")

    if not os.path.exists(args.path):
        print(f"ERROR: {args.path} not found", file=sys.stderr)
        return 1
    with open(args.path) as f:
        doc = json.load(f, object_pairs_hook=OrderedDict)

    for e in doc["events"]:
        if e["datetime_utc"] == row["datetime_utc"]:
            print(f"\nalready present: {e['name']} at {e['datetime_utc']} — nothing written")
            return 0

    if args.dry_run:
        print("\n--dry-run: nothing written")
        return 0

    doc["events"].append(row)
    doc["events"].sort(key=lambda e: e["datetime_utc"])

    # tmp -> reparse -> replace. macro.loadEvents surfaces a parse error via
    # LoadError rather than crashing, so a malformed file degrades to "no
    # events" — an empty calendar, which reads as an all-clear.
    tmp = args.path + ".tmp"
    with open(tmp, "w") as f:
        f.write(json.dumps(doc, indent=2) + "\n")
    with open(tmp) as f:
        assert len(json.load(f)["events"]) == len(doc["events"])
    os.replace(tmp, args.path)

    print(f"\nwritten — {len(doc['events'])} events in {args.path}")
    print("next: go test ./macro/ && make deploy-monitor && make deploy-web")
    print("      (events.json is go:embed'ed — the live gate needs the rebuild)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
