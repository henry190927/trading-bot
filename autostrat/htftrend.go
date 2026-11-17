package autostrat

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
)

// HTF trend follower: Donchian breakout entry, opposite-channel trailing exit.
// Design + pre-registration: docs/htf_trend_design.md; executor wiring:
// docs/htf_trend_executor_design.md. Strategies "htf-trend" (4h) and
// "htf-trend-1d" (1d) in EvalAutoTrigger, backtested by cmd/trendbt.
//
// Unlike every other strategy here it has no take-profit, so
// autotrade.EvaluateFire (fixed stop/TP) cannot score it: the backtest uses
// SimulateHTFTrend, a logged fire autotrade.EvaluateTrailFire. Both walk the
// same autotrade.WalkTrail.

// HTFTrendParams are the strategy's settings.
type HTFTrendParams struct {
	EntryN    int     // breakout channel: the N closed bars BEFORE the signal bar
	TrailM    int     // trailing stop: opposite channel of the last M closed bars
	ATRPeriod int     // ATR used for the initial stop
	StopATR   float64 // initial stop distance in ATR = 1R
}

// DefaultHTFTrendParams is pre-registered variant A (4h) and B (1d): the same
// numbers on both TFs.
func DefaultHTFTrendParams() HTFTrendParams {
	return HTFTrendParams{EntryN: 20, TrailM: 10, ATRPeriod: 20, StopATR: 3.0}
}

