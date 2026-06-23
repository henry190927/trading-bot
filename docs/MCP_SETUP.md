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

## Install (Mac)

### 1. Build the binary

```bash
cd /Users/henry.yeh/GolandProjects/myFirstGo/trading-bot
make mcp-install
```

This builds `cmd/mcp` and installs to `~/bin/trading-bot-mcp`. Make sure
`~/bin` is in your `PATH` (it usually is on Mac; check with `echo $PATH`).

### 2. Sync journal.csv from VPS

The MCP server reads `journal.csv` from a local path. Keep it fresh
with a periodic scp from VPS:

```bash
# One-off pull:
scp -i ~/.ssh/oracle-trading.key \
    ubuntu@your.vps.ip:/opt/trading/journal.csv \
    ~/trading-bot-data/journal.csv

# Or add to crontab (every 5 minutes):
*/5 * * * * scp -i ~/.ssh/oracle-trading.key ubuntu@your.vps.ip:/opt/trading/journal.csv ~/trading-bot-data/journal.csv >/dev/null 2>&1
```

> Live BingX position / candle / mark price calls don't need a local
> sync — those go directly to BingX via the API.

### 3. Register with Claude Code

Add to `~/.claude/config.json` (or wherever your Claude Code config
lives — `claude config path` will tell you):

```json
{
  "mcpServers": {
    "trading-bot": {
      "command": "/Users/henry.yeh/bin/trading-bot-mcp",
      "env": {
        "JOURNAL_PATH": "/Users/henry.yeh/trading-bot-data/journal.csv",
        "BINGX_API_KEY":    "<paste your read-permission key>",
        "BINGX_API_SECRET": "<paste your read-permission secret>"
      }
    }
  }
}
```

> **Security note**: the env block in this config file is plaintext.
> Use a **read-only** BingX key here (no trade scope, no withdraw) so
> a compromise can't move funds. The trade-permission key stays on the
> VPS where execution lives.

> If `BINGX_API_KEY` is empty, tools that need live data
> (`get_open_positions`, parts of `get_trade`) will degrade gracefully —
> they return what they can from journal.csv and skip the live calls.

### 4. Verify

Restart Claude Code (`claude` in a fresh terminal). At the prompt:

```
> list the trading-bot tools
```

Claude Code should respond with the 6 tools above. Then try:

```
> get_backtest_facts
> get_open_positions
> analyze trade #25 using the get_trade tool + my journal context
```

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
