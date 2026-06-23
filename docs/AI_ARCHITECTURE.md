# AI Advisor Architecture

The trading-web AI advisor layer wraps the user's existing
backtested rule engine + journal + BingX integration with a
LLM-driven "Quant Trader" co-pilot. This document is the canonical
reference for how requests flow, what each agent's role is, and
how the architecture is intended to evolve.

Last updated: **2026-06-23**

---

## Design principles (non-negotiable)

1. **Every LLM call — primary or sub-agent — runs as a senior Quant
   Trader / Researcher / Analyzer.** The Quant persona is baked into
   the canonical system prompt
   ([`ai/prompts.go`](../ai/prompts.go) → `SystemPromptQuantAdvisor`),
   which includes the user's accumulated discipline rules verbatim
   (4-symbol scope, mean-reversion strategy framing, closed-bar engine
   philosophy, no-second-guessing on closed trades, ship gate, backtest
   facts). No agent in the system gets a generic "helpful assistant"
   prompt — even cheap sub-agents inherit the Quant voice.

2. **Per-user API key from day 1.** No key is hardcoded. The Client
   carries no key state; every `Send` call takes an explicit `apiKey`.
   Phase 1 reads from env; Phase 2+ adds a UI settings layer. Multi-tenant
   ready without refactoring the agent layer.

3. **Advisor, not autonomous trader.** No path takes a trade action
   without explicit user confirmation. AI surfaces analysis; user retains
   all decisions. This applies in perpetuity — auto-execution from LLM
   output is out of scope.

4. **Cost discipline.** Pre-digest data in Go (or via cheap sub-agents
   in Phase 2+) so the expensive primary agent sees compact, structured
   input. Per-trade response caching prevents re-billing on UI refresh.

---

## Current state (Phase 1 — shipped 2026-06-23)

**One primary LLM agent + Go-side context pre-processing.** True
LLM sub-agents are deferred to Phase 2 once Phase 1 use validates the
output quality.

### Flow: per-trade `Analyze` button

```
   User clicks 🤖 Analyze on a trade card in /journal
              │
              ▼
   ┌────────────────────────────────────────────────────┐
   │  cmd/web: handleAIAnalyzeTrade                     │
   │  - load trade by ID (journal.csv)                  │
   │  - per-trade in-memory cache check (skip if hit)   │
   └─────────────┬──────────────────────────────────────┘
                 │ (cache miss → gather context)
   ┌─────────────┼──────────────────────────────────────┐
   │             ▼                                       │
   │   Go pre-processing layer (Phase-1 stand-in        │
   │   for future LLM sub-agents)                       │
   │                                                     │
   │   ┌──────────────────┐  ┌────────────────────┐    │
   │   │ Position fetcher │  │ Candle digester    │    │
   │   │ (BingX REST)     │  │ 50 closed bars →   │    │
   │   │ + mark price     │  │ peak/trough/path   │    │
   │   └──────────────────┘  └────────────────────┘    │
   │                                                     │
   │   ┌──────────────────┐  ┌────────────────────┐    │
   │   │ Journal          │  │ Macro event        │    │
   │   │ historian        │  │ matcher (±24h of   │    │
   │   │ (last 5 closed   │  │ opened_at, from    │    │
   │   │ same-symbol)     │  │ macro/events.json) │    │
   │   └──────────────────┘  └────────────────────┘    │
   │                                                     │
   │             │ pre-digested context                  │
   │             ▼                                       │
   │   ai.BuildTradeAnalysisMessage()                    │
   │   → structured markdown user message                │
   └─────────────┬───────────────────────────────────────┘
                 │
                 ▼
   ┌────────────────────────────────────────────────────┐
   │  PRIMARY AGENT (only LLM call in Phase 1):         │
   │                                                     │
   │  Model:       claude-sonnet-4-6                    │
   │  Temperature: 0.3 (analytical determinism)         │
   │  System:      SystemPromptQuantAdvisor             │
   │               • Quant Trader persona               │
   │               • discipline rules (verbatim from    │
   │                 memory files)                       │
   │               • backtest fact table                │
   │               • output structure guidance          │
   │  User msg:    pre-digested context above           │
   │                                                     │
   │  → Quant analysis output (markdown)                │
   └─────────────┬──────────────────────────────────────┘
                 │
                 ▼
   Cache result (text + token counts + cost) by trade ID
                 │
                 ▼
   JSON to UI → markdown renderer in collapsible pane
```

