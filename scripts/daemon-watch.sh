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
SERVICES="trading-monitor trading-web"
DATA=/opt/trading
STATE=$DATA/.daemon-state

# Liveness from each service's own signal, not from whatever runs it (systemd
# before, Docker now): the monitor rewrites its heartbeat every 15s, the web
# answers /health. This file did not change when the services moved into
# containers, and will not when they move again. 60s matches heartbeat.MaxAge.
is_up() {
  case "$1" in
    trading-monitor)
      local f=$DATA/monitor-heartbeat.json
      [ -f "$f" ] && [ $(( $(date +%s) - $(stat -c %Y "$f") )) -lt 60 ] ;;
    trading-web)
      local bind
      bind=$(grep -E '^WEB_BIND=' "$DATA/deploy.env" 2>/dev/null | cut -d= -f2-)
      [ -n "$bind" ] && curl -fsS -m 5 "http://$bind/health" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}
touch "$STATE"

push() { curl -s -H "Title: $1" -H "Priority: ${3:-default}" -H "Tags: $4" -d "$2" "$SERVER/$TOPIC" >/dev/null; }

if [ "${1:-check}" = "heartbeat" ]; then
  line=""; for s in $SERVICES; do is_up "$s" && line="$line ✅$s" || line="$line ❌$s"; done
  push "🩺 daemon heartbeat" "$line" default heartbeat
  exit 0
fi

for s in $SERVICES; do
  is_up "$s" && cur=up || cur=down
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
