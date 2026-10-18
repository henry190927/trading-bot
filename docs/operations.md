# Operations

How the system runs on the VPS, how to deploy it, and how to operate it. For
what the system *is*, see the [README](../README.md); for the CLI tools, see
[cli.md](cli.md).

- [What runs where](#what-runs-where)
- [The data directory](#the-data-directory)
- [Configuration: secrets vs runtime settings](#configuration-secrets-vs-runtime-settings)
- [Deploying](#deploying)
- [Operating](#operating)
- [Push notifications](#push-notifications)
- [Web UI](#web-ui)
- [Network and security model](#network-and-security-model)
- [Monitoring the monitors](#monitoring-the-monitors)
- [Backups and recovery](#backups-and-recovery)
- [Local setup](#local-setup)
- [Provisioning a new VM](#provisioning-a-new-vm)
- [Cost](#cost)

---

## What runs where

One Oracle Cloud Always Free VM (`VM.Standard.E2.1.Micro`: x86_64, 1 GB RAM,
2 GB swap file), reachable only over Tailscale and SSH.

| Unit / job | Binary | What it does |
|---|---|---|
| `trading-web.service` | `trading-web` (`cmd/web`) | Gin UI, SSE price stream, optional gRPC. Bound to the Tailscale IP, never `0.0.0.0`. |
| `trading-monitor.service` | `trading-monitor` (`cmd/monitor`) | Every long-running loop: zone alerts, auto-executor (paper), naked-position guard, macro pre-warnings, OI / BLS / Binance-positioning refreshers, and — outside zone-only mode — the confluence scan and structure alerts. |
| cron `10 0 * * *` | `earnings-fetch` | Earnings calendar → `earnings.json` |
| cron `30 0 * * *` | `fundamental-scan` | Fundamentals board → `fundamentals.json` |
| cron `*/10 * * * *` | `daemon-watch.sh check` | ntfy on a unit going down or recovering |
| cron `0 0 * * *` | `daemon-watch.sh heartbeat` | Daily "still watching" push, so silence is trustworthy |
| cron `15 4 * * *` | `vps-data-backup.sh` | Daily off-box backup (see [Backups](#backups-and-recovery)) |

Both units run as `ubuntu` with `WorkingDirectory=/opt/trading`,
`EnvironmentFile=/opt/trading/.env`, `Environment=TZ=Asia/Taipei` and
`Restart=always`.

> The original `trading-bot.service` (`cmd/serve`) is retired and **disabled**.
> It is not masked, so a `systemctl restart trading-bot` would still start it
> beside the monitor — nothing in the Makefile does that any more.

## The data directory

Everything lives in `/opt/trading`, as flat files. Every path is
env-overridable, which is what makes a local or fixture-backed instance cheap.

| File | Written by | Read by | Notes |
|---|---|---|---|
| `.env` | the operator, over SSH | both units (systemd `EnvironmentFile`) | **Secrets only.** No process writes it at runtime. |
| `monitor-config.json` | web (`/ops` mute toggle) | monitor, every send | Runtime settings — see below |
| `zone-config.json` | web (`/ops` zone panel) | monitor, every cycle | Zone channel on/off, auto zones, TFs |
| `zones.json` | the operator | monitor, web | Manual zones |
| `autotrade.json` | the operator, over SSH | monitor, web | Auto-executor rules and caps |
| `journal.csv` | web | web, monitor (read-only) | The trade journal, 30 columns |
| `monitor-heartbeat.json` | monitor, every 15s | web, `ops status` | Self-reported loop state |
| `zone-events.jsonl` | monitor | web, `ops zone` | Zone fires / skips / failed pushes |
| `autotrade_paper.jsonl`, `autotrade_blocked.jsonl` | monitor | web | Paper fires, and candidates the caps refused |
| `oi.jsonl`, `bls.json`, `binfut.json` | monitor | web | Data the monitor is the only process able to collect |
| `setups.jsonl`, `chart_state.json`, `ai_model.txt` | web | web | UI state |

The rule the table encodes: **the web service writes configuration files and
the journal, never `.env` and never a unit.** Before 2026-10-17 it rewrote
`.env` to mute pushes and restarted the monitor with `sudo`.

## Configuration: secrets vs runtime settings

| Kind | Where | Changed by | Takes effect |
|---|---|---|---|
| Secrets — `BINGX_API_KEY/SECRET`, `NTFY_TOPIC`, AI keys | `.env` | editing over SSH, then `ops restart` | on restart |
| Process settings — `MONITOR_ZONE_ONLY`, `MONITOR_BRACKET(_MODE)`, `MONITOR_TFS`, … | `.env` | editing over SSH, then `ops restart monitor` | on restart |
| Runtime settings — push mute | `monitor-config.json` | the `/ops` page | next send, no restart |
| Zone channel | `zone-config.json` | the `/ops` page | next cycle, no restart |

`monitor-config.json` fails **open**: if it does not parse, the monitor keeps
push ON and logs it once. The naked-position alarm shares the push channel, so
a corrupt file must not be able to silence it.

## Deploying

Every deploy cross-compiles on the Mac, `scp`s the binary and restarts the
unit:

```bash
make deploy-web        # trading-web
make deploy-monitor    # trading-monitor
make deploy-all        # the CLI tools (analyze/validate/backtest/price/journal); restarts nothing
make deploy-ops        # scripts/ops → /usr/local/bin/ops (asks for the sudo password)
```

**Deploys refuse a dirty working tree.** A deploy compiles whatever is in the
working tree, so it would ship another session's uncommitted work — including
untracked `.go` files, which are compiled in but never show in `git diff`.
`guard-clean` blocks the deploy and lists the files; `FORCE=1` overrides.

To deploy a commit while the working tree has unrelated work in progress,
deploy from a clean worktree:

```bash
git worktree add --detach /tmp/deploy <commit>
cp Makefile.local /tmp/deploy/          # host settings, gitignored
make -C /tmp/deploy deploy-web
git worktree remove --force /tmp/deploy
```

Before deploying the monitor, check what else ships with it — the binary on
the VPS may be several commits behind:

```bash
ssh trading-bot 'stat -c %y /opt/trading/trading-monitor'      # when it was last deployed
git log --since='<that time>' -- $(go list -deps ./cmd/monitor | grep trading-bot | sed 's#.*/trading-bot/##')
```

## Operating

Service control is an SSH job, through one script:

```bash
ssh trading-bot ops status                 # units + monitor heartbeat
ssh trading-bot ops logs monitor -f        # follow a journal (or: web; N lines: ops logs monitor 200)
ssh trading-bot ops restart monitor        # restart (asks for the sudo password)
ssh trading-bot ops zone 20                # recent zone-alert events
```

`ops` wraps systemd today. When the services move into containers only its
function bodies change; the commands stay the same.

The `/ops` page is the read-mostly side: unit and heartbeat state, the zone
channel, the auto-executor book, exchange verification (`/ops/verify` — always
re-check a new position's stop here rather than trusting the journal), fills,
orders, balance and OI. It changes runtime settings (push mute, zone channel)
but does not start, stop or restart anything, and does not stream raw logs.

## Push notifications

Alerts go to [ntfy](https://ntfy.sh) at priority 4, which bypasses iOS Focus
modes.

1. Install the **ntfy** app (iOS / Android).
2. Pick a hard-to-guess topic — anyone with the name can read your alerts:
   `uuidgen`.
3. Subscribe to it in the app.
4. Set `NTFY_TOPIC=<topic>` in `/opt/trading/.env`, then
   `ssh trading-bot ops restart monitor`.
5. Test the wiring without waiting for a signal:
   `curl -d "test" https://ntfy.sh/<topic>`

**Muting** (`/ops` → 🔇) skips the push and nothing else: every loop keeps
running, zone fires are still recorded (as `(muted, not pushed)`), and the
heartbeat shows push-only loops as `idle`. Un-muting applies on the next send.
A mute also silences the naked-position alarm — the confirm dialog says so.

## Web UI

`http://<tailscale-ip>:8080`, mobile-first, server-rendered (Gin +
`html/template`, templates and CSS embedded with `//go:embed`).

| Route | Page |
|---|---|
| `/` | Dashboard — engine snapshot per symbol, tap-to-copy price levels |
| `/chart` | Chart — the live-trade surface: levels, structure, and recording a trade from a price |
| `/journal`, `/journal/new`, `/journal/:id/edit`, `/journal/:id/close` | Portfolio — trades, stats, the partials calculator for multi-leg exits |
| `/validate` | Score a proposed entry |
| `/setups`, `/scan` | Setup tracking with the path-dependent real outcome; current setups |
| `/today` | Today — open trades, unit health, pending setups |
| `/tips` | Lessons distilled from the journal |
| `/calendar` | Macro calendar (curated events gate the engine; the live feed is display-only) |
| `/tw`, `/fundamentals`, `/onchain` | Taiwan stocks (TWSE open data), US stock fundamentals, on-chain |
| `/ops`, `/ops/autotrade`, `/ops/verify` | Operations — see [Operating](#operating) |

Add it to the iPhone home screen from Safari's share menu for a full-screen
app.

## Network and security model

Three independent layers gate access; an attacker would have to defeat all three:

```
   Stranger on public internet                  You on Tailscale-connected device
   ──────────────────────────                   ────────────────────────────────
              │                                                │
              ▼                                                ▼
   ① 100.79.x.x is Tailscale CGNAT     ┌─ ① Wireguard routes 100.x.x.x to VPS
      → not routable on public DNS     │
      ECONNREFUSED ❌                  │
                                       │
   (try public IP your.vps.ip:8080)   │
              │                        │
              ▼                        ▼
   ② ufw allows only 22 on eth0      ② ufw allows tailscale0 → forward
      port 8080 dropped ❌
                                       │
              ▼                        ▼
   ③ trading-web bound to            ③ socket on 100.x.x.x:8080 accepts
      100.x.x.x — not 0.0.0.0
      no listener on public iface ❌
                                       │
                                       ▼
                                       Gin handles request (no app-auth)
```

App-level auth is intentionally skipped because Tailscale + ufw + bind-address already provide three independent failures-required to breach. Add cookie session or OAuth later only if (a) you expose publicly via Cloudflare Tunnel, (b) share tailnet with teammates, or (c) want an audit log.

## Monitoring the monitors

| Signal | Source | Catches |
|---|---|---|
| ntfy "Daemon DOWN / recovered" | `daemon-watch.sh` every 10 min | a unit stopping |
| Daily "daemon heartbeat" push | `daemon-watch.sh` at 00:00 | the watcher itself dying — silence becomes trustworthy |
| `monitor-heartbeat.json` | the monitor, every 15s | a loop that is alive but stuck (`stalled`), a loop idled by config or mute — what `systemctl is-active` cannot see |
| `/ops` health card | web | all of the above, on the phone |

## Backups and recovery

- **Daily off-box backup** (`scripts/vps-data-backup.sh`, cron 04:15): an
  explicit allow-list of the files that cannot be rebuilt — `journal.csv`,
  `setups.jsonl`, `autotrade.json`, `autotrade_paper.jsonl`,
  `autotrade_blocked.jsonl`, `zones.json`, `zone-config.json`,
  `chart_state.json`, `macro-overlay.json` — is committed to a **private** GitHub
  repo with a write-only-to-that-repo deploy key. Every change is a commit,
  so a mis-edited journal row can be diffed and restored. `.env` is never
  copied. Failures and a shrinking journal push to ntfy.
- **Why off-box, and off-OCI:** the 2026-10 incident was a single access path
  failing (MFA on the Oracle account), not lost storage — a backup inside the
  same account would have been just as unreachable. See
  [architecture_hardening.md](architecture_hardening.md).
- **SSH trouble:** start with [ssh_recovery_runbook.md](ssh_recovery_runbook.md).
  The first thing to check is the local `ssh-agent` (an empty agent plus a
  passphrase key hidden by `BatchMode` looked exactly like a server lockout).

## Local setup

1. **`.env`** (gitignored) — `cp .env.example .env`. Public market data needs
   no keys; `BINGX_API_KEY/SECRET` only for account endpoints, `NTFY_TOPIC`
   for pushes.
2. **`Makefile.local`** (gitignored) — `cp Makefile.local.example
   Makefile.local` and set `ORACLE_HOST` / `ORACLE_USER` / `ORACLE_KEY` for
   the deploy targets.
3. **`~/.ssh/config`** — a `trading-bot` host so `ssh trading-bot ops ...`
   works, loading the passphrase key from the macOS keychain so a reboot does
   not leave the agent empty:

   ```
   Host trading-bot
       HostName <tailscale-or-public-ip>
       User ubuntu
       IdentityFile ~/.ssh/<key>
       UseKeychain yes
       AddKeysToAgent yes
   ```

4. **Tailscale** on the Mac, the phone and the VPS, in one tailnet. Find the
   VPS address with `tailscale ip -4` and set `WEB_BIND=<that>:8080` in the
   `trading-web` unit.

## Provisioning a new VM

Not scripted yet — making a rebuild cheap is the point of
[architecture_hardening.md](architecture_hardening.md) and of the container
work in [`k8s/`](../k8s/). Until then, by hand:

1. Oracle Cloud → *Always Free* → Ubuntu 22.04 on `VM.Standard.E2.1.Micro`
   (x86_64; `A1.Flex` ARM also works if you build with `GOARCH=arm64`), with a
   public IPv4 and a generated SSH key.
2. `sudo timedatectl set-timezone Asia/Taipei`; add a 2 GB swap file (1 GB of
   RAM has no headroom without one).
3. Install Tailscale; `ufw` allowing only 22 on the public interface.
4. Create `/opt/trading` owned by `ubuntu`, put `.env` there (`chmod 600`),
   install the two units (see [What runs where](#what-runs-where)), the
   crontab, and `make deploy-web deploy-monitor deploy-all deploy-ops`.
5. Restore the data files from the private backup repo.

## Cost

| Item | Cost |
|---|---|
| VM (E2 micro or A1 ARM in Always Free) | $0 |
| 200 GB block storage (Always Free) | $0 |
| 10 TB egress/month (Always Free) — bot uses ~1 MB/day | $0 |
| ntfy.sh public service | $0 |
| BingX public REST | $0 |
| **Total** | **$0/month forever** |

You stay in Always Free as long as you don't spin up more than:
- 2 AMD VMs OR 4 ARM OCPUs total
- 200 GB block storage
- 2 Block Volume Backups

For a single trading bot, you'll use about 1% of that budget.
