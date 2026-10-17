#!/bin/bash
# hb-compare — TEMPORARY. Samples the /ops heartbeat side-by-side so the
# one-day comparison rests on the whole day, not on a single look at the
# page the next morning: a stretch of disagreement between two looks would
# otherwise be invisible.
#
# Runs from the ubuntu crontab every 10 minutes. Read-only: one GET to the
# local web service, one line appended to the log. Remove this script and
# its crontab line when the inferred loop card is retired (step 3).
#
# Log line: time  alive  hb_age  systemd  agree_systemd  mismatches  detail
set -u
LOG="${HB_COMPARE_LOG:-/opt/trading/hb-compare.log}"
URL="${HB_COMPARE_URL:-http://100.79.112.56:8080/ops/services}"

if ! out=$(curl -s -m 10 "$URL"); then
    echo "$(date '+%F %T') fetch-failed" >> "$LOG"
    exit 0
fi

printf '%s' "$out" | python3 -c '
import json, sys, datetime
now = datetime.datetime.now().strftime("%F %T")
try:
    h = json.load(sys.stdin)["heartbeat"]
except Exception as e:
    print(now, "bad-response", type(e).__name__)
    sys.exit(0)
detail = []
for l in h.get("loops", []):
    if l.get("agree") is False:
        detail.append("%s:hb=%s,inferred=%s" % (l["name"], l["state"], "on" if l.get("inferred_on") else "off"))
    elif l.get("state") == "stalled":
        detail.append("%s:stalled(%ss)" % (l["name"], l.get("age_sec")))
print(now,
      "alive" if h.get("alive") else "DEAD",
      "age=%ss" % h.get("age_sec"),
      "systemd=" + ("active" if h.get("systemd_active") else "inactive"),
      "agree_systemd=" + ("y" if h.get("agree_systemd") else "N"),
      "mismatches=%d" % h.get("mismatches", 0),
      " ".join(detail) if detail else "-")
' >> "$LOG"
