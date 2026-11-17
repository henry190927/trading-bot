# HTF trend in the auto-executor — paper now, live later (design, no code)

**Status:** DRAFT for review · **Date:** 2026-11-15 · **Base:** main 567729d
**Strategy:** docs/htf_trend_design.md (4h Donchian N20 breakout, initial stop
3×ATR(20) = 1R, trail = opposite M=10 channel, ratcheted, no TP). Code:
`autostrat/htftrend.go` (HTFTrendTrigger :41, HTFTrendTrailStop :72,
SimulateHTFTrend :153, runTrail :174). Verdict there: paper-ship eligible.

## 摘要（zh-TW）

- 現在的 autotrade 管線從頭到尾假設「固定停損 + 固定止盈」。htf-trend 沒有 TP，
  直接接上去會**靜默出錯**：`EvaluateFire` 對 TP=0 的多單會在下一根 K 判成「止盈」，
  價格 0，R ≈ −16R（停損 6% 時），這一筆就會觸發每日熔斷（線上設定 −6R，程式預設 −3R），**整個自動執行器停一天**。
  所以第一步是防呆，不是接策略。
- 紙上交易設計：把移動停損的唯一實作搬到 `autotrade`（避免 import cycle），
  新增 `EvaluateTrailFire`，從「紀錄的 fire + K 線」**重算**每根 4h 收盤後的停損，
  不另存狀態（paper log 是 append-only，重算是精確的）。回測與紙上共用同一個函式，
  並用 parity test 逐根比對。
- 實盤（之後）：每根 4h 收盤算出新停損 → 先掛新單、再撤舊單（`protect.MoveStop` 已是這個順序），
  交易所才是狀態；要先解決：開倉沒有 market 單函式、`PlaceLimit` 的 hedge 參數寫死 false、
  訂單歸屬（hedge 模式下手動單和自動單是同一個倉位）。
- 風控：rule 層級 `risk_pct`（建議 0.25–0.5%）、R 單位不一致時熔斷要換算、
  同方向持倉上限（需先預先登記一次 trendbt 回測再定數字）。
- 誠實的限制：紙上前測**驗證不了 edge**（要 ~1000 筆才能讓 +0.08R/筆 達 2 SE），
  它驗證的是機制與 parity。是否上實盤要靠回測 + parity，不是靠紙上 P&L。
- 待你決定的事見 §6。

---

## 0. Scope

**IN:** paper execution of htf-trend through the existing executor and /ops panel;
a shared trail primitive; parity tests; a per-rule risk override; a same-side cap;
the live design (written, not built).
**OUT:** building the live trail manager; any change to N/M/K or a trend filter
(pre-registration §5 of the strategy doc forbids it); changing other strategies'
scoring; engine changes (closed-bar evaluation stays as is).

## 1. The pipeline as it is, and what breaks

### 1.1 As built

