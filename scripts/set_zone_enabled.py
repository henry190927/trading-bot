#!/usr/bin/env python3
"""List / arm / retire manual zones in /opt/trading/zones.json.

Run it on the VPS, or pipe it there from a workstation:

    ssh ... 'python3 - --list'                        < scripts/set_zone_enabled.py
    ssh ... 'python3 - --symbol BTC --lo 79135 --off' < scripts/set_zone_enabled.py

--list is read-only and is the default when no action is given, so a bad
selector shows you the table instead of silently touching the wrong row.

Selection is by symbol plus (optionally) the band, because zones.json carries
several rows per symbol and index positions shift whenever a row is added.
A selector that matches more than one row is refused rather than guessed at.

Why the temp-file dance
-----------------------
The zone-alert loop re-reads this file on its own poll, and a JSON parse error
there means the daemon skips the file — every armed zone silently stops being
watched. So: write .tmp, re-parse the .tmp, and only then os.replace() it into
position. The live file is never in a state the daemon would reject.

zones.json is the MANUAL half of the channel; the auto pivot zones are derived
separately (zone.ComputeAuto over market.All()) and are not in this file.
"""

import argparse
import json
import os
import sys

PATH = "/opt/trading/zones.json"


def rows(cfg):
    return cfg if isinstance(cfg, list) else cfg.get("zones", [])


def show(cfg):
    print(f"{'#':>2}  {'sym':<5} {'lo':>12} {'hi':>12}  {'dir':<6} {'tf':<3} "
          f"{'confirm':<12} {'on':<5} note")
    for i, z in enumerate(rows(cfg)):
        note = (z.get("note") or "")
        if len(note) > 44:
            note = note[:41] + "..."
        print(f"{i:>2}  {z.get('symbol',''):<5} {z.get('lo',0):>12} {z.get('hi',0):>12}  "
              f"{z.get('dir',''):<6} {z.get('tf',''):<3} {str(z.get('confirm','')):<12} "
              f"{str(bool(z.get('enabled'))):<5} {note}")


def main() -> int:
    ap = argparse.ArgumentParser(description="List / arm / retire manual zones.")
    ap.add_argument("--path", default=PATH)
    ap.add_argument("--list", action="store_true", help="print the table and exit")
    ap.add_argument("--symbol", help="short symbol of the row to change, e.g. BTC")
    ap.add_argument("--lo", type=float, help="band low, to disambiguate rows")
    ap.add_argument("--hi", type=float, help="band high, to disambiguate rows")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--on", action="store_true", help="arm the selected zone")
    g.add_argument("--off", action="store_true", help="retire the selected zone")
    args = ap.parse_args()

    if not os.path.exists(args.path):
        print(f"ERROR: {args.path} not found", file=sys.stderr)
        return 1
    with open(args.path) as f:
        cfg = json.load(f)

    if args.list or not (args.on or args.off):
        show(cfg)
        return 0
    if not args.symbol:
        print("ERROR: --on/--off needs --symbol", file=sys.stderr)
        return 1

    want = args.symbol.strip().upper()
    hits = []
    for i, z in enumerate(rows(cfg)):
        if str(z.get("symbol", "")).strip().upper() != want:
            continue
        if args.lo is not None and abs(float(z.get("lo", 0)) - args.lo) > 1e-6:
            continue
        if args.hi is not None and abs(float(z.get("hi", 0)) - args.hi) > 1e-6:
            continue
        hits.append(i)

    if not hits:
        print(f"ERROR: no zone matched symbol={want} lo={args.lo} hi={args.hi}", file=sys.stderr)
        show(cfg)
        return 1
    if len(hits) > 1:
        print(f"ERROR: {len(hits)} zones matched (rows {hits}) — add --lo/--hi to "
              f"pick one; refusing to guess", file=sys.stderr)
        show(cfg)
        return 1

    i = hits[0]
    z = rows(cfg)[i]
    before = bool(z.get("enabled"))
    z["enabled"] = bool(args.on)

    tmp = args.path + ".tmp"
    with open(tmp, "w") as f:
        f.write(json.dumps(cfg, indent=2, ensure_ascii=False) + "\n")
    with open(tmp) as f:
        json.load(f)  # parse-check BEFORE the daemon can read it
    os.replace(tmp, args.path)

    print(f"row {i}: {z.get('symbol')} {z.get('lo')}-{z.get('hi')} {z.get('dir')} "
          f"enabled {before} -> {bool(args.on)}")
    print()
    show(cfg)
    return 0


if __name__ == "__main__":
    sys.exit(main())
