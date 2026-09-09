# trading-bot MCP server — setup

The MCP server is the **zero-cost** path to AI analysis of your trading
data: instead of paying Anthropic API tokens through `trading-web`'s 🤖
Analyze button, you use Claude Code (already authenticated to your
account via OAuth) and let it call this server's tools to fetch trade
context.

See [`AI_ARCHITECTURE.md`](AI_ARCHITECTURE.md) for the full picture of
how this fits with the web-button path.

---

## What it does

When you launch `claude` in your terminal and ask something like:

> *"analyze trade #25 as a senior quant"*

Claude Code automatically invokes these tools to gather context:

| Tool | What it returns |
|---|---|
| `get_trade(id)` | Single trade row: plan, journal notes, BingX order IDs, signal context |
| `get_journal_history(symbol, status, limit)` | Recent journal trades, filterable by symbol + open/closed |
| `get_open_positions()` | Live BingX positions across all 4 symbols |
| `get_recent_candles(symbol, tf, n)` | Pre-digested OHLCV summary (peak/trough/path, last 5 bars) |
| `get_macro_events_near(timestamp)` | CPI/FOMC/NFP/PPI events ±24h of a reference time |
| `get_backtest_facts()` | Canonical 2026-06-02 backtest aggregate table + headline facts |

All output is markdown — LLM-friendly, not raw JSON dumps.

---

## Install (Mac) — step by step

### 1. Get a read-only BingX key

BingX → API Management → Create API Key. **Only check "Read" /
"Perpetual Futures Read"**. Do NOT check Trade or Withdraw. The
trade-permission key stays on the VPS where execution lives; this
local MCP only needs to *see* positions and candles.