### Why "Go pre-processing" instead of LLM sub-agents in Phase 1

For a one-off per-trade analysis, mechanical data extraction
(BingX position read, candle peak/trough computation, journal
filtering, macro window matching) is cheaper and more deterministic
in code than as an LLM sub-agent. Phase 1 prioritises ship velocity +
predictable behaviour over architectural purity. Phase 2 will swap
specific pre-processors out for true sub-agents where the value
shows up (e.g. summarising 30 raw trades into a regime narrative).

---

## Phase 2 — true multi-agent (planned, not shipped)

The Go pre-processors become real LLM calls, each running with the
Quant persona system prompt. Cheap models (Haiku, Gemini Flash,
GPT-4o-mini — pluggable via the ai.Client) handle the mechanical
digesting; the primary Sonnet model only sees the digested outputs.

```
   ┌─────────────────────────────────────────────────────────────┐
   │                  PRIMARY SYNTHESIZER                        │
   │                  (Claude Sonnet, Quant persona)             │
   │                                                              │
   │            ▲              ▲              ▲                  │
   │            │ digested     │ digested     │ digested         │
   │            │ output       │ output       │ output           │
   └────────────┼──────────────┼──────────────┼─────────────────┘
                │              │              │
   ┌────────────┴──┐  ┌────────┴────────┐  ┌─┴───────────────┐
   │  INDICATOR    │  │  REGIME         │  │  JOURNAL        │
   │  EXTRACTOR    │  │  CLASSIFIER     │  │  HISTORIAN      │
   │  sub-agent    │  │  sub-agent      │  │  sub-agent      │
   │               │  │                 │  │                 │
   │  Quant persona│  │  Quant persona  │  │  Quant persona  │
   │  cheap model  │  │  cheap model    │  │  cheap model    │
   │               │  │                 │  │                 │
   │  Reads: 50    │  │  Reads: SPX     │  │  Reads: last 30 │
   │  raw bars +   │  │  + DXY + gold   │  │  closed trades  │
   │  RSI/MACD/    │  │  recent 5d      │  │  on symbol      │
   │  BOLL state   │  │                 │  │                 │
   │               │  │  Outputs:       │  │  Outputs:       │
   │  Outputs: 3-  │  │  "risk-on /     │  │  "last 5 BTC    │
   │  line summary │  │  metals weak"   │  │  trades -3.8R   │
   │  of momentum  │  │  + 1-line       │  │  aggregate;     │
   │  + key levels │  │  rationale      │  │  3 stop-outs"   │
   └───────────────┘  └─────────────────┘  └─────────────────┘

   Optional fourth sub-agent for the +record / pre-trade review path:

   ┌─────────────────────────────────────────────────────────────┐
   │  BACKTEST CROSS-CHECKER (sub-agent, Quant persona)          │
   │  Reads: proposed (symbol, TF, side, score) tuple            │
   │  Reads: backtest fact table baked into system prompt        │
   │  Outputs: "this combo's backtest aggregate is -X.X R over   │
   │           60/90/120d windows — below daemon MIN_SCORE if    │
   │           score < 3" or "this combo is the standout edge"   │
   │  Used: BEFORE the user submits a new trade plan             │
   └─────────────────────────────────────────────────────────────┘
```

Token economics: per primary analysis drops from ~3K input / ~800
output to ~600 input (just digested summaries) / ~800 output. Cost
per analysis falls from ~$0.025 to ~$0.008, even after counting
the cheap sub-agent calls. 3-5× cost reduction at scale.

---

## Phase 3 — per-trade chat threads (planned, not shipped)

Long-lived chat threads attached to specific trades. Persists to
`chat_history.json` so the user can revisit a conversation about a
trade days later. Each thread retains the same Quant persona + the
trade's full context as system context.

Phase 3 also adds:

- **Streaming responses** (SSE) so long analyses don't appear as a
  delayed wall of text.
- **Per-user API key settings UI** (env still works as default, but
  per-user override stored encrypted; replaces single-tenant Phase 1).
- **Daily token budget cap** (`ANTHROPIC_DAILY_TOKEN_BUDGET=100000`)
  enforced before each call; surfaces "approaching cap" UI banner.

---

## Pre-trade review (Phase 1.5 — design only)

A `+record` button extension. Same architecture as the per-trade
analyzer but triggered BEFORE the trade row is written:

```
   /journal/new form → "Get AI review" button
              │
              ▼
   Same context packager (trade plan is hypothetical;
   no position / no journal close data yet)
              │
              ▼
   Primary agent (Quant persona) returns one of:

     GO       — setup aligns with rules, no red flags
     WAIT     — soft warning (score < 3, downtrend concern,
                 macro release within 4h, etc.)
     DON'T    — hard rule violation (XAU + 15m, would
                 violate ship gate threshold, etc.)

   + reasoning, surfaced beside the submit button.
```

Value proposition: catches the `#26 / #27` class of mistakes
(LONG into confirmed downtrend, score 2 below MIN_SCORE 3) at the
decision point, not after the loss is realized.

Status: design only as of 2026-06-23 — ship after Phase 1 validates
the analysis output quality on real trades.

---

## MCP server path (shipped 2026-06-23)

The trading-web `/ai/analyze` button is the **API-key inference path**.
The MCP server (`cmd/mcp/`) is the **OAuth inference path** — same
context packaging, same Quant persona instructions, but inference runs
inside the user's Claude Code session (no API key, no per-call billing).

Setup, env vars, and the Claude Code config snippet are in
[`MCP_SETUP.md`](MCP_SETUP.md).

The two paths share `ai/context.go` for context packaging and
`SystemPromptQuantAdvisor` for the persona contract — update one, both
benefit. Pick path based on workflow:

| Where you are | Use |
|---|---|
| Terminal / dev machine, deep analysis | MCP (free, full Claude Code iteration) |
| Mobile / quick verdict | Web button (API key, $1-5/mo) |

## File layout

| Path | Purpose |
|---|---|
| `ai/client.go` | Anthropic Messages API client. `Client.Send(SendOptions)`. DRY-RUN mode, token tracking, EstimatedCostUSD. |
| `ai/prompts.go` | `SystemPromptQuantAdvisor` constant. Sole source of truth for the persona + memory rules. Update here, all agents pick it up. |
| `ai/context.go` | `BuildTradeAnalysisMessage(TradeAnalysisInputs)`. Go-side pre-digestion of candles / journal / position / macro. |
| `cmd/web/handlers.go` | `handleAIAnalyzeTrade` endpoint + in-memory cache on `server`. |
| `cmd/web/templates/journal_list.html` | 🤖 Analyze button + collapsible pane + minimal markdown renderer. |
| `cmd/web/static/style.css` | `.btn-analyze`, `.ai-analysis-pane`, `.ai-md-table` styling. |
| `cmd/mcp/main.go` | MCP server binary. Same Quant persona via `serverInstructions`. Reuses `ai/context.go` + `journal/` + `bingx/` + `macro/`. |
| `docs/MCP_SETUP.md` | Install + Claude Code config + journal.csv sync workflow. |

## Env vars

| Var | Effect |
|---|---|
| `ANTHROPIC_API_KEY` | Default key for all calls. Phase 2+ adds per-user override on top. |
| `ANTHROPIC_DRY_RUN=true` | All LLM calls log + return a stub response. No billing. |
| `ANTHROPIC_DAILY_TOKEN_BUDGET` | (planned Phase 3) hard cap before request fires. |

## Cost summary (Phase 1, Sonnet 4.6)

- Per analysis: ~2-3K input tokens × $3/MTok + ~600-800 output × $15/MTok = **~$0.02-0.03**
- 50 analyses/month → ~$1.50/mo
- 200 analyses/month (heavy use) → ~$5/mo

Phase 2 multi-agent reduces these by 3-5× via sub-agent digesting.
