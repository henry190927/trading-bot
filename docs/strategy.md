# Strategy reference

How the engine scores a setup, the backtests that set its original defaults,
and how to read HVN / volume profile as a discretionary overlay. For the
system overview see the [README](../README.md).

## Confluence factors (vote model)

Each factor casts at most one vote per direction per bar. Score = max(bull votes, bear votes).

| Factor | When it votes bull | When it votes bear |
|---|---|---|
| RSI(14) | < 30 | > 70 |
| MACD(12,26,9) | histogram crosses up through zero | crosses down through zero |
| Bollinger(20, 2σ) | close at/below lower band | close at/above upper band |
| Fib 0.618 | price within 0.3% of level in an uptrend | within 0.3% in a downtrend |
| Liquidity sweep **(close-confirmed)** | wick pierced equal-lows AND closed back above AND no later candle closed back below the swept level | wick pierced equal-highs AND closed back below AND no later candle closed back above |
| RSI divergence | bullish regular divergence | bearish regular |
| CVD divergence (if trades available) | bullish regular | bearish regular |
| Range expansion + volume bar | bullish bar with range > 1.5×ATR(14) and vol > 1.5× 20-bar mean | bearish bar with same magnitude |

**Sweep invalidation (2026-05-27 fix):** a detected sweep is killed the moment a later candle closes through the swept level in the wrong direction. Prevents stale sweeps from continuing to fire after structure has already broken. Was the root cause of the XAG `LONG score=2` during the −2.4% dump on 2026-05-27.

**Range expansion (re-promoted 2026-05-27):** originally voted symmetrically, was demoted to display-only because symbol-regime dependent in an earlier backtest. Re-promoted after a XAG case where the engine gave a 9.0/10 LONG *while* a 3-ATR bearish range-expansion bar was being detected but ignored by scoring. Subsequent 60-day backtest on 1h: total netR +10.91 → **+19.97 (+83%)**, XAG flipped from −8.65R to +0.66R.

Context filters (`signal.Context`) downgrade signals — they don't add votes but add `Warnings`:
- Funding > +0.05% per interval → "crowded longs" warning on LONG signals
- Funding < −0.05% → "crowded shorts" warning on SHORT signals
- Open interest dropping > 2% → "squeeze, not real flow"

Display-only data (computed and shown as `·` lines, but does NOT vote):

- **HVN / 籌碼密集區** (Volume Profile) — top-5 high-volume nodes + POC from the last 200 bars. We tested HVN as a confluence vote and it dropped net edge across symbols (`backtest`: BTC −1R, Silver −5.8R) by diluting the score threshold with marginal setups. Surfaced for the trader's eye only — use POC and nodes for entry staging, partial-take placement, and stop selection.
- **MTF bias** — higher-timeframe MACD direction. Same story: backtest shows enforcing it hurts the mean-reversion edge (ETH dropped from +6.5R to −2.4R). Available behind `-bias` flag for experimentation.
- **Double top / double bottom** — pairs of pivot extremes within 0.3% of each other, separated by ≥5 bars, with a ≥1% valley/peak between them. The pattern surfaces only when current price is retesting the level. Backtest showed it as symbol-regime dependent — kept as a note for discretionary reading.
- **Daily / weekly / monthly opens** — opening prices of the current UTC day, week (Mon 00:00), and month. Institutional desks reference these as intraday S/R; price closing back through the daily open often resolves intraday direction. Shows `-` for periods where insufficient candle history is loaded. Use as anchor points for TP refinement and bias context, not for voting.

### Opt-in: stop refinement (清算區 / 流動性獵取)

Push the stop past nearby HVN levels or equal-highs/lows clusters within 0.5×ATR beyond the provisional stop, to dodge obvious sweep flows. Off by default — backtest showed wider stops shrink R-multiples within the 24-bar hold window (more timeouts at small loss, fewer 2R winners). However, the in-theory live benefit of avoiding actual stop hunts cannot be modeled by the R-based simulator (no slippage / wick-then-reverse). Enable with `-stop-refine` on `backtest` and observe live outcomes via the journal.

---

## Backtest results — May 2026 snapshot (60 days, 1h, fee = 6bp, stops = 1.5×ATR, min-score=2, sweep-only)

> **Historical.** These tables are the May 2026 runs that set the original
> engine defaults. They are kept because the decisions in the README cite them,
> not because they describe today's book: strategy changes since then were
> measured by the research harnesses and `shipgate` (see the README's
> *How a strategy change ships*), and the record of what passed and failed is
> in [open-and-rejected.md](open-and-rejected.md). Re-run `make backtest` for current numbers.

After the 2026-05-27 sweep-invalidation + range-expansion-voting fixes:

| Symbol | n | WR | netR (baseline) | netR (with fixes) | Δ |
|---|---|---|---|---|---|
| BTC-USDT | 68 | 49.2% | +3.75 | **+7.79** | **+4.04** |
| ETH-USDT | 66 | 47.4% | +8.09 | +6.15 | −1.94 |
| NCCOGOLD2USD-USDT (gold) | 72 | 45.2% | +7.72 | +5.37 | −2.35 |
| NCCOXAG2USD-USDT (silver) | 76 | 38.0% | **−8.65** | **+0.66** | **+9.31** |
| **TOTAL** | **282** | | **+10.91R** | **+19.97R** | **+9.06R (+83%)** |

**Reading these:**
- The XAG transformation (−8.65R → +0.66R) is the falling-knife problem disappearing — exactly the case that motivated the fixes.
- BTC also picks up materially (+4.04R) on better-quality sweeps.
- ETH and XAU regress slightly (still solidly positive) — the expected cost of being more selective.
- Gold is no longer the disaster it was — sweep invalidation rehabilitated it on 1h.
- Aggregate **+83% improvement in net R** with one fewer trade in the sample.

### Cross-timeframe comparison (2026-05-29 re-run)

All windows: 4 symbols, sweep-only, min-score=2, fee=6bp, ending 2026-05-29.

| TF | 60d | 90d | 120d | Trades / 60d | Verdict |
|---|---|---|---|---|---|
| 15m (30d only) | **−99.91R** | — | — | ~570 | **Don't trade.** Fee drag + noise crush the edge regardless of fixes. |
| 30m | −29.99 | −88.05 | −69.22 | ~590 | Net negative on every window. XAG-only research possible; XAU is a money fire. |
| **1h** | **+20.77** | −13.31 | −1.45 | ~287 | Highest 60d gain. Degrades on longer windows but stays near break-even. |
| 2h | +4.28 | −7.25 | −10.68 | ~130 | Near-zero edge across windows. |
| **4h** | +9.16 | **+8.21** | **+5.20** | ~53 | **Only TF positive on all 3 windows.** Lower trade count, lower DD, slower edge accumulation. |

**Two strategic choices for the daemon:**

- **1h** — highest absolute return on the 60d window. Trade-count rich (good for accumulating live samples). Use this if you want more signals.
- **4h** — robust positive across all three measurement windows. Slower (one trade per symbol per ~4 days). Use this if you want the strategy to "always be working" rather than oscillating between profitable and breakeven months.

Default in this repo is **1h** because of the 60d edge and signal volume — but 4h is a defensible alternative for a less attention-hungry deployment.

Per-symbol cross-TF pattern (60d):
- **BTC**: positive only on 1h and 4h
- **ETH**: strong on 1h (+14.98), weak elsewhere
- **XAU**: strong on 1h (+5.41) and 4h (+4.23), bleeds heavily on 30m
- **XAG**: positive on **every TF tested** — the most TF-robust symbol

At the time this set the retired `cmd/serve` daemon to `TRADING_TF=1h`, `TRADING_MIN_SCORE=2`, `sweep-only`, with 4h as the more robust alternative.

---

## Using HVN / 籌碼密集區 as a trader

Volume Profile maps where **executed** trading volume accumulated, by price level. It's a structural map of *positioning* — where participants own positions, where break-even points sit, where stop clusters form.

The engine's score tells you *"is the setup valid?"*. **HVN tells you *"is the trade going to work, and how much?"*.** Two different questions. The engine doesn't vote on HVN (backtest said voting on it hurts), but you should layer it on top of every signal as a discretionary overlay.

### 1. Direction context — read POC vs current price

| Position | Meaning | Implication |
|---|---|---|
| Price **far above POC** | Many positions opened below, now in profit | Demand below; longs have support, shorts face air pocket |
| Price **far below POC** | Many positions opened above, now underwater | **Overhead supply** as price tries to rally; longs fight break-even sellers |
| Price **at POC** | Balance zone — fair value | Expect chop; mean-reversion edge weakest |
| Price **between two HVNs** (low-volume gap) | Air pocket | Expect acceleration — price doesn't dwell here |

### 2. Setup quality filter — sweep direction × HVN side

The single highest-leverage HVN application:

| Setup | HVN context | Verdict |
|---|---|---|
| Sweep HIGH (short) **into an HVN from below** | Best | Trapped longs at HVN + chip resistance = strong reversal |
| Sweep LOW (long) **into an HVN from above** | Best | Trapped shorts at HVN + chip support = strong bounce |
| Sweep into open space (no HVN within ~0.5%) | OK | Sweep works but no amplification |
| Sweep HIGH **already above ALL HVNs** | **Skip** | Chasing into open air, no resistance to fade |
| Sweep LOW **already below ALL HVNs** | **Skip** | Same in reverse |

