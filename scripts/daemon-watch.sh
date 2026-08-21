#!/bin/bash
# daemon-watch — ntfy alert when a trading daemon dies (so a silent death like
# trading-bot being down for a week can't happen unnoticed again).
#   check      (cron every ~10min): push ONLY on a state change (up→down alert,
#              down→up recovery), throttled via a state file — no spam.
#   heartbeat  (cron daily): push a "still watching" summary so that SILENCE is
#              trustworthy — if the daily heartbeat stops, the watcher itself died.
set -a; . /opt/trading/.env 2>/dev/null; set +a
TOPIC="${NTFY_TOPIC}"; SERVER="${NTFY_SERVER:-https://ntfy.sh}"
[ -z "$TOPIC" ] && exit 0
SERVICES="trading-bot trading-monitor trading-web"
STATE=/opt/trading/.daemon-state
touch "$STATE"

push() { curl -s -H "Title: $1" -H "Priority: ${3:-default}" -H "Tags: $4" -d "$2" "$SERVER/$TOPIC" >/dev/null; }

if [ "${1:-check}" = "heartbeat" ]; then
  line=""; for s in $SERVICES; do systemctl is-active --quiet "$s" && line="$line ✅$s" || line="$line ❌$s"; done
  push "🩺 daemon heartbeat" "$line" default heartbeat
  exit 0
fi

for s in $SERVICES; do
  systemctl is-active --quiet "$s" && cur=up || cur=down
  prev=$(grep "^$s=" "$STATE" 2>/dev/null | cut -d= -f2)
  if [ "$cur" != "$prev" ]; then
    if [ "$cur" = "down" ]; then
      push "⚠ Daemon DOWN" "$s is DOWN — check /ops" high warning
    elif [ -n "$prev" ]; then
      push "✅ Daemon recovered" "$s is back up" default white_check_mark
    fi
    grep -v "^$s=" "$STATE" 2>/dev/null > "$STATE.tmp"; echo "$s=$cur" >> "$STATE.tmp"; mv "$STATE.tmp" "$STATE"
  fi
done
