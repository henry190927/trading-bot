# CLI reference

The command-line tools in `cmd/`. Most are thin wrappers over the same packages
the web UI and the monitor import, so a CLI answer and a web answer come from
the same code. For the system overview see the [README](../README.md); for
running it on the VPS see [operations.md](operations.md).

`make analyze` / `make validate` / `make backtest` wrap the most common forms
with positional arguments (`make validate BTC long 74500 15m`); everything below
is the raw `go run` form, which exposes every flag.

> `cmd/serve`, the original scan-and-alert daemon, is retired: its unit is
> disabled on the VPS and the monitor (`cmd/monitor`) covers what it did. The
> source is kept; it is no longer deployed.

### `cmd/analyze` — one-shot snapshot

Fetches the latest closed candle of `-tf`, runs the engine, prints per-symbol
detail (side, score, N字 structure, HVN/POC, session opens, reasons, warnings,
plan) plus a summary table with the verdict. Use this when you want to know
"what's actionable right now?".

Defaults to `market.All()` — the four-symbol DAEMON universe (BTC/ETH/XAU/XAG).
`-symbols` addresses anything `market.Resolve` knows, including the stock
synthetics and the crypto alts that are deliberately kept out of the daemon
universe:

```bash
go run ./cmd/analyze -symbols BTC,ETH,SUI,SNDK -tf 1h
```

```bash
go run ./cmd/analyze                  # default 1h
go run ./cmd/analyze -tf=15m          # 15-minute snapshot
go run ./cmd/analyze -tf=4h           # 4-hour snapshot
go run ./cmd/analyze -tf=4h -min-score=4   # stricter threshold
go run ./cmd/analyze -bias            # opt into MTF bias filter
```

### `cmd/backtest` — historical replay

Pulls `-days` of history at `-tf`, replays the engine, simulates each setup with a fee model. Prints per-symbol stats (WR, net R, max drawdown). Use to validate the strategy before live trading.

```bash
go run ./cmd/backtest                              # default 1h, 60 days
go run ./cmd/backtest -tf=15m -days=30             # 15-min over 30 days
go run ./cmd/backtest -tf=1h -sweep-only           # sweep-only
go run ./cmd/backtest -tf=1h -fee-bps=10           # taker fee assumption
go run ./cmd/backtest -tf=1h -v                    # print every trade
```

### `cmd/validate` — score a proposed trade

Given a symbol, side, entry price, and timeframe, scores **your proposed entry** 0–10 against the engine's current state and HVN structure. Use when you're considering a discretionary entry and want a sanity check.

```bash
go run ./cmd/validate -symbol=BTC -side=long -entry=74500 -tf=1h
go run ./cmd/validate -symbol=XAG -side=short -entry=75.80 -tf=15m
go run ./cmd/validate -symbol=ETH -side=long -entry=2030 -tf=1h -fee-bps=10
```

Symbol aliases: `BTC` / `ETH` / `XAU` or `GOLD` / `XAG` or `SILVER`. Side: `long` / `short` (or `l` / `s`).

**Important:** the score evaluates *your entry*, not the engine's own plan. The engine's independent plan is shown alongside for comparison so you can see whether your entry beats or chases the engine's anchor:

```
Engine's own plan (compare with yours):
  side   SHORT  (anchor: sweep high @ 76753.5000)
  order  LIMIT
  entry  76753.5000   (your entry: 76896.0000 → diff +0.19%)
  stop   77302.8343   (risk 549.3343)
  TP1    76204.1657   (1R)
  TP2    75654.8313   (2R)
```

- Diff *with* your trade direction (e.g., higher entry for a short, lower entry for a long) = you got a better fill than the engine
- Diff *against* your direction = you're chasing — the engine had a better entry available

The block only appears when the engine has its own plan (Side ≠ FLAT). When the engine is flat, your trade is entirely your own thesis — score still computes off your entry and HVN context.

Scoring weights:

| Factor | Points |
|---|---|
| Direction aligned with engine | +2.0 |
| Direction opposes engine | −2.0 |
| Engine score ≥ 3 with same direction | +1.0 |
| Entry at sweep level, correct side | +2.0 |
| Entry at sweep, wrong side | −0.5 |
| Entry at fib 0.618 | +1.5 |
| Entry at BOLL band touch | +0.5 |
| Entry at non-POC HVN, direction-aligned | +1.5 |
| Entry at POC | −1.0 |
| Entry at HVN wrong side | −1.0 |
| POC reachable as TP (0.5–5% in trade direction) | +1.0 |
| POC far away (>5% in trade direction) | −0.5 |
| Fading away from chip zone (POC >2% in wrong direction) | −0.5 |
| Fee math healthy / acceptable / thin / fatal | +1.0 / +0.5 / 0 / −1.5 |

