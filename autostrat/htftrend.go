package autostrat

import (
	"fmt"
	"math"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// HTF trend follower: Donchian breakout entry, opposite-channel trailing exit.
// Design + pre-registration: docs/htf_trend_design.md. NOT wired into
// EvalAutoTrigger / KnownStrategies — it is a backtest candidate (cmd/trendbt).
//
// Unlike every other strategy here it has no take-profit, so
// autotrade.EvaluateFire (fixed stop/TP) cannot score it; SimulateHTFTrend is
// its path-dependent replacement.

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
// StopATR×atr below. Short mirrors at the channel low. TP is 0: the exit is
// the trail (HTFTrendTrailStop), not a target.
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
			Fire: true, Side: "long", Market: true, Entry: bar.Close, Stop: bar.Close - d,
			Why:  fmt.Sprintf("close %.4f broke the %d-bar high %.4f; trail = %d-bar low", bar.Close, p.EntryN, hi, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", hi, p.EntryN)},
		}
	case wantShort && bar.Close < lo:
		return Trigger{
			Fire: true, Side: "short", Market: true, Entry: bar.Close, Stop: bar.Close + d,
			Why:  fmt.Sprintf("close %.4f broke the %d-bar low %.4f; trail = %d-bar high", bar.Close, p.EntryN, lo, p.TrailM),
			Tags: []Tag{tag("htf-trend-break", lo, p.EntryN)},
		}
	}
	return Trigger{}
}

// HTFTrendTrailStop is the stop that applies to bar j+1 of a position whose
// stop during bar j was cur: the opposite M-bar channel ending at closed bar
// j, ratcheted so it never loosens. Reads cs[j-M+1 .. j] only.
func HTFTrendTrailStop(cs []market.Candle, j int, long bool, cur float64, m int) float64 {
	if m < 1 || j < m-1 || j >= len(cs) {
		return cur
	}
	if long {
		lo := math.Inf(1)
		for _, c := range cs[j-m+1 : j+1] {
			lo = math.Min(lo, c.Low)
		}
		return math.Max(cur, lo)
	}
	hi := math.Inf(-1)
	for _, c := range cs[j-m+1 : j+1] {
		hi = math.Max(hi, c.High)
	}
	return math.Min(cur, hi)
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

// runTrail walks one position forward from its signal bar.
func runTrail(cs []market.Candle, i int, side string, entry, stop float64, m int, feeBps float64) TrendTrade {
	long := side == "long"
	risk := math.Abs(entry - stop)
	t := TrendTrade{
		Side: side, EntryIdx: i, EntryTime: cs[i].CloseTime, Entry: entry, InitStop: stop, Risk: risk,
		StopPct: risk / entry * 100, MaxLev: autotrade.MaxLevForStop(risk / entry),
		FeeR: trendFeeR(side, entry, stop, feeBps),
	}
	cur := stop
	exitIdx, exit, open := len(cs)-1, cs[len(cs)-1].Close, true
	for j := i + 1; j < len(cs); j++ {
		// Trail from the bars that have CLOSED before bar j opens (≤ j-1).
		// For j = i+1 that includes the signal bar itself.
		cur = HTFTrendTrailStop(cs, j-1, long, cur, m)
		b := cs[j]
		if long && b.Low <= cur {
			exitIdx, exit, open = j, math.Min(cur, b.Open), false
			break
		}
		if !long && b.High >= cur {
			exitIdx, exit, open = j, math.Max(cur, b.Open), false
			break
		}
	}
	t.ExitIdx, t.ExitTime, t.Exit, t.Open = exitIdx, cs[exitIdx].CloseTime, exit, open
	t.BarsHeld = exitIdx - i
	t.GrossR = t.dir() * (exit - entry) / risk
	t.NetR = t.GrossR - t.FeeR
	return t
}
