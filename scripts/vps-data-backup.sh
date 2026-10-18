#!/usr/bin/env bash
# Daily off-box copy of the VPS state that cannot be rebuilt, into the
# PRIVATE repo henry190927/trading-bot-data. Runs from the ubuntu crontab:
#
#   15 4 * * * set -a; . /opt/trading/.env; set +a; /opt/trading/vps-data-backup.sh >> /opt/trading/vps-data-backup.log 2>&1
#
# Why git and not a copy: every change is a commit, so an overwritten or
# mis-edited journal row can be diffed and restored (editing a row recomputes
# R — a scale-out's true R is lost on the first edit).
#
# Auth is a deploy key with write access to that one repo only — never a
# personal key. Failures and a shrinking journal are pushed to ntfy, because a
# backup that fails silently is the same as no backup.
set -uo pipefail

DATA="${DATA:-/opt/trading}"
REPO_DIR="${REPO_DIR:-$HOME/trading-bot-data}"
REMOTE="${REMOTE:-git@github.com:henry190927/trading-bot-data.git}"
KEY="${KEY:-$HOME/.ssh/trading-bot-data-deploy}"
# Explicit allow-list. .env is NEVER copied (live exchange keys). oi.jsonl is
# left out: rebuildable, multi-MB and growing, it would bloat the history.
FILES=(journal.csv setups.jsonl autotrade.json autotrade_paper.jsonl
       autotrade_blocked.jsonl zones.json zone-config.json chart_state.json
       macro-overlay.json alerts.jsonl zone-events.jsonl monitor-config.json)

export GIT_SSH_COMMAND="ssh -i $KEY -o IdentitiesOnly=yes -o BatchMode=yes"
GIT=(git -c user.name="trading-bot VPS" -c user.email="henry190927@users.noreply.github.com")

notify() {  # $1 title, $2 body
    [ -n "${NTFY_TOPIC:-}" ] || return 0
    curl -fsS -m 15 -H "Title: $1" -H "Priority: high" -H "Tags: warning" \
         -d "$2" "${NTFY_SERVER:-https://ntfy.sh}/$NTFY_TOPIC" >/dev/null || true
}
fail() { echo "$(date -Is) FAIL: $1" >&2; notify "VPS data backup FAILED" "$1"; exit 1; }

[ -f "$DATA/journal.csv" ] || fail "$DATA/journal.csv is missing"

if [ ! -d "$REPO_DIR/.git" ]; then
    git clone -q "$REMOTE" "$REPO_DIR" 2>/dev/null || fail "clone $REMOTE (deploy key / repo access?)"
fi
cd "$REPO_DIR" || fail "cd $REPO_DIR"
if git rev-parse -q --verify HEAD >/dev/null; then
    git pull -q --ff-only || fail "pull --ff-only (someone pushed a divergent history?)"
else
    git checkout -q -B main   # first run against an empty repo
fi

printf '.env\n.env.*\n' > .gitignore
for f in "${FILES[@]}"; do
    [ -f "$DATA/$f" ] && cp -p "$DATA/$f" "$f"
done

# A journal that loses >10% of its rows between snapshots is far more likely
# an accident than trading. Still commit it (the history keeps the old one),
# but say so.
prev=$(git show HEAD:journal.csv 2>/dev/null | wc -l)
now=$(wc -l < journal.csv)
if [ "$prev" -gt 0 ] && [ "$now" -lt $(( prev * 9 / 10 )) ]; then
    notify "journal.csv shrank" "journal.csv went from $prev to $now lines — check for an overwrite. The previous version is in trading-bot-data history."
fi

git add -A
if git diff --cached --quiet; then
    echo "$(date -Is) no changes (journal.csv $now lines)"
    exit 0
fi
"${GIT[@]}" commit -q -m "snapshot $(date '+%F %H:%M %Z') · journal.csv $now lines" || fail "commit"
git push -q origin HEAD:main || fail "push to $REMOTE"
echo "$(date -Is) pushed $(git rev-parse --short HEAD) (journal.csv $now lines)"