Verdicts: 8+ STRONG TAKE · 6–7.5 TAKE · 4–5.5 NEUTRAL · 2–3.5 WEAK · ≤1.5 AVOID.

Output sections (in order):
1. **Header / Engine state** — engine's independent direction + score
2. **Engine reasons** — what fired in the engine
3. **Engine's own plan** — for comparison with yours (when engine has a plan)
4. **Entry-level proximity** — distances from your entry to sweep / fib / BOLL levels
5. **HVN context** — POC and top-5 HVN distances from your entry
6. **Suggested levels** — stop and TP1/TP2 bracket computed from *your* entry ± 1.5×ATR
7. **Factors** — every scored factor with points and rationale
8. **Total score & Verdict**

### `cmd/journal` — record live trade outcomes

Tracks opened positions and their outcomes to a CSV log. Use to validate whether live results match backtest expectations over time. After ~20–30 closed trades, `journal stats` will tell you if your live avgR roughly matches the backtest avgR — if it does, the strategy is generalizing; if live << backtest, you have look-ahead bias or regime drift.

Stored at `$JOURNAL_PATH` (defaults to `./journal.csv`). On the VPS it is `/opt/trading/journal.csv`.

> **The web UI (`/journal`) is the primary way to record trades now.** The CLI
> below still works and is what the examples use, but the `make j*` wrappers are
> gone: run it on the VPS, where `/etc/profile.d/trading-aliases.sh` provides
> `jopen` / `jclose` / `jlist` / `jstats` / `jupdate` / `jdelete` / `janchors`.

#### Open / close a trade

Flags (`--score`, `--at`) must come before the positional args.

```bash
# Open — SYMBOL SIDE ENTRY STOP TP1 TP2 ANCHOR TF [notes]
jopen XAG short 77.49 78.16 76.81 76.14 sweep-high 1h "RSI div + chip resistance"

# Open with score + analysis time
jopen --score=3 --at="2026-05-26 21:00" XAG short 77.49 78.16 76.81 76.14 sweep-high 15m,1h "RSI div, multi-TF"
jopen --score=v7.5 BTC long 75000 74500 75500 76000 sweep-low 1h "validate said TAKE"
jopen --at=-3h ETH long 2120 2098 2142 2164 sweep-low 1h "analyzed 3h before placing"

# Close — ID|SYMBOL OUTCOME EXIT_PRICE [notes]
jclose XAG tp2 76.14 "hit TP2 in 4h"
jclose 7 stop 78.18
```

OUTCOME ∈ `{tp1, tp2, stop, manual, timeout}`. R-multiple is computed automatically at close.

Tip: `jopen` flags are recognized as `--name=value` or `--name value`, in any order, but must come **before** the positional args. `tf` accepts comma-separated values when you analyzed multiple timeframes (e.g. `15m,1h`).

#### View / aggregate

```bash
jlist                    # last 20 trades, color-coded
jlist 50                 # last 50

# Aggregate WR / avgR / totalR — overall + grouped by symbol, tf, anchor
jstats
```

Sample `jlist` output (timestamps in your local zone, full timeline + price ladder visible):

```
ID  ANALYZED          OPENED            CLOSED            SYM  SIDE  TF   SCORE  ENTRY     STOP      TP1       TP2       EXIT      R       STATUS  ANCHOR
2   2026-05-26 01:21  2026-05-26 04:30  2026-05-26 09:36  XAU  SHORT 15m  2      4577.36   4596.05   4558.72   4539.65   4539.65   +2.02   tp2     sweep-high
1   2026-05-24 17:15  2026-05-24 18:05  2026-05-24 22:15  XAG  SHORT 1h   3      77.48     78.16     76.81     76.14     76.13     +1.99   tp2     sweep-high
```

Three timestamp columns:
- **`ANALYZED`** — when you ran `analyze` / `validate` (decision time)
- **`OPENED`** — when the limit order filled
- **`CLOSED`** — when the position closed (empty `-` for open trades)

Four price columns make outcome-vs-plan easy to scan at a glance:
- `ENTRY` / `STOP` — the plan
- `TP1` / `TP2` — the targets you set
- `EXIT` — where the trade actually closed; matching TP2 confirms a 2R win