| step | where | what it assumes |
|---|---|---|
| 60 s tick, hot-reload config | cmd/monitor/autoexec.go:62, :73 | nothing per-TF; each rule reads its own TF |
| book from the fire log | autoexec.go:90-91 → autotrade/book.go:68 | every fire scored by `EvaluateFire` (book.go:103, :130) |
| daily-loss halt | autoexec.go:108 → caps.go:90 | `RealizedRToday` = settled TP/stop R today, deduped (book.go:137-146) |
| equity read | autoexec.go:124 | only when `cfg.RiskPerTradePct > 0` |
| trigger | autoexec.go:144 → autostrat/eval.go:64 | strategy switch; `htf-trend` absent (KnownStrategies autotrade.go:109) |
| per-bar dedup | autoexec.go:151-162 | in-memory `lastFireBar`; a fire may happen any minute while that bar is newest |
| blackout | autoexec.go:165 | silent `continue`, nothing recorded |
| one slot per rule (paper) | autoexec.go:175 → paperBlocked :337 | newest fire per Symbol|Strategy in the last 300 rows (:339-340); `EvaluateFire` (:355); slot frees on tp/no-fill/unscored (:361) |
| one slot (live) | autoexec.go:171-173 | ANY open position on the symbol blocks |
| sizing | autoexec.go:184 → sizing.go:82 | book-wide `RiskPerTradePct`; leverage = min(rule, MaxLevForStop) (:109) |
| caps | autoexec.go:205 → caps.go:87 | same-symbol-side (:114), total count (:129), margin (:136) |
| paper record | autoexec.go:260-269, fireRecord :321 | `PaperFire` has Entry/Stop/TP only (autotrade/paper.go:14-40) |
| live placement | autoexec.go:297 | `PlaceLimit(..., trig.Stop, trig.TP, false)` — a bracket, **hedge=false** |
| panel | cmd/web/autotrade.go:202-217 | `BuildBook` + `ReplayWithCaps(..., 6, cooldown, time.Hour, EvaluateFireLive)` |
| scanner | cmd/web/scan_setups.go:197-204 | every enabled rule is evaluated; `scanRR` (:213) needs a TP |

### 1.2 Assumptions that break for a no-TP trailing exit

- **B1 — TP=0 is scored as a take-profit at price 0 (the dangerous one).**
  `EvaluateFire` checks `c.High >= f.TP` (outcome.go:144). With TP=0 a long that
  is not stopped on the first bar after the fire becomes `OutTP`, exit 0,
  `NetR = (0−entry)/risk` ≈ −16.7R for a 6 % stop. `EvaluateFireLive` does the
  same against the mark (:218). Consequences: book.go:139-145 adds it to
  `RealizedRToday` → the daily halt trips (live −6R, code default −3R) and **every rule stops for the day**;
  paperBlocked treats tp as "free to re-enter" (:362) so it re-fires next bar;
  the panel paints it green. A short is the mirror failure: `c.Low <= 0` never
  holds, so it sits at its initial stop forever and never trails. Any htf-trend
  fire logged before the resolver exists triggers this. (Checked with a throwaway
  test on 567729d: entry 100 / stop 94 / TP 0, long → `tp`, NetR −16.67, exit 0;
  short → `open`.)
- **B2 — no trail anywhere in scoring.** `EvaluateFire` only knows a fixed stop
  (outcome.go:138-159). The trail lives in autostrat, which `autotrade` cannot
  import (autostrat imports autotrade — autotrade.go:106-108).
- **B3 — "settled" means OutTP|OutStop**, hard-coded in Summarize (outcome.go:320-347),
  WithFee (:365), PnLUSDT (paperbal.go:52), rTradesFromPositions (cmd/web/rstats.go:49),
  the breaker (book.go:139), DedupFires/ReplayWithCaps hold switches
  (outcome.go:269-280, replay.go:110-122), the row flag (cmd/web/autotrade.go:249).
  A new status would have to be added to all of them; one miss leaves trades silently
  unsettled.
- **B4 — WinRate = TP/(TP+Stop)** (outcome.go:345-347). Every trail exit is a stop
  order, so htf-trend would show 0 % WR with positive R.
- **B5 — R units.** R is gross everywhere on the panel; fees exist only in USDT
  (paperbal.go:37-40, FeeBp=6). `SimulateHTFTrend` reports NetR *after* a 10 bp fee
  (htftrend.go:136-140, :201). And once rules risk different % of equity, summing
  their R (breaker, equity curve) mixes units.
- **B6 — cooldown.** paperBlocked uses the rule's own cooldown × its TF
  (autoexec.go:360), but BuildBook and the panel use one max-across-rules value ×
  **1h** (book.go:132-137, autobook.go:72; cmd/web/autotrade.go:185-198, :217). The
  backtest has no cooldown (next signal ≥ exit+1, htftrend.go:168). Every trail exit
  is a stop, so the replays would absorb re-entries the executor really took.
