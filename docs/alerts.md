# Alerts

How the system tells you something broke. This replaced the `/ops` "系統狀態"
card and the `/today` daemon row (2026-10-18).

## Why incidents, not a status page

A status page answers only when someone looks. An incident records when the
problem started, how long it lasted, and whether anyone saw it. The bracket
guard stalling for four minutes at 03:00 is gone from a page by 03:05. An
incident keeps it until you acknowledge it.

| | Status page (removed) | Alerts |
|---|---|---|
| How you learn | you go and look | it tells you |
| Unit | the state right now | an incident: fired → acknowledged → resolved |
| History | none | `alerts.jsonl` |

## Who watches whom

A process cannot report its own death, so the watcher is never the thing it
watches:

```
daemon-watch (host cron, 10 min) ──watches──▶ web   (/health)  ──▶ ntfy only
web (every 15s)                  ──watches──▶ monitor (heartbeat) ──▶ bell (+ ntfy if critical)
```

If the web service is down, the bell is down with it, so web-down can only
be an ntfy push from daemon-watch. That pairing is the normal commercial
shape: in-app notifications plus one independent outside channel.

## Rules (`cmd/web/alerts.go`, `evaluateAlerts`)

| Key | Severity | Fires when | Notes |
|---|---|---|---|
| `monitor_down` | critical | heartbeat missing, unreadable, or older than 60s | Its push comes from daemon-watch, not the web, so one outage gives one push. While it is open, the `loop_stalled:*` rules are *held* (see below). |
| `loop_stalled:bracket` | critical | the naked-position guard has not started a cycle in max(3×interval, 2m) | The process is still running, so a restart policy cannot see this. |
| `loop_stalled:<other>` | warning | same, for any other monitor loop | Loops that are off or idle by config are not faults. |
| `monitor_restart_loop` | warning | ≥3 monitor starts within 1h | Detected by a change in `started_at`. This catches OOM kills without access to the Docker socket. |
| `price_ws` | warning | exchange WebSocket down, or connected but silent >30s | Only while a page is subscribed. The hub is lazy and pages are the feed's only consumer, so an idle hub clears the alert. (A first version held it instead, and the incident stayed open for as long as nobody was looking.) |
| `macro_overlay` | warning | overlay file failed to load, or has entries that were skipped | This failure is silent in the dangerous direction: "no blackout". |
| `web_started`, `monitor_restarted` | info | one-shot notices | History only: no badge, no push. |

## Lifecycle (`alert` package)

- **Dedup.** Each incident has one stable `Key`. A condition that stays true
  updates the same alert's detail; it does not create a new alert every round.
- **Hysteresis.** A condition fires after 2 consecutive true rounds and
  resolves after 2 consecutive false rounds. So it takes 15–30s to fire, and
  a value hovering on a threshold does not flap.
- **Held (inhibition).** A key prefix the caller cannot judge this round
  neither advances nor resets. When the monitor is down its loops are
  unknown, not healthy. Without holding, a monitor outage would resolve every
  loop alert and then re-fire them all on recovery.
- **Ack is not resolve.** Acknowledging stops the badge from counting the
  alert. The alert stays open until its condition clears.
- **Unread counts resolved alerts too.** An incident that began and ended
  while nobody was looking is the one most worth being told about.

## Storage

`/opt/trading/alerts.jsonl` (env `ALERTS_PATH`) is append-only. It gets one
line per fire, ack, resolve or notice, and each line carries the full alert
snapshot. On start the web replays the file, and the last snapshot per ID
wins. That is event sourcing at the size of one file. Detail refreshes are
kept in memory only, so a long incident does not write a line every 15s.
When this moves to a database (multi-user), the file becomes a table.

## Delivery

| Severity | Bell + panel | Toast | ntfy |
|---|---|---|---|
| critical | ✅ red badge | ✅ | ✅ fire and resolve (except `monitor_down`, see above) |
| warning | ✅ | ✅ | — |
| info | `/alerts` only, with "顯示紀錄" checked | — | — |

The runtime push mute (`/ops`) silences ntfy only. The bell keeps working.

**Polling, not SSE.** The bell polls `/api/alerts` every 20s, and only while
the tab is visible. It also refreshes immediately when the tab becomes
visible again. The rules only change every 15s, so a stream would gain at
most ~20s of latency. The cost would be one connection per tab, and plain
HTTP/1.1 allows six per host. `/chart` and `/today` already hold one each for
prices, so three tabs would exhaust them.

## Not covered

- The web service itself is covered by daemon-watch (ntfy), as above.
- Backup failures and bracket guard naked-position alerts keep their own ntfy
  pushes. They are not in the bell yet.
- The log is never compacted. At a few lines a day this is not a concern for
  years.