When you don't pass `--at` to `jopen`, `ANALYZED` matches `OPENED`. When they diverge (e.g. analyzed at 22:00 but limit filled at 04:30), the gap reveals patterns like "did I sit on too many ideas before pulling the trigger?".

#### Edit existing entries

```bash
jupdate 1 entry=77.50 stop=78.20
jupdate 1 open_notes="RSI div + funding crowd"
jupdate 1 analyzed_at=-3h
jupdate 1 close_notes="hit TP2 in 4h"

jdelete 7                # nuke entry 7 (cannot be undone)
```

Editable fields:

- **Times** (any time spec — `now`, `-2h`, RFC3339, `YYYY-MM-DD HH:MM`): `opened_at`, `analyzed_at`, `closed_at`
- **Strings:** `side`, `tf`, `anchor`, `open_notes`, `outcome`, `close_notes`
- **Numbers:** `entry`, `stop`, `tp1`, `tp2`, `exit_price`

`id` and `r_realized` are not directly editable (R is auto-recomputed whenever you edit `entry`/`stop`/`exit_price` on a closed trade).

Useful examples:

```bash
# You backfilled the entry with today's timestamp but actually opened yesterday
jupdate 1 opened_at=2026-05-24T15:30:00Z

# Same for the close (the trade closed at a specific UTC time you remember)
jupdate 1 closed_at=2026-05-24T19:45:00Z
```

#### CSV schema (30 columns — current v9)

Versioned by COLUMN COUNT and auto-migrated on read, so an old file keeps
working: v1=16, v2=17 (+analyzed_at), v3=18 (+score), v4=19 (+leverage),
v5=20 (+filled_at), v6=21 (+signal_ctx), v7=23 (+tp1 auto-placement state),
v8=29 (+margin_usdt and entry/stop/tp2 placement state), v9=30 (+equity_usdt).

```
id, opened_at, analyzed_at, closed_at, symbol, side, tf, score,
entry, stop, tp1, tp2,
anchor, open_notes,
exit_price, outcome, r_realized, close_notes,
leverage, filled_at, signal_ctx,
tp1_auto, tp1_order_id, margin_usdt,
entry_order_id, stop_auto, stop_order_id, tp2_auto, tp2_order_id,
equity_usdt
```

`equity_usdt` is the column that makes a row say anything about RISK rather
than about the read. R is leverage-independent by definition, so an R series
is structurally incapable of showing an account going to zero — on 2026-09-08
this file read 66 settled / gross +19.04R while the balance read 0.00000000,
and both were correct. Without equity-at-entry no row and no sum of rows can
say what fraction of the account was on the table.

Four more columns deserve a callout:

| Column | What it captures |
|---|---|
| `analyzed_at` | When you ran `analyze` or `validate` and made the decision. Can predate `opened_at` (e.g. you saw a setup at 8 pm but only opened at 10 pm). Default = same as `opened_at`. Editable via `--at <spec>` on `jopen` or `analyzed_at=<spec>` on `jupdate`. Spec accepts `now`, `-Nh/-Nm/-Nd`, RFC3339, or `YYYY-MM-DD HH:MM`. |
| `tf` | The timeframe(s) you were analyzing when you decided. **Accepts comma-separated values** like `15m,1h` if you saw the same setup on multiple frames. `jstats by tf` groups by exact string, so `15m,1h` and `1h,15m` would be two groups — pick an order and stick with it. |
| **`score`** | The confluence/validate score that drove the decision. **Free-form string with conventions:** `3` or `4` for engine confluence scores from `analyze`; `v7.0` or `v8.5` for `validate` 0–10 scores; `3,v7.0` if you saw it in both; `manual` for discretionary entries; empty if not recorded. Prefix self-identifies the source so `jstats by score` naturally groups by tool. |
| `anchor` | What you were keying entry to. Free-form text, but `jstats` groups by exact string — use canonical labels (run `janchors` for the list) so groupings stay clean. |

##### Timezone

All timestamps are stored in **Asia/Taipei (UTC+8)** with explicit `+08:00` offset in the CSV. Naive time inputs (no timezone marker) are interpreted as Taipei local time. Both space and `T` separators between date and time are accepted:

