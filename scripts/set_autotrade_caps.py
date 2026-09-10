#!/usr/bin/env python3
"""Set the autotrade global caps in /opt/trading/autotrade.json.

Run it on the VPS, or pipe it there from a workstation:

    ssh ... 'python3 - --halt=-6'            < scripts/set_autotrade_caps.py
    ssh ... 'python3 - --same-side=1'        < scripts/set_autotrade_caps.py
    ssh ... 'python3 - --concurrent=8 --margin=280' < scripts/set_autotrade_caps.py

Only the fields you pass are touched; the rest are left exactly as they are.
Passing nothing changes nothing and just prints the current caps — use that as
a read-only check. Use the --halt=-6 form rather than --halt -6: argparse can
read a bare negative number as an option name.

Replaces the one-shot raise_autotrade_caps.py (4/140 -> 8/280, applied
2026-09-08 17:39) so that the next cap change does not grow a third script.

Why the temp-file dance
-----------------------
autotrade.Load() falls back to Default() on a parse error, SILENTLY. A
half-written or malformed autotrade.json raises nothing an operator would see;
the executor just quietly runs default caps. So: write .tmp, re-parse the .tmp,
and only then os.replace() it into position. The live file is never in a state
Load() would reject.

Cap semantics, from autotrade/caps.go
-------------------------------------
Zero (or negative, for the two ceilings) means UNLIMITED, not "block
everything" — an autotrade.json written before these fields existed unmarshals
them to 0. daily_loss_halt_r is itself negative in normal use, so 0 is its
disabled value.

CheckCaps tests halt -> same-symbol-side -> concurrent -> margin, and the FIRST
one to bind is the one that reports. Two consequences:

  concurrent and margin have to move TOGETHER — 8 slots at 35u needs a 280u
  ceiling, or margin binds at 4 and the concurrent value is inert.

  max_same_symbol_side is reported ahead of the global caps, deliberately:
  "already short ETH" is more useful than "8 open >= cap 8". Setting it to 1
  means the executor will never hold two positions in the same direction on
  the same symbol; it still permits a hedge (long + short on one symbol).

No restart needed: cmd/monitor/autoexec.go reloads the config each tick.
"""

import argparse
import json
import os
import sys

PATH = "/opt/trading/autotrade.json"
FIELDS = ("max_concurrent_total", "max_margin_total_usdt", "daily_loss_halt_r",
          "max_same_symbol_side")


def main() -> int:
    ap = argparse.ArgumentParser(description="Set autotrade global caps.")
    ap.add_argument("--concurrent", type=int, help="max_concurrent_total (0 = unlimited)")
    ap.add_argument("--margin", type=float, help="max_margin_total_usdt (0 = unlimited)")
    ap.add_argument("--halt", type=float, help="daily_loss_halt_r, e.g. -6 (0 = disabled)")
    ap.add_argument("--same-side", type=int, dest="same_side",
                    help="max_same_symbol_side: positions allowed per (symbol, direction). "
                         "0 = unlimited (ships this way), 1 = never repeat an opinion already open")
    ap.add_argument("--path", default=PATH, help=f"config path (default {PATH})")
    args = ap.parse_args()

    if not os.path.exists(args.path):
        print(f"ERROR: {args.path} not found", file=sys.stderr)
        return 1

    with open(args.path) as f:
        cfg = json.load(f)

    before = {k: cfg.get(k) for k in FIELDS}

    changes = {}
    if args.concurrent is not None:
        changes["max_concurrent_total"] = args.concurrent
    if args.margin is not None:
        changes["max_margin_total_usdt"] = args.margin
    if args.halt is not None:
        changes["daily_loss_halt_r"] = args.halt
    if args.same_side is not None:
        changes["max_same_symbol_side"] = args.same_side

    if not changes:
        print("no fields given — current caps, unchanged:")
        for k in FIELDS:
            print(f"  {k:24} = {before[k]!r}")
        return 0

    cfg.update(changes)

    # A concurrent cap only yields that many slots if the margin ceiling can
    # fund them. Warn rather than refuse: the operator may be mid-way through
    # a two-step change, and a hard failure here would be worse than a note.
    conc = cfg.get("max_concurrent_total") or 0
    marg = cfg.get("max_margin_total_usdt") or 0
    slots = {r.get("margin_usdt") for r in cfg.get("rules", []) if r.get("enabled")}
    slots.discard(None)
    if conc > 0 and marg > 0 and len(slots) == 1:
        per = slots.pop()
        fundable = int(marg // per) if per else 0
        if fundable < conc:
            print(
                f"WARNING: margin {marg:g}u funds only {fundable} slots at {per:g}u/rule, "
                f"but concurrent is {conc} — margin will bind first, concurrent is inert",
                file=sys.stderr,
            )
    elif conc > 0 and len(slots) > 1:
        print(
            f"WARNING: enabled rules use mixed margins {sorted(slots)} — cannot check "
            f"whether {marg:g}u funds {conc} slots",
            file=sys.stderr,
        )

    tmp = args.path + ".tmp"
    with open(tmp, "w") as f:
        f.write(json.dumps(cfg, indent=2, ensure_ascii=False) + "\n")
    with open(tmp) as f:
        json.load(f)  # parse-check BEFORE it becomes the live file
    os.replace(tmp, args.path)

    for k in FIELDS:
        mark = "  <-- changed" if k in changes else ""
        print(f"  {k:24} {before[k]!r} -> {cfg.get(k)!r}{mark}")
    print(f"paper={cfg.get('paper')}  enabled={cfg.get('enabled')}  "
          f"rules={len(cfg.get('rules', []))}")
    print("no restart needed — cmd/monitor/autoexec.go reloads config each tick")
    return 0


if __name__ == "__main__":
    sys.exit(main())