// HTFTrendTrigger evaluates closed bar i of cs. The channel is cs[i-N:i], so
// bar i cannot break its own range, and nothing after bar i is read.
// atr is ATR(p.ATRPeriod) as of bar i (bar i is closed, so it may include it).
//
// Long: Close[i] > max High of the channel → market long at Close[i], stop
// StopATR×atr below. Short mirrors at the channel low. TP is 0 and TrailM is
// set: the exit is the trail (HTFTrendTrailStop), not a target.
func HTFTrendTrigger(cs []market.Candle, atr float64, i int, wantLong, wantShort bool, p HTFTrendParams) Trigger {
	if p.EntryN < 1 || i < p.EntryN || i >= len(cs) || atr <= 0 || p.StopATR <= 0 {
		return Trigger{}
	}
	hi, lo := math.Inf(-1), math.Inf(1)
	for _, c := range cs[i-p.EntryN : i] {
		hi = math.Max(hi, c.High)
		lo = math.Min(lo, c.Low)
	}
	bar := cs[i]
	d := p.StopATR * atr
	switch {
	case wantLong && bar.Close > hi:
		return Trigger{
			Fire: true, Side: "long", Market: true, Entry: bar.Close, Stop: bar.Close - d, TrailM: p.TrailM, BarTime: bar.CloseTime,
			Why:  fmt.Sprintf("close %.4f broke the %d-bar high %.4f; trail = %d-bar low", bar.Close, p.EntryN, hi, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", hi, p.EntryN)},
		}
	case wantShort && bar.Close < lo:
		return Trigger{
			Fire: true, Side: "short", Market: true, Entry: bar.Close, Stop: bar.Close + d, TrailM: p.TrailM, BarTime: bar.CloseTime,
			Why:  fmt.Sprintf("close %.4f broke the %d-bar low %.4f; trail = %d-bar high", bar.Close, p.EntryN, lo, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", lo, p.EntryN)},
		}
	}
	return Trigger{}
}

// HTFTrendTrailStop is the stop that applies to bar j+1 of a position whose
// stop during bar j was cur: the opposite M-bar channel ending at closed bar
// j, ratcheted so it never loosens. Reads cs[j-M+1 .. j] only.
//
// A thin caller of autotrade.TrailStop, the one implementation the paper
// resolver also runs (docs/htf_trend_executor_design.md §2.2).
func HTFTrendTrailStop(cs []market.Candle, j int, long bool, cur float64, m int) float64 {
	return autotrade.TrailStop(cs, j, long, cur, m)
}

// TrendTrade is one simulated position. All R are relative to the INITIAL
// stop (Risk = |Entry - InitStop|).
type TrendTrade struct {
	Side      string
	EntryIdx  int // signal bar; filled at its close
	EntryTime time.Time
	Entry     float64
	InitStop  float64
	Risk      float64
	ExitIdx   int // bar the stop was hit on; last bar when Open
	ExitTime  time.Time
	Exit      float64
	Open      bool    // still open at the last bar: Exit = last close (marked to market)
	GrossR    float64 // (exit-entry)/risk, signed by side
	FeeR      float64 // round-trip fee in R, as autotrade.WithFee
	NetR      float64 // GrossR - FeeR
	StopPct   float64 // |entry-initStop|/entry, percent
	MaxLev    int     // autotrade.MaxLevForStop of the initial stop
	BarsHeld  int
}

func (t TrendTrade) dir() float64 {
	if t.Side == "short" {
		return -1
	}
	return 1
}

// MTMR is the trade's net R as of the close of bar k: 0 before entry, the fee
// alone on the entry bar (fees are booked at entry), gross mark-to-close minus
// fee while open, NetR from the exit bar on.
func (t TrendTrade) MTMR(cs []market.Candle, k int) float64 {
	switch {
	case k < t.EntryIdx:
		return 0
	case k >= t.ExitIdx:
		return t.NetR
	default:
		return t.dir()*(cs[k].Close-t.Entry)/t.Risk - t.FeeR
	}
}

// trendFeeR charges the round trip through autotrade.WithFee itself, so the
// conversion cannot drift from the one the rest of the book uses. WithFee
// only charges resolved trades, so the status is always a resolved one here;
// an open trade is charged too (as if closed at the last close).
func trendFeeR(side string, entry, stop, feeBps float64) float64 {
	f := autotrade.PaperFire{Side: side, Entry: entry, Stop: stop}
	oc := autotrade.WithFee(f, autotrade.Outcome{Status: autotrade.OutStop, FillPrice: entry}, feeBps)
	return -oc.NetR
}

// SimulateHTFTrend runs the strategy over cs with one position at a time.
// atr[i] must be ATR(p.ATRPeriod) as of closed bar i (0 = not warm, no
// signal). Signals are taken only on bars i >= firstSignal; exits use every
// later bar. Rules (docs/htf_trend_design.md §2):
//
//   - entry at the signal bar's close; the first bar that can stop it is i+1;
//   - the stop for bar j is fixed before bar j opens (initial stop, then the
//     ratcheted M-bar opposite channel from bars ≤ j-1); a long exits when
//     Low[j] ≤ stop at the stop, or at Open[j] if the bar opens through it;
//   - there is no favourable exit, so intrabar ordering cannot help a trade;
//   - after an exit on bar j, the next signal must come on bar ≥ j+1.
func SimulateHTFTrend(cs []market.Candle, atr []float64, firstSignal int, wantLong, wantShort bool, p HTFTrendParams, feeBps float64) []TrendTrade {
	var out []TrendTrade
	if len(atr) != len(cs) {
		return nil
	}
	for i := max(firstSignal, p.EntryN); i < len(cs); i++ {
		tr := HTFTrendTrigger(cs, atr[i], i, wantLong, wantShort, p)
		if !tr.Fire {
			continue
		}
		t := runTrail(cs, i, tr.Side, tr.Entry, tr.Stop, p.TrailM, feeBps)
		out = append(out, t)
		if t.Open {
			break
		}
		i = t.ExitIdx // loop's i++ makes the next signal bar ExitIdx+1
	}
	return out
}

// runTrail walks one position forward from its signal bar: the walk is
// autotrade.WalkTrail (shared with the paper resolver); this adds the fee and
// the backtest's bookkeeping.
func runTrail(cs []market.Candle, i int, side string, entry, stop float64, m int, feeBps float64) TrendTrade {
	risk := math.Abs(entry - stop)
	t := TrendTrade{
		Side: side, EntryIdx: i, EntryTime: cs[i].CloseTime, Entry: entry, InitStop: stop, Risk: risk,
		StopPct: risk / entry * 100, MaxLev: autotrade.MaxLevForStop(risk / entry),
		FeeR: trendFeeR(side, entry, stop, feeBps),
	}
	w := autotrade.WalkTrail(cs, i, side, stop, m)
	t.ExitIdx, t.ExitTime, t.Exit, t.Open = w.ExitIdx, cs[w.ExitIdx].CloseTime, w.Exit, w.Open
	t.BarsHeld = w.ExitIdx - i
	t.GrossR = t.dir() * (w.Exit - entry) / risk
	t.NetR = t.GrossR - t.FeeR
	return t
}

// htfTrendLiveBars is how many closed bars the live trigger reads. ATR(20) is
// Wilder-smoothed from a seed at bar 20 (indicator.ATR), and the seed's weight
// after k more bars is (19/20)^k: at 500 bars it is ~2e-11, so the live stop
// equals the one trendbt computes over the whole history (pinned by
// TestHTFTrendLiveWindowParity). 300 bars would leave ~1e-6.
const htfTrendLiveBars = 500

// evalHTFTrend is the executor/scanner entry: the pre-registered parameters on
// the rule's last closed bar. Refuses a TF the strategy is not registered for
// (autotrade.TrailStrategyTF) rather than run an untested variant.
func evalHTFTrend(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	if r.TF != autotrade.TrailStrategyTF(r.Strategy) {
		return Trigger{}
	}
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), htfTrendLiveBars)
	if err != nil {
		return Trigger{}
	}
	return HTFTrendOnWindow(cs, r)
}