```bash
jopen --at="2026-05-26 21:00" ...        # 21:00 Taipei (space separator)
jopen --at=2026-05-26T21:00 ...          # 21:00 Taipei (T separator, no quotes needed)
jupdate 2 closed_at=2026-05-25T12:00     # 12:00 Taipei
jupdate 2 closed_at="2026-05-25 12:00"   # same thing
```

Explicit timezone inputs work too:

```bash
jopen --at=2026-05-26T13:00:00Z ...        # 13:00 UTC = 21:00 Taipei
jopen --at=2026-05-26T21:00:00+08:00 ...   # explicit Taipei offset
```

All accepted formats for `--at` and time fields in `jupdate`:

| Form | Example | Interpreted as |
|---|---|---|
| Relative | `now`, `-2h`, `-30m`, `-1d` | offset from current moment |
| Naive (space) | `2026-05-24 17:15`, `2026-05-24 17:15:00` | Taipei local |
| Naive (T) | `2026-05-24T17:15`, `2026-05-24T17:15:00` | Taipei local |
| Date only | `2026-05-24` | Taipei 00:00 |
| RFC3339 UTC | `2026-05-24T17:15:00Z` | UTC, preserved |
| RFC3339 offset | `2026-05-24T17:15:00+08:00` | Offset preserved |

##### Daemon timestamps in journalctl

For `ops logs` to show the services' log lines in the same timezone as systemd's own prefix, set both layers to Asia/Taipei:

```bash
# System-wide (once on the VPS — affects systemctl, journalctl prefixes, date, etc.)
sudo timedatectl set-timezone Asia/Taipei

# Each service — pass TZ to the binary via its systemd unit, under [Service]
# (trading-web.service and trading-monitor.service both carry it):
Environment=TZ=Asia/Taipei
```

Without the unit-level `Environment=TZ=...`, the Go binary falls back to UTC in its `log.Printf` output even though systemd's prefix is in Taipei — splits the same line into two timezones, 8 hours apart. The line above keeps everything coherent.

##### Schema migrations

The reader auto-migrates legacy CSVs on first read:

| Version | Columns | Notes |
|---|---|---|
| v1 | 16 | Pre-`analyzed_at`. Migration: `analyzed_at` = `opened_at`, `score` = `""` |
| v2 | 17 | Added `analyzed_at`. Migration: `score` = `""` |
| v3 | 18 | Added `score` after `tf`. |
| v4–v9 | 19–30 | See *CSV schema* above. v9 (30 columns) is current. |

Old files keep working — the first write after a read rewrites in the current form.

#### Anchor reference

Free text technically, but for `jstats` to group meaningfully use these canonical lowercase-hyphen labels:

```bash
janchors
```

