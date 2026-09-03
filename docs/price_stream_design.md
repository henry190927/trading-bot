# Live price stream — design

Two things live here:

1. **What shipped** (2026-09-03): server-owned fan-out over **SSE**. Section 1–4.
2. **The WebSocket design** that was deliberately *not* built, written out as an
   SD exercise. Section 5–9. It is a design record, not a TODO.

---

## 1. The problem

The home market table and the chart's ticker strip both showed a price. Each
open tab ran its own `setInterval` + `fetch('/api/tickers')`.

Consequences:

- **Latency floor = the poll interval.** At 10s the table looked static; the
  tick-flash effect almost never fired. Dropping to 5s helped the feel and
  multiplied upstream calls by 2.
- **Work repeated per client.** 11 symbols × 2 REST calls per refresh. A 3s
  server-side cache kept upstream flat-ish, but each browser still paid the
  round trip and each did its own JSON parse.
- **Polling forever.** Whichever tab was open kept calling the exchange, even
  with nobody looking.

## 2. Requirements, stated before choosing a transport

The transport choice falls out of these, so they come first.

| | |
|---|---|
| Direction | **Server → client only.** The client never sends anything on this channel. |
| Value semantics | **Last-value-wins.** A superseded price carries no information. |
| Loss tolerance | **High.** Missing an intermediate tick is invisible; missing the *latest* is not. |
| Ordering | Must not show an older price after a newer one. |
| Fan-out | Unbounded readers, one upstream. |
| Freshness target | ~3s, matching the exchange REST cache. |
| Auth | None — public market data, no per-user state. |

## 3. Decision: single upstream + SSE

**Ownership (design B).** One server-side goroutine refreshes; browsers
subscribe. Upstream cost is independent of audience size. The alternative —
each browser holding its own exchange connection — would put credentials and
rate-limit exposure in the client and make N tabs into N upstream connections.

**Transport: SSE, not WebSocket.** Given the table above, WS's bidirectionality
is dead weight. SSE is plain HTTP/1.1 (`text/event-stream`), the browser
reconnects by itself, and there is no upgrade handshake to get wrong. See
section 5 for when this flips.

**Backpressure: bounded 1-slot channel, drop-OLDEST.** A backgrounded or
network-stalled tab must not stall the hub or its peers. Each subscriber gets a
one-deep buffer; when full, the *pending* snapshot is discarded and replaced by
the newer one.

> Drop-oldest is licensed by the value semantics, not by convenience. On a
> price feed, the stale pending value is worthless — so discarding it is free
> and the slow client resyncs to the truth on its next read. The same policy on
> an order feed, a trade feed, or a fill notification would be a **bug**: those
> are event-semantics, where every message is a distinct fact. Name the
> semantics before picking the drop policy.

**Marshal once per tick.** The payload is identical for everyone, so it is
serialised once and the same `[]byte` is handed to every subscriber. This was
the only per-client cost that would otherwise scale with audience.

**Lazy lifecycle.** The poller starts on the first subscriber and stops with
the last, so an idle deployment makes zero exchange calls.

**Replay-on-connect.** A new subscriber is handed the most recent snapshot
immediately, so a fresh tab paints without waiting an interval.

## 4. Shipped shape

```
BingX REST ──► buildTickers() ──► tickerHub.refresh()
  (one caller)                        │ marshal once
                                      ▼
                              broadcast() ──► chan[1] ──► SSE ──► tab
                                          ──► chan[1] ──► SSE ──► tab
                                          ──► chan[1] ──► SSE ──► tab
```

- `cmd/web/stream.go` — `tickerHub`, `handleStreamTickers`
- `cmd/web/chart_bias.go` — `buildTickers` (extracted; `/api/tickers` and the
  hub share it, so the two transports cannot serve different numbers)
- `cmd/web/templates/today.html` — `EventSource`, polling fallback
- `GET /api/stream/tickers`

**The source is injected** (`func(context.Context) any`) rather than the hub
reaching into `*server`. That makes the hub testable without an exchange, and
it is the exact seam an exchange websocket would replace: `pollLoop` is the
only thing that changes, and subscribers/backpressure/transport are untouched.

Client behaviour worth noting:

- REST paints first, then SSE takes over — no blank table while connecting.
- The poller is torn down on the **first received event**, not on `onopen`. A
  proxy that buffers `text/event-stream` into silence would otherwise strand
  the table with an open-but-mute connection.
- `onerror` is not a failure path — EventSource fires it on every transient
  drop and reconnects itself. It re-enables polling *while disconnected* only.
- Heartbeat comment (`: ping`) every 20s so intermediaries don't reap a quiet
  stream. `X-Accel-Buffering: no` defeats nginx-style response buffering.

**Measured after deploy** (3 concurrent clients): identical values delivered at
identical instants, ~3s cadence, one `hub started` / `hub stopped` pair per
session.

---

## 5. WebSocket: when it would be the right call

SSE wins here *because* of section 2. Flip any of these and the answer changes:

| Change | Why WS becomes correct |
|---|---|
| Client needs to send (subscribe to a symbol subset, place an order) | SSE has no upstream channel; you'd bolt on a second POST endpoint and now have two things to keep in sync |
| Binary payloads | SSE is UTF-8 text framing only; binary needs base64, ~33% overhead |
| Many streams per client | HTTP/1.1 caps ~6 connections per origin, and each SSE stream is one. HTTP/2 multiplexes and removes this, but only if you control the whole path |
| Sub-second, high-frequency ticks | SSE's per-event framing overhead and text encoding start to matter |
| Server needs to know liveness precisely | WS has protocol-level ping/pong; SSE liveness is inferred from write errors |

For **this** system, none apply today. Placing orders from the browser would be
the realistic trigger — and that is a channel with completely different
requirements (auth, exactly-once, event-semantics), so it would likely be its
own connection rather than a reason to convert this one.

## 6. What the WS version would have to solve that SSE gave for free

This is the part worth rehearsing — SSE hides four problems.