- **B7 — kline depth.** The executor caches 300 bars (autobook.go:59) = 50 days of
  4h. A fire older than the window is `OutUnscored` (outcome.go:73-74), which books
  no R, does not count as open (book.go:108) and frees the slot (autoexec.go:361) —
  so a long-running trend position would be re-entered. Separately the trail for
  the first bar reads `cs[i-M+1..i]` (htftrend.go:72-80): the panel fetches only
  from oldest-fire − 2 bars (cmd/web/klinecache.go:34), and `HTFTrendTrailStop`
  silently returns `cur` when `j < m-1` — a looser trail with no error.
- **B8 — fire window.** paperBlocked reads the newest 300 rows (autoexec.go:339);
  a position held for weeks can scroll out while other rules keep firing → re-fire
  while still open. The book reads 500 (autoexec.go:90).
- **B9 — stale entries.** Dedup is per bar (autoexec.go:161), not "at the close":
  a candidate blocked by caps at 00:01 can fire at 03:30 with `Entry` = the 00:00
  close (paper fills at `f.Entry`, outcome.go:90-92). The backtest fills at the
  signal close only.
- **B10 — expiry is meaningless** for a market entry (EvaluateFire uses it only for
  resting limits, outcome.go:93-117), but ReplayWithCaps frees the rule's slot
  `expiry×1h` after an *open* fire (replay.go:120-121). Harmless only because the
  executor never logs a second fire while the first is open.
- **B11 — slot key is Symbol|Strategy, not TF** (autoexec.go:340, outcome.go:257,
  book.go:77, replay.go:79). A 4h and a 1d htf-trend rule on one symbol would block
  each other.
- **B12 — rendering.** TP column (templates/autotrade.html:179) shows 0;
  `scanRR` with TP=0 gives −16.7 for longs, +16.7 for shorts; recording that scan
  row into /setups gives the forward log a nonsense target.
- **Not broken, despite the framing:** the tick is 60 s, not 1h; a 4h rule is
  evaluated every minute on its last closed 4h bar and fires within ~1 min of the
  close. The 1h assumptions are the hard-coded `time.Hour` in B6/B10.

## 2. Paper design

### 2.1 Where "exit = trail" lives: on the trigger and the fire, not the rule

A rule field `exit: "trail"` would be a knob a config could set on range-edge,
which has no trail to run — the RequireAligned/MaxConcurrent shape removed in
autotrade.go:33-42. The exit belongs to the strategy, so: `Trigger.TrailM int`
(0 = fixed bracket), copied to `PaperFire.TrailM` (omitempty). Every older row
decodes to 0 and keeps today's path byte-for-byte.