(If you reuse the VPS's trade-permission key here, a compromised
laptop config could route real orders. Don't.)

### 2. Build + install the MCP binary

```bash
cd /Users/henry.yeh/GolandProjects/myFirstGo/trading-bot
make mcp-install
```

Installs to `~/bin/trading-bot-mcp`. Verify:

```bash
which trading-bot-mcp
# /Users/henry.yeh/bin/trading-bot-mcp
```

### 3. Sync journal.csv from VPS to local path

```bash
make mcp-sync-journal
# → ~/trading-bot-data/journal.csv
```

Auto-refresh every 5 minutes via cron:

```bash
crontab -e
# Add the line below:
*/5 * * * * scp -i ~/.ssh/oracle-trading.key ubuntu@<your-vps-ip>:/opt/trading/journal.csv ~/trading-bot-data/journal.csv >/dev/null 2>&1
```

(Live BingX position / candle / mark price calls don't need this
sync — they go directly to BingX via the API.)

### 4. Register with Claude Code

**Option A — claude CLI (recommended, no JSON typos)**:

```bash
claude mcp add trading-bot ~/bin/trading-bot-mcp \
  --env JOURNAL_PATH=$HOME/trading-bot-data/journal.csv \
  --env BINGX_API_KEY=<paste read-only key> \
  --env BINGX_API_SECRET=<paste read-only secret>
```

**Option B — edit `~/.claude.json` manually**, add a top-level
`mcpServers` section:

```json
"mcpServers": {
  "trading-bot": {
    "command": "/Users/henry.yeh/bin/trading-bot-mcp",
    "env": {
      "JOURNAL_PATH":     "/Users/henry.yeh/trading-bot-data/journal.csv",
      "BINGX_API_KEY":    "<paste read-only key>",
      "BINGX_API_SECRET": "<paste read-only secret>"
    }
  }
}
```

⚠ The env block is plaintext on disk. Read-only keys only.

### 5. Restart Claude Code

Existing sessions don't pick up new MCP servers. Open a fresh
terminal and run `claude`. The new session will spawn
`trading-bot-mcp` as a subprocess on first MCP call.

### 6. Verify

```
> list the trading-bot tools
```

Should respond with 6 tools (`get_trade`, `get_open_positions`,
`get_recent_candles`, `get_journal_history`, `get_macro_events_near`,
`get_backtest_facts`). Smoke test:

```
> get_backtest_facts
> get_open_positions
> get_recent_candles BTC 1h 30
```

---

## Sample analysis prompts (Quant terminology)

Once registered, drop these into a fresh `claude` session as
starting points. The MCP server's `serverInstructions` already
establishes Quant-Trader persona + the user's discipline rules,
so prompts can stay terse and assume the framing.

### Per-trade analysis (open or closed)

> Analyze trade #25 via get_trade. Quant framing: status snapshot,
> setup quality vs backtest facts (use get_backtest_facts), path
> observation, regime context, verdict with confidence.

> Trade #25 health check: get_trade + get_open_positions to
> confirm BingX state matches journal. Identify execution leaks if
> any. End with journal one-liner.

> Compare trade #25 to last 5 same-symbol entries
> (get_journal_history symbol=XAU limit=5). Is this setup in
> distribution or a tail outlier? Quantify R dispersion, hit-rate
> delta, hold-time delta.

### Pre-trade review (before pulling the trigger)

> Pre-trade review. Proposed: ETH SHORT @ 1680, stop 1695, TP1
> 1670, TP2 1660, score 3, ratio 7.5/10. Use get_backtest_facts
> to anchor symbol/TF edge. get_macro_events_near now() to flag
> event windows. get_open_positions for concurrent exposure.
> get_recent_candles ETH 1h 50 for structure. Output: GO / WAIT /
> DON'T with reasoning. Quantify expectancy where possible.

> Quick sanity check on [SYMBOL] [SIDE] @ [PRICE]: cross-check
> backtest fact table, current LH-LL or HH-HL structure, and any
> macro window within 4h. One-paragraph verdict.

### Portfolio / regime assessment

> Pull get_open_positions and give me a portfolio-level risk
> read: notional exposure, side concentration, max R if all SL
> hit vs max R if all TP hit. Note correlation if multiple
> positions are on the same side of risk-on/risk-off.

> Cross-symbol regime classification: pull get_recent_candles
> for BTC 1h, ETH 1h, XAU 1h, XAG 1h (50 bars each). Identify
> risk-on vs safe-haven flow, alignment vs dispersion, and which
> of the 4 is currently structurally strongest / weakest.

> Anchor-strength scan: for each of the 4 symbols, fetch 50 1h
> bars and report whether structure is HH-HL trending,
> LH-LL trending, or ranging. Flag symbols where mean-reversion
> would be counter-trend (high risk).

### Post-mortem / journal analysis

> Pull last 10 closed trades via get_journal_history limit=10.
> Identify the pattern in the losers: common setup
> characteristics, common exit reasons, recurring discipline
> leaks. End with one structural takeaway.

> Symbol-level expectancy review: get_journal_history per symbol
> (BTC, ETH, XAU, XAG, limit=20 each). Hit rate, avg R, R
> dispersion (std), time-to-resolution. Which symbol is earning
> its place in the universe?

> Compare last 5 TP1-hit trades vs last 5 stop-hit trades:
> setup features, score/ratio at entry, TF, anchor type. What
> separates winners from losers in this strategy's current
> regime?

### Macro-event aware

> get_macro_events_near 2026-07-15T12:30:00Z then get_recent_candles
> XAU 1h 50. Build a risk plan: which hours to avoid trading, what
> to do with any open XAU position in the blackout window.

> Pull next 30 days of macro events via get_macro_events_near with
> rolling timestamps. Build a calendar of windows when I should
> not enter new positions.

### Backtest-anchored decisions

> Use get_backtest_facts. Given the current dashboard shows XAU
> 1h setup at score 3 / ratio 7.0, should I take it? Reference
> the symbol's historical aggregate net R per window. Quantify
> "below threshold" vs "above threshold" decision rule.

> Cross-check the XAG 2h discretionary edge claim with backtest
> facts. If XAG 2h delivered +33R aggregate, what is the average
> per-trade expectancy and how many trades did that come from?
> Is the sample size sufficient to claim robust edge?

### Multi-symbol comparison

> Pull get_recent_candles for BTC 1h, ETH 1h, XAG 2h, XAU 1h, 50
> each. Rank them by current "setup quality" using these factors:
> distance from BOLL bands, RSI extremity, volume profile. Output
> a ranked table.

### Execution / order management

> Trade #26 hit stop earlier today. Pull get_trade(26) and
> get_recent_candles ETH 30m 60. Was this a path-noise stop
> (price reverted past TP within 6 bars) or a thesis-failure
> stop (price kept going against)? Apply the
> stopped-then-reversed framework from feedback-execution
> memory rule.

---

## Notes on prompt style

The MCP server's `serverInstructions` is short on purpose — it
sets persona + discipline rules and points Claude at the tools.
The verbosity of the prompt determines how thoroughly Claude
mines the tools.

- **Lazy / short prompts** ("analyze #25") → Claude usually pulls
  get_trade + maybe one other tool. Cheap, fast, mediocre depth.
- **Structured prompts** (specifying which tools and which
  framing) → Claude does the full pull and synthesizes. Higher
  token cost but proper Quant-level output.
- **Multi-step iteration** ("first pull X, then…", or
  ask follow-ups) → leverages Claude Code's session memory.
  This is where the MCP path beats the API-button path on
  depth.

When in doubt, **explicitly name the tools** in your prompt —
Claude prioritizes tool calls when you instruct it to.

---

## Server instructions are baked in

The MCP server sends a `serverInstructions` payload to Claude Code on
initialize. This instructs the LLM to act as a senior Quant Trader
when using these tools — same persona as the web-button path, same
discipline rules (no second-guessing, ship-gate backtest, closed-bar
engine, execution-not-stop-width). You don't need to re-state the
persona in every prompt; Claude Code picks it up automatically.

---

## Cost & ownership

- **Inference** runs on your Claude Code session = your existing
  Anthropic account (work SSO or personal — whichever you signed in
  with). **No incremental billing**.
- The MCP server itself is **local-only**, no network listener (it's a
  stdio subprocess of Claude Code). No exposed surface.
- The trading-web `/ai/analyze` button is **independent** — it still
  exists for mobile / quick analysis and uses an API key. You can use
  either, both, or neither.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `claude` doesn't list trading-bot tools | Config file path wrong, or `trading-bot-mcp` not in PATH. Run `which trading-bot-mcp`. |
| `read journal: no such file` | `JOURNAL_PATH` env var points to a non-existent file. Check the scp sync ran. |
| `get_open_positions` returns "BINGX_API_KEY not configured" | Env var empty in the MCP config block. Paste a read-only key. |
| Stale data | journal.csv hasn't sync'd lately; crontab maybe paused. Manual `scp` to force refresh. |
| Schema validation errors from Claude Code | mcp-go SDK version mismatch with Claude Code's MCP protocol version. Update `mcp-go` in go.mod. |

## What about HTTP transport instead of stdio?

Future option: run the server on the VPS as part of trading-web,
expose MCP via HTTP/SSE, and have Claude Code connect over Tailscale.
Removes the journal.csv sync requirement. Not implemented yet —
stdio is simpler for first iteration. See ROADMAP.