**6.1 Reconnect with backfill.** EventSource reconnects automatically and sends
`Last-Event-ID`. WS gives you nothing: you write the reconnect loop, the
exponential backoff, the jitter (so N tabs don't stampede a recovering server),
and a cap. And you decide what happens to the gap.

For last-value-wins data the answer is easy — **don't backfill, just resend
current state on connect**. State that explicitly, because on an event-semantics
stream it would be wrong, and the reviewer is checking whether you know which
one you have.

**6.2 Keepalive and half-open detection.** A TCP connection can be dead while
both ends believe it is open (NAT timeout, sleeping laptop). WS has ping/pong
frames: server pings every ~30s, expects a pong within ~10s, otherwise closes.
Without it, the server accumulates zombie subscribers and keeps marshalling for
nobody.

**6.3 The upgrade handshake.** `Connection: Upgrade`, `Sec-WebSocket-Key`, the
magic GUID, the SHA-1/base64 accept value. Proxies that don't forward upgrade
headers are a classic production surprise. Also `Origin` checking — WS is
**not** subject to same-origin policy, so any site can open a WS to your server
unless you check `Origin` yourself. (SSE, being `fetch`-like, is governed by
CORS.) This is a real security difference, not a detail.

**6.4 Per-connection write serialisation.** Concurrent writes to one WS
connection corrupt the frame stream. Each connection needs a single writer
goroutine consuming from its channel — which is where the bounded-buffer
backpressure design would live, same policy as section 3.

## 7. Sketch, if it were built

```
                   ┌──────────── one per process ────────────┐
exchange WS ──► upstreamClient ──► hub ──► sessions map
   │              (reconnect,        │      ┌── session ──┐
   │               resubscribe,      ├─────►│ chan[N]     │──► writer goroutine ──► conn
   │               seq gap detect)   │      │ ping timer  │◄── reader goroutine ◄── conn
   └── ping/pong                     │      └─────────────┘        (pong, close)
                                     └─────► ... 
```

- **`upstreamClient`** owns exactly one exchange connection. On drop:
  backoff+jitter reconnect, then **re-send every subscription** (exchanges do
  not remember them) and mark state stale until the first message arrives.
- **`hub`** is unchanged from the SSE version — same broadcast, same
  drop-oldest. That is the payoff of having put the seam at `source`.
- **`session`** = one client. Reader goroutine handles pong/close and any
  client→server messages; writer goroutine is the *only* thing touching the
  connection.
- **Symbol-subset subscription** is the one genuine feature WS unlocks here: a
  tab showing only BTC would receive only BTC. That changes `broadcast` from
  one shared `[]byte` to per-topic payloads — and reintroduces the per-client
  marshalling cost that section 3 removed. Worth it only if payload size
  actually hurts; with 11 symbols it does not.

## 8. Scaling past one process

Everything above is single-process. The moment there are two:

- Each process holds its own upstream connection — acceptable (small constant),
  or elect one publisher and push through Redis pub/sub.
- **Sticky sessions are not required** for last-value-wins: any process can
  serve any client, because they all converge on the same current value. This
  is a nice property to point out — it is *why* the data semantics matter more
  than the transport.
- A shared bus (Redis/NATS) between processes reuses the same drop-oldest
  policy at the bus boundary.

## 9. Cross-cutting concerns

Written down explicitly, because interviewers grade what is *said*
(feedback-sd-state-explicitly):

- **Auth**: none needed — public market data, no per-user state. An order
  channel would need per-connection auth at handshake, and re-auth on
  reconnect.
- **Origin/CSRF**: SSE is CORS-governed; **WS is not** — check `Origin`
  server-side (§6.3).
- **TLS**: `wss://` / `https://` only. Plaintext `ws://` from an `https://`
  page is blocked as mixed content anyway.
- **Rate limiting**: connections per IP, and a cap on total subscribers so a
  connection flood cannot exhaust file descriptors.
- **Observability**: subscriber gauge, drop counter (drops/sec is the
  backpressure signal — currently *not* instrumented, see below), upstream
  reconnect counter, broadcast latency.
- **Load shedding**: above a subscriber ceiling, refuse new connections with
  `503` and a `Retry-After` rather than degrading everyone.
- **Graceful shutdown**: close sessions with a normal-closure code so clients
  reconnect promptly instead of waiting for a timeout.

## 10. Observability (added 2026-09-03)

`hub.stats()` is surfaced on `/ops` under 價格推流, and in the `/ops/services`
JSON as `stream`:

| field | why it exists |
|---|---|
| `running` / `subscribers` / `peak_subs` | is the poller up, and for whom. `peak_subs` is a high-water mark and deliberately does **not** fall back with the live count |
| **`dropped` / `drop_pct`** | **the backpressure signal.** A drop means a client fell behind a 3s interval |
| `delivered` | denominator for `drop_pct`; grows per-subscriber-per-broadcast |
| `refreshes` / `refresh_errs` | upstream health, independent of client health |
| `last_age_sec` | staleness. `-1` = nothing broadcast yet. Flagged when subscribers exist and age > 3 intervals |

`reportDrops()` logs a **delta** only when drops have occurred since the last
report, so a healthy stream is silent and a starved one is visible in the
journal without a per-drop line (which at 3s × N subscribers would be spam).

Verified in production: 1 → 3 → 1 subscribers tracked correctly, `peak_subs`
held at 3 after they left, `refreshes` +1 per 3s, `delivered` growing at
subscribers × broadcasts. The drop path is covered by unit tests with exact
expected counts (`TestDropAndDeliverCounters`) rather than a production demo —
starving a real client needs ~2 minutes of socket-buffer fill to trigger.

## 10b. WebSocket upstream — shipped 2026-09-03

§5–§9 described the WS design as *not built*. The **upstream** half is now
built; the client-facing transport stayed SSE, exactly as §3 argued.

**Protocol facts, all read off the wire with `cmd/wsprobe`, not from docs.**
That mattered: the pre-existing `bingx/ws.go` scaffold carried notes from a
previous look, and probing confirmed one and would have caught a wrong one.

| fact | detail |
|---|---|
| framing | BINARY frames containing **gzip**. A 145-byte frame holds ~120 bytes of JSON — compression barely pays at this size, but it is not optional. |
| keepalive | application-level **TEXT `"Ping"` → reply `"Pong"`**. NOT a protocol ping, so gorilla's automatic handler never sees it. Miss it and the server drops you. |
| multiplexing | **ONE connection carries N subscriptions.** Verified with 4 dataTypes on one socket, each ~1 frame/sec independently. This is why the hub needs one upstream connection, not one per symbol. |
| cadence | `@markPrice` pushes at **~1 Hz per symbol**. This is a 1-second feed, **not** a per-trade tick stream. |
| symbols | the US-stock synthetics (`NCSKSNDK2USD-USDT`) ride the same stream as the crypto pairs. |
| `@ticker` | gives real 24h `h`/`l`, but its `o`/`p`/`P` are over a **short (~2 min) window**, not 24h — so the true 24h change still needs klines. |

**Upstream is now split, which is what the injected `source` seam bought:**

```
bingx ws @markPrice ──► markLoop  ──┐  (price, ~1 Hz, ONE connection, 11 symbols)
                                    ├─► emit(): overlay + marshal once ──► broadcast
REST buildTickers ──► statsLoop ────┘  (24h ref / funding / range, every 30s)
                        broadcastLoop coalesces to <= 1 per 500ms
```

- `statsInterval` went **3s → 30s**. REST was only the freshness floor because
  it was the *only* source; everything it still provides moves on the order of
  minutes. Measured: `refreshes` grew 3 → 4 over 32s.
- `broadcastInterval` 500ms coalesces. 11 symbols × 1 Hz would be ~11 SSE
  frames/sec per tab to move one number.
- **The overlay is non-destructive.** A symbol with no WS price keeps its REST
  price, so a partial or dropped feed degrades per-symbol instead of blanking
  the strip. `emit()` copies rather than mutating `statRows`, or the next REST
  refresh would diff against WS-contaminated values.
- Only a **changed** mark sets the dirty flag — BingX republishes the same
  price when nothing traded.
- `RunMarkPrices` supervises with **exponential backoff + jitter** (§6.1), logs
  every drop, and resets the backoff after a connection that stayed up >1min so
  one bad night does not pin the delay at the ceiling.
- Sends into the mark channel are non-blocking drops — same last-value-wins
  licence as §3, and the same warning: do not copy it to a fills stream.

**Freshness floor is now ~1s, was 3s.** Verified in production: 15 events in
14s, all 11 symbols flagged `live`, zero drops, one `ws connecting (11 symbols,
attempt 1)` with no reconnect churn.

`DecodeMarkFrame` is pure and carries the protocol tests (18 cases: gzipped and
plaintext, the Ping in four whitespace variants, subscription acks, other
channels on the shared socket, non-zero `code`, zero/unparseable price, and the
stock synthetics).

## 11. Remaining gaps

- **No subscriber cap.** Nothing refuses connection number 10,000. §9 load
  shedding is designed, not built.
- **The floor is now the exchange's ~1 Hz `@markPrice` cadence**, not our
  polling. Going below that needs `@trade` (per-print) and a different
  aggregation story; `SubscribeTrades` is still a stub.
- ~~No `/ops` readout of websocket connection health~~ — **done**. `/ops` now
  shows **two** rows, because there are two independent failures: the exchange
  websocket can be dead while the SSE fan-out is perfectly healthy (it keeps
  serving 30s-old REST prices), and vice versa. One combined "stream OK" light
  would hide exactly that case.
  - `RunMarkPrices`' hook became a **`StreamEvent` struct** instead of a
    pre-formatted string. The first version handed back log sentences, which
    meant a caller wanting to *surface* health had to parse English back into
    fields. Package `bingx` owns the facts; the caller owns the wording.
  - `BackoffFor(attempt)` is exported and pure, so the retry schedule is
    asserted (bounds per attempt, cap at 30s, no overflow at attempt 40) rather
    than described — including a test that the jitter actually **varies**,
    since non-varying jitter defeats its only purpose.
  - **Connected ≠ working.** `ws_up` is set by the first received FRAME, never
    by a successful dial, and the card flags "連線中但無資料" when up but stale
    (>10s on a ~1Hz feed). `ws_live_syms / ws_total_syms` exposes the partial
    feed — some symbols streaming, others silently not.
  - The DOWN path could not be produced on a live box without breaking the feed
    for real, so it is covered by unit tests on the state machine rather than a
    production demo. Verified live: `ws_up true, 11/11 live, age 0s, connects 1,
    drops 0`.
- `SubscribeKlines` / `SubscribeDepth` remain stubs; nothing needs them yet.
- **Single process only** — see §8 for what changes with two.
- No `Last-Event-ID` handling. Harmless here (last-value-wins means a
  reconnect just needs current state, which replay-on-connect already gives),
  but it is the hook a resumable stream would use.
