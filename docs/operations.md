# Operations

How the system runs on the VPS, how to deploy it, and how to operate it. For
what the system *is*, see the [README](../README.md); for the CLI tools, see
[cli.md](cli.md).

- [What runs where](#what-runs-where)
- [The data directory](#the-data-directory)
- [Configuration: secrets vs runtime settings](#configuration-secrets-vs-runtime-settings)
- [Deploying](#deploying)
- [Cut-over and rollback](#cut-over-and-rollback)
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

| Service / job | Runs as | What it does |
|---|---|---|
| `trading-web` | container, `ghcr.io/henry190927/trading-bot-web:<sha>` | Gin UI, SSE price stream, optional gRPC. Bound to the Tailscale IP, never `0.0.0.0`. |
| `trading-monitor` | container, `ghcr.io/henry190927/trading-bot-monitor:<sha>` | Every long-running loop: zone alerts, auto-executor (paper), naked-position guard, macro pre-warnings, OI / BLS / Binance-positioning refreshers, and — outside zone-only mode — the confluence scan and structure alerts. |
| cron `10 0 * * *` | `earnings-fetch` | Earnings calendar → `earnings.json` |
| cron `30 0 * * *` | `fundamental-scan` | Fundamentals board → `fundamentals.json` |
| cron `*/10 * * * *` | `daemon-watch.sh check` | ntfy on a service going down or recovering |
| cron `0 0 * * *` | `daemon-watch.sh heartbeat` | Daily "still watching" push, so silence is trustworthy |
| cron `15 4 * * *` | `vps-data-backup.sh` | Daily off-box backup (see [Backups](#backups-and-recovery)) |

Both containers are defined in [`deploy/compose.yaml`](../deploy/compose.yaml):
host networking, UID 1001 (the owner of `/opt/trading`), read-only root, no
capabilities, `restart: unless-stopped`, logs to journald, `mem_limit` with
`GOMEMLIMIT` at about 80% of it. Secrets come from `/opt/trading/.env` at
runtime; `/opt/trading/deploy.env` holds `TAG` (the commit running) and
`WEB_BIND`. The images are `FROM scratch` — no shell, nothing to `exec` into.

The systemd units `trading-web.service` and `trading-monitor.service` are
**disabled, not deleted**: they and the binaries in `/opt/trading` are the
rollback path (see [Cut-over and rollback](#cut-over-and-rollback)). The
original `trading-bot.service` (`cmd/serve`) is retired and disabled too.

## The data directory

Everything lives in `/opt/trading`, as flat files. Every path is
env-overridable, which is what makes a local or fixture-backed instance cheap.

| File | Written by | Read by | Notes |
|---|---|---|---|
| `.env` | the operator, over SSH | both containers (compose `env_file`) | **Secrets only.** No process writes it at runtime. |
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

**The services deploy through CI.** A push to `main` runs
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml):

```
check   go vet + go test -race                     every push and PR
image   build web + monitor, tag :<sha>, push      main only
deploy  waits for approval on `production`, then   main only
        ssh <sha> → /usr/local/bin/trading-deploy
```

Docs-only commits (`*.md`, `docs/`, `k8s/`) do not trigger it.

`trading-deploy` ([source](../deploy/trading-deploy)) is the only thing CI's
deploy key can run: its `authorized_keys` entry is

```
command="sudo /usr/local/bin/trading-deploy \"$SSH_ORIGINAL_COMMAND\"",restrict ssh-ed25519 AAAA… ci-deploy
```

so whatever the client sends arrives as one argument, which must be a 40-char
commit SHA. It fetches **that commit's** `deploy/compose.yaml` (image and
settings always come from one commit), pulls, `up -d`, and within 60 seconds
requires web `/health` 200 and a monitor heartbeat from a **new** process
(its `started_at` changed — not its pid, which is 1 in every container) — or it
puts the previous `TAG` back and fails the job. Sudoers allows `ubuntu`
exactly that script, nothing else.

What still deploys from the Mac:

```bash
make deploy-all        # the CLI tools (analyze/validate/backtest/price/journal); restarts nothing
make deploy-ops        # host tooling: ops, trading-deploy → /usr/local/bin (root-owned); daemon-watch.sh
```

These refuse a dirty working tree (`guard-clean`; untracked `.go` files are
compiled in but never show in `git diff`). `FORCE=1` overrides; or deploy
from a clean worktree:

```bash
git worktree add --detach /tmp/deploy <commit>
cp Makefile.local /tmp/deploy/          # host settings, gitignored
make -C /tmp/deploy deploy-all
git worktree remove --force /tmp/deploy
```

## Cut-over and rollback

The move from systemd to containers. **Done 2026-10-18**: the units stopped at
18:43:47, the containers answered at 18:44:27 — about 40 seconds down. Kept as
the record of how, and as the runbook for a rebuilt host. Each step is
reversible until the last.

**One-time setup**

1. GitHub → *Settings → Environments* → `production`, with yourself as a
   required reviewer.
2. A deploy key pair just for CI (`ssh-keygen -t ed25519 -N "" -C ci-deploy`).
   Repository secrets: `VPS_DEPLOY_KEY` (private key), `VPS_HOST`,
   `VPS_KNOWN_HOSTS` (`ssh-keyscan -t ed25519 <host>`).
3. On the VPS:
   - `/opt/trading/deploy.env` with `WEB_BIND=<tailscale-ip>:8080`
   - `make deploy-ops`
   - `/etc/sudoers.d/trading-deploy`: `ubuntu ALL=(root) NOPASSWD: /usr/local/bin/trading-deploy`
     (check with `sudo visudo -c`)
   - the `authorized_keys` line above, with the CI public key
   - remove cloud-init's `ubuntu ALL=(ALL) NOPASSWD:ALL`
     (`/etc/sudoers.d/90-cloud-init-users`) once `ubuntu` has a password you have
     tested (`su - ubuntu -c true`). Done here 2026-10-18: `sudo` asks for the
     password for everything except `trading-deploy`, so `ops restart` prompts.

**Cut-over**

4. Merge to `main`. CI builds and pushes both images, then waits for approval.
5. First push creates the ghcr.io packages as **private**: set both to public
   (package → *Package settings → Change visibility*), then confirm the VPS
   can pull without logging in: `sudo docker pull ghcr.io/henry190927/trading-bot-web:<sha>`.
6. Stop the units — downtime starts here:
   `sudo systemctl disable --now trading-web trading-monitor`
7. Approve the deploy. `trading-deploy` refuses to run while the units are
   active, so steps 6 and 7 cannot be swapped by accident.

**Rollback** — the binaries never left `/opt/trading`:

```bash
sudo docker compose --env-file /opt/trading/deploy.env -f /opt/trading/compose.yaml down
sudo systemctl enable --now trading-web trading-monitor
```

## Operating

Service control is an SSH job, through one script:

```bash
ssh trading-bot ops status                 # deployed commit, web /health, monitor heartbeat
ssh trading-bot ops logs monitor -f        # follow the logs (or: web; N lines: ops logs monitor 200)
ssh trading-bot ops restart monitor        # restart a container (asks for the sudo password)
ssh trading-bot ops zone 20                # recent zone-alert events
```

`ops` wrapped systemd first and wraps Docker Compose now; only its function
bodies changed. Logs go through journald (tag `trading-web` /
`trading-monitor`), so reading them needs no sudo.

The `/ops` page is the read-mostly side: service and heartbeat state, the zone
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

| Section | Route | Page |
|---|---|---|
| Today | `/` (also `/today`) | Open trades, today's R, macro gaps the blackout does not cover, pending setups, market table |
| | `/ops/scan` | Live setups — every auto rule (and every unarmed strategy × symbol cell) evaluated on the latest closed bar |
| Chart | `/chart` | The live-trade surface: candles, score /100, plan, factors, structure read, OI; `+` at a price to validate or record |
| | `/validate` | Score a proposed entry |
| Portfolio | `/journal`, `/journal/new`, `/journal/:id/edit`, `/journal/:id/close` | Trades and stats; the partials calculator for multi-leg exits |
| | `/setups` | Setup tracking with the path-dependent real outcome |
| | `/ops/autotrade` | Paper auto-trade log, caps replay |
| | `/ops/verify` | Every open trade against its actual BingX position and orders; attach a stop/TP |
| Research | `/calendar` | Macro calendar (curated events gate the engine; the live feed is display-only) |
| | `/scan` | One row per symbol on a chosen TF, expandable |
| | `/tips` | Lessons distilled from the journal |
| Settings | `/ops` | Push, channels, runtime settings — see [Operating](#operating) |
| | `/alerts` (and the 🔔 on every page) | In-app alerts — see [alerts.md](alerts.md) |

Every page renders in Chinese or English on the server; `/lang/zh` and
`/lang/en` set the `lang` cookie and return to the page.

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
| ntfy "Daemon DOWN / recovered" | `daemon-watch.sh` every 10 min | a service stopping |
| Daily "daemon heartbeat" push | `daemon-watch.sh` at 00:00 | the watcher itself dying — silence becomes trustworthy |
| `monitor-heartbeat.json` | the monitor, every 15s | a loop that is alive but stuck (`stalled`), a loop idled by config or mute |
| 🔔 in-app alerts | web, every 15s | monitor down, stalled loops (bracket = critical → also ntfy), restart loops, price feed down, bad macro overlay — see [alerts.md](alerts.md) |

`daemon-watch.sh` asks the services, not the platform: the monitor is up if
its heartbeat file changed in the last 60 seconds, the web if `/health`
answers. It did not change when the services moved into containers.

## Backups and recovery

- **Daily off-box backup** (`scripts/vps-data-backup.sh`, cron 04:15): an
  explicit allow-list of the files that cannot be rebuilt — `journal.csv`,
  `setups.jsonl`, `autotrade.json`, `autotrade_paper.jsonl`,
  `autotrade_blocked.jsonl`, `zones.json`, `zone-config.json`,
  `chart_state.json`, `macro-overlay.json`, `alerts.jsonl`, `zone-events.jsonl`,
`monitor-config.json` — is committed to a **private** GitHub
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
[architecture_hardening.md](architecture_hardening.md). With the services in
images it is now short:

1. Oracle Cloud → *Always Free* → Ubuntu 22.04 on `VM.Standard.E2.1.Micro`
   (x86_64; `A1.Flex` ARM would need `linux/arm64` added to the image build),
   with a public IPv4 and a generated SSH key.
2. `sudo timedatectl set-timezone Asia/Taipei`; a 2 GB swap file (1 GB of RAM
   has no headroom without one).
3. Install Tailscale and Docker (the official apt repository, not the
   `curl | sh` script); `ufw` allowing only 22 on the public interface. Do
   **not** add `ubuntu` to the `docker` group — it is root without a password.
4. Create `/opt/trading` owned by `ubuntu`, put `.env` there (`chmod 600`) and
   `deploy.env`; the crontab; the one-time setup in
   [Cut-over and rollback](#cut-over-and-rollback); `make deploy-all`.
5. Restore the data files from the private backup repo, then trigger a deploy
   of the current `main`.

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