New `PaperFire` fields (all omitempty): `TrailM int`, `BarTime time.Time` (signal
bar's CloseTime — removes any inference from `Time`; BlockedCandidate already has
one), `CooldownBars int` (the rule's, so replays stop guessing — B6).

### 2.2 One trail implementation, in `autotrade`

Move the pure primitive to `autotrade/trail.go`:
`TrailStop(cs, j, long, cur, m)` (today's HTFTrendTrailStop) and
`WalkTrail(cs, i, side, entry, stop, m) TrailWalk` (today's runTrail loop, gross,
no fee). `autostrat.HTFTrendTrailStop` / `SimulateHTFTrend` become thin callers, so
trendbt, the executor and the panel all execute the same loop. Fee stays where it
is (trendFeeR) for the backtest; the book keeps R gross (B5).

### 2.3 Resolver: `EvaluateTrailFire(f, cs) Outcome`

1. `i` = index of the bar with `CloseTime == f.BarTime` (fallback for rows without
   it: newest bar with `CloseTime <= f.Time`). Not found → `OutUnscored`.
2. Coverage: require `i >= f.TrailM-1` (the first trail's lookback). Otherwise
   `OutUnscored` — never compute a looser trail (B7).
3. `WalkTrail` from `i`. Exit found → `OutStop` at the walk's exit price
   (`min(stop, Open)` on gaps, htftrend.go:189-195), `ExitAt` = exit bar close,
   `NetR` = gross R vs the **initial** stop. No exit → `OutOpen`, `UnrealR` marked
   to last close, new field `Outcome.CurStop` = stop in force for the forming bar.
4. Live overlay (panel only): mark ≤ CurStop (long) → resolve now at CurStop; else
   UnrealR at the mark. Same rule as EvaluateFireLive, against the trailed level.

Status stays `OutStop` for every exit (it *is* a stop order), so all of B3's
plumbing settles it unchanged. Add `Outcome.Trailed bool` (exit stop ≠ initial)
for display. Fix B4 by counting wins as `NetR > 0` among TP+Stop: for every
fixed bracket a TP is > 0 and a stop is exactly −1 (gross), so this is
byte-identical for existing rules — pin that with a test over the current log shape.

### 2.4 State: recompute vs persist

| | recompute from fire + klines | persisted trail state |
|---|---|---|
| exactness | exact: trail reads only High/Low of closed bars; entry and initial stop are in the fire | exact only if every update was written; a missed tick or restart diverges |
| log model | fits the append-only log | needs a mutable file → the two-writer CSV hazard bracket.go:20-23 warns about |
| restart | free | needs recovery logic |
| cost | klines back to `BarTime − (M−1)` bars; one call per (symbol, TF) per tick | none |
| failure mode | kline gap or revision → Unscored (visible) | silent drift |

**Choice: recompute.** The cost is fixed by B7: for trail fires the executor
cache fetches 1440 bars (BingX max = 240 days of 4h, bingx/client.go:59-61), and a
trail fire that still cannot be scored is treated as **open** (holds its slot, counts
in the book) — fail-safe, the same posture BuildBook takes for missing candles
(book.go:34-39), the opposite of today's Unscored-frees-the-slot. Live keeps this
property: the desired stop is recomputed, the actual stop is read from the
exchange; no local state either way.

### 2.5 Dispatcher and call sites

`autotrade.Resolve(f, cs, expiry) Outcome` = `EvaluateTrailFire` when
`f.TrailM > 0`, else `EvaluateFire`; `ResolveLive` likewise. Replace the four
direct calls: book.go:103, book.go:130, autoexec.go:355, cmd/web/autotrade.go:206.
Defence in depth for B1: `EvaluateFire` with `TP <= 0` returns `OutOpen` (holds a
slot, books no R) — the existing convention for a malformed bracket (outcome.go:67-68);
same in EvaluateFireLive.

### 2.6 Executor changes (cmd/monitor)

- `evalHTFTrend` in autostrat: Klines(sym, TF, 500), `indicator.ATR(cs, 20)`, call
  `HTFTrendTrigger` on the last closed bar. 500 bars makes the Wilder seed
  (atr.go:25-33) irrelevant ((19/20)^480 ≈ 2e-11) so the live stop equals
  trendbt's. Set `TrailM`, `TP: 0`. Add to the switch and KnownStrategies
  (dispatch_test pins both).
- Fire only within `trailFireGrace` (5 min) of the signal bar's close; otherwise
  record a blocked candidate `stale-signal` (B9). A breakout not taken at its close
  is not taken — what the backtest does.
- paperBlocked for trail rules: find the newest fire for the rule in the whole log,
  not the last 300 (B8); resolve via `Resolve`; cooldown from the fire (0 for
  htf-trend, matching the backtest).
- Record blackout skips for trail rules in the blocked log so every signal the
  backtest takes and paper does not has a written reason (§5 P5 needs it).
- Strategy name: one `htf-trend` rule per symbol. If the 1d variant also runs, it is
  strategy `htf-trend-1d` (same code, TF from the rule) rather than re-keying every
  slot map on TF (B11) — re-keying changes existing strategies' dedup.

### 2.7 Panel and scanner

- Trail positions show `trail @CurStop` in the TP column and "trailed" on exit.
- Breakdown by strategy shows the rule's own R; book-level totals that mix rules
  are shown in USDT (PaperBalance already is) or in normalised R (§4.3).
- `scanRR` returns 0 for TP ≤ 0 and the row says "trail"; recording a trail row to
  /setups is refused until /setups has a trail outcome (it does not).
- `?since=` and paper balance: unchanged; they consume Outcome.

## 3. Live design (later phase — not built now)

### 3.1 Entry

There is no market-open order in the client (orders.go: PlaceLimit :177,
PlaceStopMarket :247, MarketCloseReduceOnly :317 only). Options: a marketable limit
at signal close ± k ticks with a short expiry, or add `PlaceMarket`. The backtest
fills at the close; the no-fill rate is the book's biggest documented leak, so a
plain limit at the close is wrong for a breakout. Also fix autoexec.go:297, which
passes `hedgeMode=false` while the VPS runs hedge mode (the leverage call 30 lines
up already switches on BINGX_HEDGE_MODE, :283-289).

The first live stop is not the 3×ATR stop: the trail for bar i+1 is already known
at entry (`TrailStop(cs, i, …)`, htftrend.go:187). Place `max(initial, trail(i))`
(long) as the bundled stop; R stays measured against the initial stop.

### 3.2 Trail manager (one goroutine, runs a minute after each 4h close)

For each live trail fire still open: desired = `EvaluateTrailFire(...).CurStop`;
actual = our resting stop on the exchange. If desired is tighter than actual by
≥ 1 tick (and ≥ a min step, to avoid churn), move it; if actual is already tighter
(a human moved it), leave it — the stop never loosens, by anyone's hand.

Move = place new, then cancel old — `protect.MoveStop` (protect/protect.go:227-264)
already has this order and its failure semantics (place fails → old stays; cancel
fails → two reduce-only stops, which cannot over-close). Two required changes
before reuse:

- **Ownership.** `protect.BuildPlan` sizes from the live position (:95-106) and
  move_stop cancels every protective stop (cmd/web/move_stop.go:132-136). In hedge
  mode a manual ETH long and an htf-trend ETH long are ONE exchange position, so
  that would resize and cancel the user's stop. The manager must use the fire's own
  qty and cancel only its own order id. The client has no clientOrderId support
  (grep bingx/: none) — add it (verify BingX swap accepts `clientOrderID`) and tag
  `ht-<fire time>`; ownership is then read from the exchange, not stored.
- **Mark check.** BuildPlan refuses a stop at/through the mark (:124-130). If the
  trail is through the mark at the 4h close (the bar closed beyond it), the
  backtest would exit on the next bar's open — the manager closes at market
  (`MarketCloseReduceOnly`, own qty) instead of placing.

### 3.3 Bracket guard

Auto fires are not journaled (autoexec.go:311 writes the paper log only), so
bracketCycle never sees them; orphanCycle does (cmd/monitor/bracket.go:421): any
position whose Symbol|side has no journal row with a stop must carry a protective
stop, checked with `bracket.Protects` (bracket/bracket.go:240) every 30 s (:66).
Place-before-cancel keeps it quiet: during a move there are two stops, never zero.
A cancel-first implementation would page "NAKED / NOT IN JOURNAL" every 4h per
position. StopOrderID is a journal field (journal.go:139-141) and plays no part
here. If the user journals a trade on the same symbol+side, orphanCycle hands that
key to bracketCycle (guardCoverage :406-418), which compares against the journal
stop — so the shared-position case (3.2) must be refused at entry: keep the
existing live gate (any position on the symbol blocks, autoexec.go:171-173) and
alert when a manual trade lands on top of an auto position.

### 3.4 Failures

| failure | behaviour |
|---|---|
| klines fetch fails | skip this cycle, keep the old stop (it is only looser, never absent); alert after 2 missed 4h closes |
| place new stop fails | old stop stays; retry next minute up to the next close; ntfy |
| cancel old fails | two stops rest; next cycle retries the cancel by id |
| position gone (stop hit) | fill-detection path (position_gone.go) reports; the fire resolves from klines as in paper |
| exchange stop ≠ any desired level (human edit) | never loosen; log once |
| daemon down across a close | next run recomputes from klines — no state lost |

### 3.5 Rate limits and amend

Per position per 4h: positions + open orders + mark + place + cancel ≈ 5 signed
calls; 9 positions ≈ 45 calls per 4h — negligible, but stagger the 9 by a few
seconds at the close. BingX has a cancel-and-replace endpoint (verify:
`/openApi/swap/v1/trade/cancelReplace`); its cancel-first order is exactly the one
protect.go:232-233 rules out, so do not use it even if it exists.

## 4. Risk and caps

### 4.1 Per-rule `risk_pct`
`Rule.RiskPct float64 \`json:"risk_pct,omitempty"\``; effective = rule if > 0 else
`cfg.RiskPerTradePct`; 0 → fixed margin as today. `Size` takes the effective value
(sizing.go:104-121), the equity read (autoexec.go:124) runs when **any** enabled rule
has an effective pct > 0, and the panel rules table shows it per rule. Backtest
suggests 0.25–0.5 % vs the book's 1.5 %: MDD 46.5R (htf_trend_design.md §6) is
11.6 % / 23 % / 70 % of equity at 0.25 / 0.5 / 1.5 %.

Note the real money at risk is usually below the sizing figure: the first stop is
`max(initial, trail(i))`, and the backtest's average loss is −0.50 to −0.56R
(htf_trend_design.md §6 table).

### 4.2 R units in the breaker
With mixed risk_pct, −1R of htf-trend is a sixth of −1R of a fade at 1.5 %. The
breaker (book.go:145) should sum `NetR × w`, `w = (RiskUSDT/Equity) / (book pct/100)`
from the fire's recorded sizing (paper.go:34-35); `w = 1` when either is missing (old
rows, fixed-margin mode). Same weighting for any book-level R on the panel, with the
unit labelled. Without it the halt fires late for 1.5 % rules and early for 0.25 % ones.

### 4.3 Same-direction cap
All 9 symbols fired together in the backtest (max concurrent 9/9). Proposal:
`Config.StrategyCaps map[string]{MaxSameSide int}` checked in CheckCaps with
`Candidate.Strategy` and `Leg.Strategy` added; zero = unlimited like every other cap
(caps.go:82-86). Slots are taken first-come in rule order (orderForReplay,
replay.go:138), which is a selection rule and changes results. **The value must come
from a pre-registered trendbt run with the cap**, listed as untested in
htf_trend_design.md §6 — not from the post-hoc concurrency count (a bucket split
is not an A/B).

### 4.4 Liquidation guard and size
Median stop 5–6.5 % → `MaxLevForStop` 7–9× (sizing.go:54-61); worst stops 1–2×. The
trail only moves toward price, so a stop inside the guard at entry stays inside.
Example: equity E, 0.5 %, 6 % stop → notional 0.083E, margin ≈ 0.010E at 8×; at E
= 400u that is 33u notional — check each symbol's min qty/notional and lot step
(cmd/contracts) before live; 0.25 % halves it.

### 4.5 Shared budget
htf-trend positions occupy `max_concurrent_total` / margin slots (caps.go:129-136)
that the fade rules also need, and can sit for weeks. Either accept, or give trail
rules their own count via StrategyCaps. User decision.

## 5. Phased plan (each step ships alone)

| # | step | tests | verify |
|---|---|---|---|
| P0 | EvaluateFire/Live: TP ≤ 0 → OutOpen; scanRR TP ≤ 0 → 0 | long+short TP=0 cases; existing suite unchanged | `go test ./...`; no behaviour change on the current log (panel totals identical before/after) |
| P1 | move trail primitive to autotrade/trail.go; autostrat wraps it | htftrend tests unchanged | `trendbt -tf 4h -fee-bps 10 -report` output byte-identical before/after |
| P2 | PaperFire fields; EvaluateTrailFire; Resolve/ResolveLive; WinRate by NetR>0; 4 call sites | **prefix parity test** (below); WinRate identical on fixed-bracket fixtures; unscored on short coverage | panel totals identical on the current log (no trail fires yet) |
| P3 | evalHTFTrend + KnownStrategies; grace window; paperBlocked full-log + Resolve; kline depth 1440 for trail TFs; blackout recording; `risk_pct`; equity-read condition | trigger parity (live window vs full history at same bar); Size with rule override; dispatch_test | add 9 rules **disabled**, then enable one (BTC) for a week; check the first fire by hand against trendbt |
| P4 | panel: trail column, Trailed, R-unit labels, breaker weighting; StrategyCaps (value pending §6) | Book/replay tests with mixed risk; CheckCaps strategy cap | compare one closed real trail fire on /ops with trendbt for the same bar — a display that never had real data is untested |
| P5 | `trendbt -compare-log autotrade_paper.jsonl -since <start>`: per-fire match (signal bar, entry, stop, exit, R) + every missing/extra signal with its blocked reason | — | weekly; any unexplained mismatch stops the forward test |
| P6 | live (later): PlaceMarket or marketable limit, hedge flag fix, clientOrderId, trail manager | fake-exchange tests for place/cancel ordering and all §3.4 rows | DryRun, then one symbol at minimum size |

**Prefix parity test (P2, the core one).** For fixture candles (synthetic + a
recorded real 4h slice in testdata): run SimulateHTFTrend; for each trade build
the fire it would have logged; for every prefix `cs[:k+1]` with k ≥ EntryIdx,
`EvaluateTrailFire` must give OutOpen with `CurStop` = the walk's stop for bar k+1
while k < ExitIdx, and OutStop with the same exit price, exit time and GrossR once
k ≥ ExitIdx. This proves the incremental view (paper today, live later) equals the
batch backtest at every bar, not just at the end.

## 6. Open questions for the user

1. **risk_pct** for htf-trend: 0.25 % or 0.5 %? (MDD ≈ 12 % vs 23 % of equity.)
2. **Symbols:** all 9 as pre-registered, including XAG (9 months of 4h history,
   −3.1R)? The pre-registration says do not drop losers; including it is consistent.
3. **1d variant** also on paper (as `htf-trend-1d`)? Thin (≈1 trade/symbol/month).
4. **Same-side cap value** — only after a pre-registered trendbt run; OK to run it?
5. **Slot budget:** share `max_concurrent_total` with the fades, or separate?
6. **Forward test length / stop criteria.** The forward test cannot confirm the
   edge: with SD 1.34, +0.083R/trade needs n ≈ (2×1.34/0.083)² ≈ 1040 trades for
   2 SE (~23 months at 45/month); the latest-year +0.030 needs ~8600. Proposal: the
   paper phase checks mechanics and P5 parity only (e.g. 6 weeks with zero unexplained
   mismatches), and the live decision rests on the backtest.
7. **Live collision policy:** keep "any position on the symbol blocks" (htf-trend
   then misses breakouts while a fade or manual trade is on), or allow and accept
   the shared-position risk in §3.2?

### 6.1 Decisions (user, 2026-11-15)

1. `risk_pct` **0.25** for every htf-trend rule.
2. **8 crypto symbols** (BTC ETH SOL XRP SUI HYPE NEAR LINK). XAG is left out by the
   user's call, a deliberate departure from the pre-registered 9.
3. **1d variant runs too**, as `htf-trend-1d`, same symbols and risk.
4. Same-side cap: run a **pre-registered trendbt** test first, then choose the value.
5. **Separate slot budget** from the fade rules.
6. **Shorter forward test.** The mechanics are verified OFFLINE first: P5's compare
   tool replays trendbt's historical signals through the live resolver and panel code
   (a synthetic paper log), so parity is checked over hundreds of past trades in one
   run instead of waiting for them. Live paper then only has to cover what a replay
   cannot (tick timing, kline fetch depth, grace window, restarts): all 8 symbols
   enabled together from day one, **2 weeks** with zero unexplained P5 mismatches.
   The edge itself still rests on the backtest; no paper length can prove it.
7. Live collision policy: keep "any position on the symbol blocks"; revisit at P6.

## 7. What could silently go wrong

- **B1 before P0/P2 land.** Enabling an htf-trend rule earlier halts the whole
  executor through a fake −16R "tp". P0 ships first; P3 must not merge without P2.
- **Two implementations of the trail.** If the panel ever scores with something
  other than the walk trendbt uses, paper and backtest drift with no error. P1 makes
  that structural; the prefix test pins it.
- **Short history → looser trail** (B7) — refused as Unscored, held as open.
- **Unscored frees the slot** (autoexec.go:361) — trail fires fail safe instead.
- **Stale fills** (B9) — grace window.
- **Display never fed real data.** The trail column, the Trailed flag and breaker
  weighting have no real rows until the first fire; P4's check is against a real
  closed fire, not a fixture.
- **Mixed R units** summed on one curve (B5/§4.2).
- **Cooldown mismatch** between executor and replay (B6) — cooldown carried on the
  fire.
- **Paper ≠ live on same-symbol overlap:** paper lets htf-trend and a fade hold the
  same symbol; live blocks it (autoexec.go:171-173). Paper R will be higher than
  live could have been; P5 should count these overlaps.
- **Kline revisions / exchange gaps** change a recomputed exit after the fact; rare,
  but P5 would show it as a mismatch, which is the point.

## 8. Checklist (stated, not assumed)

Closed bars only (Klines drops the forming bar, bingx/candles.go:24-31) · no engine
change · paper default, live triple-gated (autotrade.go:129-133) unchanged · no new
mutable state file · every refusal recorded with a reason · R gross on the panel,
fee in USDT · parity test in CI · ntfy on every fire and every live stop move ·
hedge mode handled on every order path · no rule knob without a consumer.

## 8. 1d switched to touch entry (2026-11-17, user decision)

After docs/htf_trend_entry_variants_design.md §7 (touch-1d ADOPT-candidate:
R/t +0.219 vs +0.176, paired t +2.49, gate PASS; 0.97 correlated with
close-1d, so a REPLACEMENT, not an addition) the user switched the 1d leg:
"換成觸價進場吧". 4h stays on close entry (touch-4h UNDECIDED).

- New strategy name `htf-trend-1d-touch` (TF 1d), so the 1d close-entry
  history stays separable; its rules replace the `htf-trend-1d` rules in
  autotrade.json and join the `htf-trend` slot group.
- `autostrat.HTFTrendTouchTrigger`: stop-entries at the prior 20-bar channel
  edges, ATR as of the bar before; fill = the level or the open on a gap;
  both edges touched → nearer the open, tie = none. The backtest's rules
  moved here as `SimulateHTFTrendTouch`; `TestSimulateTouchMatchesAutostrat`
  pins it to the A/B's `simulateTouch` trade for trade, fee included.
- **Paper replays the resting order from the closed bar.** The executor
  evaluates at the 1d close and logs the fire with Entry = the intrabar fill
  price and BarTime = that bar, `Touch: true`. So the paper fill is exact,
  but the fire appears up to a day after the fill — on the home-page decision
  card it can already show R moved.
- `PaperFire.Touch` makes `EvaluateTrailFire` check the fill bar itself:
  a fill bar that also reached the initial stop is a stop on that bar,
  −1R, as the backtest scores it (`TestHTFTrendTouchExecutorParity`).
- **Live** needs a real resting stop-entry order re-placed each bar plus the
  trail manager (P6); until then live trail fires stay refused as before.