### 3. POC vs HVN — different roles in your trade

POC and non-POC HVNs sound similar but behave very differently. Don't trade them the same way.

**POC is a TARGET, not an entry.** It's the equilibrium price — the single price where buyers and sellers agreed most often. That means:
- Price isn't extended at POC → no mean-reversion edge to capture
- Two-sided book activity = chop → stops get whipsawed
- The most aggressively defended level → rejection more likely than break

**Non-POC HVNs are valid ENTRIES**, because they represent one-sided positioning (longs trapped above, or shorts trapped below). Reactions there are cleaner.

Direction-by-direction:

| Approach | At POC | At a non-POC HVN |
|---|---|---|
| LONG approaching from below | **Bad** — fading into supply | OK if HVN is acting as resistance for shorts (you're catching the rejection) |
| LONG approaching from above | OK but barely extended | **Best** — clean chip-support bounce |
| SHORT approaching from below | OK (rejection setup) | **Best** — clean chip-resistance rejection |
| SHORT approaching from above | **Bad** — fading into demand | OK if HVN is acting as support that's breaking |

**Where to actually open positions:**

| Zone | Why |
|---|---|
| At a **non-POC HVN** with directional signal aligned | Strong S/R with cleaner reaction than POC's chop |
| In the **LVN gap** between two HVNs | Price moves *fast* through low-volume zones — momentum favorable |
| At a **sweep level distant from POC** | Clean runway toward POC as TP target |
| Just past a **sweep, near a non-POC HVN edge** | Classic smart-money reversal — what the engine is built to detect |

**How to use POC:**
- Long from oversold (below chip zone) → TP1 at first HVN below POC, full exit at POC
- Short from overbought (above chip zone) → TP1 at first HVN above POC, full exit at POC
- Multiple bars clustering AT POC → expect range chop, **disengage mean-reversion entries** until price breaks one side decisively

### 4. Entry refinement

If the engine's entry sits within 0.2% of an HVN, **use the HVN price as your limit** instead. More resting orders exist there → higher fill probability, tighter slippage, cleaner stop placement.

### 5. Stop placement — never inside an HVN

Chip zones produce bounces. A stop inside one gets whipsawed by the noise it generates. Always place stops *past* the HVN by a buffer (0.5×ATR works). If the engine's computed stop lands inside an HVN, manually widen it past the node — costs a few R per losing trade, but win rate jumps noticeably.

### 6. Target / take-profit

Best TP logic: **next HVN in the trade's direction.**

- TP1 = nearest HVN beyond entry
- TP2 = the HVN after that, or POC if you're trading toward it
- If price approaches POC, trim or exit — heaviest reaction zone

The engine's default 1R/2R targets are mechanical and chip-zone-blind. If the next HVN sits at 0.7R, take partial there instead of grinding to 1R and risking a bounce.

### 7. Position sizing modifier

Where HVN really saves money:

| Setup vs HVN context | Risk size |
|---|---|
| Score ≥ 3, sweep-anchored, reacting AT an HVN with aligned direction | **Full size (1% risk)** |
| Score ≥ 3, sweep-anchored, HVN context neutral | Default (0.5% risk) |
| Score ≥ 3, sweep-anchored, but fighting chip-zone overhead | **Quarter size (0.25%) or skip** |
| Score = 2 (borderline) with strong HVN tailwind | Promote to half size (0.5%) |

This is the discretionary overlay backtests can't capture. HVN doesn't vote, but it scales conviction.

### 8. Trade duration estimate

Gap to next HVN ≈ how long the trade will take.

- Wide gap (>3×ATR) → patient management, expect multi-bar move
- Tight gap (<1×ATR) → quick reaction expected, manage actively

### Worked example — Silver 15m LONG (real signal)

Snapshot when the signal fired:
- Entry 75.3070, stop 75.1972 (risk 0.1098), TP1 75.4168, TP2 75.5265
- HVN nodes: 75.72, 75.99, 76.25, **75.46**, 76.52

Reading it:
- Entry is **below** the nearest HVN at 75.46 → long is bouncing *into* a chip zone (target-rich) ✓
- POC at 75.72 sits well above → clean runway up to 75.46, then chip resistance kicks in
- Stop at 75.1972 is below all HVNs → clean placement, no whipsaw risk ✓
- Natural TP refinement: **exit at 75.46** (the HVN) instead of waiting for engine's TP2 at 75.5265 — the HVN will likely react first

Verdict: **take it full size, exit at HVN instead of mechanical TP2.**