// HTFTrendOnWindow evaluates the newest bar of a closed-bar window — what the
// executor does with the bars its fetch returned. Exported for the offline
// replay of the executor in cmd/trendbt.
func HTFTrendOnWindow(cs []market.Candle, r autotrade.Rule) Trigger {
	p := DefaultHTFTrendParams()
	if len(cs) < max(p.EntryN, p.ATRPeriod)+2 {
		return Trigger{}
	}
	atr := indicator.ATR(cs, p.ATRPeriod)
	if len(atr) != len(cs) {
		return Trigger{}
	}
	i := len(cs) - 1
	return HTFTrendTrigger(cs, atr[i], i, r.Side == "long" || r.Side == "auto", r.Side == "short" || r.Side == "auto", p)
}

// HTFTrendTouchTrigger is the touch-entry variant (docs/htf_trend_entry_variants_design.md
// §3.2, user-adopted for 1d on 2026-11-17): stop-entries rest at the prior
// N-bar channel edges before bar i opens, and this reports whether closed bar
// i filled one. atrPrev is ATR as of bar i-1 — known before bar i opened, so
// the stop distance is the one a resting bracket would have carried.
//
// Fill = the level, or bar i's open when it gapped through. Both edges
// touched in one bar: the level nearer the open filled first; an exact tie is
// no fire. The fire is returned even when bar i also reached the initial
// stop: the resolver scores that as a stop on bar i (Touch), as the backtest
// does, so a same-bar loss is booked rather than silently skipped.
func HTFTrendTouchTrigger(cs []market.Candle, atrPrev float64, i int, wantLong, wantShort bool, p HTFTrendParams) Trigger {
	if p.EntryN < 1 || i < p.EntryN || i >= len(cs) || atrPrev <= 0 || p.StopATR <= 0 {
		return Trigger{}
	}
	hi, lo := math.Inf(-1), math.Inf(1)
	for _, c := range cs[i-p.EntryN : i] {
		hi = math.Max(hi, c.High)
		lo = math.Min(lo, c.Low)
	}
	b := cs[i]
	longHit := wantLong && b.High >= hi
	shortHit := wantShort && b.Low <= lo
	side := ""
	switch {
	case longHit && shortHit:
		dl, ds := math.Max(0, hi-b.Open), math.Max(0, b.Open-lo)
		switch {
		case dl < ds:
			side = "long"
		case ds < dl:
			side = "short"
		}
	case longHit:
		side = "long"
	case shortHit:
		side = "short"
	}
	d := p.StopATR * atrPrev
	switch side {
	case "long":
		e := math.Max(hi, b.Open)
		return Trigger{
			Fire: true, Side: "long", Market: true, Entry: e, Stop: e - d, TrailM: p.TrailM, BarTime: b.CloseTime, Touch: true,
			Why:  fmt.Sprintf("touched the %d-bar high %.4f (fill %.4f); trail = %d-bar low", p.EntryN, hi, e, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", hi, p.EntryN)},
		}
	case "short":
		e := math.Min(lo, b.Open)
		return Trigger{
			Fire: true, Side: "short", Market: true, Entry: e, Stop: e + d, TrailM: p.TrailM, BarTime: b.CloseTime, Touch: true,
			Why:  fmt.Sprintf("touched the %d-bar low %.4f (fill %.4f); trail = %d-bar high", p.EntryN, lo, e, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", lo, p.EntryN)},
		}
	}
	return Trigger{}
}

// SimulateHTFTrendTouch is SimulateHTFTrend with touch entries: one position
// at a time, the next resting orders from the bar after an exit.
func SimulateHTFTrendTouch(cs []market.Candle, atr []float64, firstSignal int, wantLong, wantShort bool, p HTFTrendParams, feeBps float64) []TrendTrade {
	var out []TrendTrade
	if len(atr) != len(cs) {
		return nil
	}
	for i := max(firstSignal, p.EntryN, 1); i < len(cs); i++ {
		t := HTFTrendTouchTrigger(cs, atr[i-1], i, wantLong, wantShort, p)
		if !t.Fire {
			continue
		}
		var tr TrendTrade
		if (t.Side == "long" && cs[i].Low <= t.Stop) || (t.Side == "short" && cs[i].High >= t.Stop) {
			tr = runTrail(cs, i, t.Side, t.Entry, t.Stop, 1, feeBps) // Risk/Fee/StopPct/MaxLev only
			tr.ExitIdx, tr.ExitTime, tr.Exit, tr.Open, tr.BarsHeld = i, cs[i].CloseTime, t.Stop, false, 0
			tr.GrossR = -1
			tr.NetR = tr.GrossR - tr.FeeR
		} else {
			tr = runTrail(cs, i, t.Side, t.Entry, t.Stop, p.TrailM, feeBps)
		}
		out = append(out, tr)
		if tr.Open {
			break
		}
		i = tr.ExitIdx
	}
	return out
}

// evalHTFTrendTouch is the executor entry for htf-trend-1d-touch: the last
// closed bar of the live window, ATR as of the bar before it.
func evalHTFTrendTouch(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	if r.TF != autotrade.TrailStrategyTF(r.Strategy) {
		return Trigger{}
	}
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), htfTrendLiveBars)
	if err != nil {
		return Trigger{}
	}
	return HTFTrendTouchOnWindow(cs, r)
}

// HTFTrendTouchOnWindow evaluates the newest bar of a closed-bar window.
func HTFTrendTouchOnWindow(cs []market.Candle, r autotrade.Rule) Trigger {
	p := DefaultHTFTrendParams()
	if len(cs) < max(p.EntryN, p.ATRPeriod)+2 {
		return Trigger{}
	}
	atr := indicator.ATR(cs, p.ATRPeriod)
	if len(atr) != len(cs) {
		return Trigger{}
	}
	i := len(cs) - 1
	return HTFTrendTouchTrigger(cs, atr[i-1], i, r.Side == "long" || r.Side == "auto", r.Side == "short" || r.Side == "auto", p)
}