Output (so you don't have to run it):

| Anchor | Meaning |
|---|---|
| `sweep-low` | Long after low-side liquidity grab + reclaim |
| `sweep-high` | Short after high-side grab + reclaim |
| `fib-uptrend` | Long at fib 0.618 in an uptrend |
| `fib-downtrend` | Short at fib 0.618 in a downtrend |
| `boll-lower` / `boll-upper` | Mean-reversion at the Bollinger band |
| `hvn-support` / `hvn-resistance` | Bounce/rejection at a non-POC HVN |
| `daily-open` / `weekly-open` | Reaction at the UTC open |
| `discretionary` / `manual` | Entry not tied to any strategy signal |

Mixed case, plurals, typos create separate groups in `jstats` — be consistent.

#### Win/loss math

`r_realized` is the single source of truth:

- Long: `R = (exit − entry) / (entry − stop)`
- Short: `R = (entry − exit) / (stop − entry)`

A win is `R > 0`, loss is `R < 0`. `jstats` computes WR as `count(R > 0) / total`. Position size and absolute PnL are intentionally outside the journal — R is the strategy-level metric, position sizing is your money-management overlay.

### All flags

| Flag | Default | Used by | Meaning |
|---|---|---|---|
| `-tf` | `1h` | all | Base timeframe: `1m`, `5m`, `15m`, `1h`, `4h`, `1d` |
| `-min-score` | `3` | all | Don't act below this confluence score |
| `-sweep-only` | off | backtest | Restrict to liquidity-sweep–anchored entries |
| `-bias` | off | all | Enable MTF bias filter (off because backtest shows it hurts mean-reversion edge) |
| `-bias-tf` | auto | all | Override the higher-TF used for bias (1h→4h, 5m→1h) |
| `-stop-refine` | off | backtest | Push stop past nearby HVN / equal-level clusters to dodge sweep flows (off — backtest shows R-shrinkage; opt in if you observe live edge) |
| `-fee-bps` | `6` | backtest | Round-trip fee in basis points (4 = both maker, 10 = both taker) |
| `-days` | `60` | backtest | History window |
| `-hold` | `24` | backtest | Max bars before timing out a position |
| `-v` | off | backtest | Print every individual trade |

---

## How to read the output

### `analyze` (snapshot)

```
=== ETH-USDT 1h @ 2122.4300 ===
Side: LONG   Score: 3   Bias: FLAT   Funding: 0.0100%   OI: 480M
HVN (籌碼密集區): POC=2129.4766   nodes=[2129.4766, 2123.4278, 2117.3791, 2135.5253, 2111.3303]
  + Liquidity grab at 2122.43 (low side)
  + RSI bullish divergence
  + Pullback to fib 0.618 in uptrend
  >> LIMIT LONG  entry=2122.4300  stop=2118.9100  risk=3.5200  anchor=sweep low @ 2122.4300
     TP1=2125.9500 (1.0R)
     TP2=2129.4700 (2.0R)
```

→ Place a **limit buy** at `2122.4300` on BingX. Set stop at `2118.9100`. Take 50% off at `TP1`, trail the rest to `TP2`. Don't market in — limit only.

**Line by line:**
- `Side` — engine's direction call (LONG/SHORT/FLAT).
- `Score` — confluence vote count. ≥ 3 with sweep anchor = tradeable.
- `Bias` — higher-timeframe MACD direction; only enforced when `-bias` flag is on.
- `Funding / OI` — perp context. Extreme funding warnings appear as `!` lines.
- `HVN (籌碼密集區)` — Volume Profile from last 200 bars. **POC** is the single price with the most accumulated traded volume; **nodes** are the top-5 high-volume zones (strong S/R). Use these for entry staging and stop placement — they do NOT vote on direction (see "Confluence factors" below for why).
- `+` lines (green) — confluence factors that **fired and voted** on the score.
- `·` lines (cyan) — **display-only observations** (range expansion bars, double top/bottom patterns). These are computed and surfaced for trader context but do **not** vote on the score. See "Display-only data" under *Confluence factors* below.
- `!` lines (yellow) — warnings (crowded funding, OI squeeze, etc.). These don't reduce the score, just flag risk.
- `>>` line — the executable plan.

### Summary table

At the bottom of `analyze` output:

```
SYMBOL              SIDE  SCR BIAS  ENTRY      STOP       TP1        TP2        VERDICT
---------------------------------------------------------------------------------------
BTC-USDT            FLAT  0   FLAT  -          -          -          -          skip — no direction
ETH-USDT            LONG  3   FLAT  2122.43    2118.91    2125.95    2129.47    TRADEABLE — sweep-anchored ✓
NCCOGOLD2USD-USDT   SHORT 2   FLAT  4538.72    4544.16    4533.27    4527.83    borderline — watch for one more confluence
NCCOXAG2USD-USDT    LONG  3   FLAT  76.12      75.79      76.44      76.77      TRADEABLE — sweep-anchored ✓
```

Colors in your terminal: LONG/score≥threshold are bold green, SHORT is bold red, FLAT/skip is dim, TRADEABLE verdict is bold green. Set `NO_COLOR=1` to disable.

Verdict rules:
- `TRADEABLE — sweep-anchored ✓` — score ≥ threshold AND entry is anchored to a liquidity sweep. Highest-edge setup.
- `tradeable but anchor isn't a sweep — size down` — score ≥ threshold but entry uses fib or BOLL anchor. Acceptable but historically lower win rate.
- `borderline — watch` — one shy of threshold. Keep an eye on the next bar.
- `skip` — below threshold or flat.

### `backtest` summary

```
ETH-USDT 1h [sweep-only, fee=6.0bp] | n=27 sig, 27 filled | WR=53.8% | grossR=+7.84 netR=+6.50 avgNet=+0.25 maxDD=3.16R
```

| Field | Meaning |
|---|---|
| `n=27 sig, 27 filled` | 27 score≥threshold setups, all got their limit entry filled |
| `WR=53.8%` | win rate |
| `grossR` | sum of R-multiples before fees |
| `netR` | after fees — **this is the real number** |
| `avgNet` | average R per trade after fees; > 0 = real edge |
| `maxDD` | worst peak-to-trough drawdown in R units (sizing 1% per trade → expect ~maxDD% account drawdown) |
