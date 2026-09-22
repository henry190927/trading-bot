// Package autostrat holds the auto-executor's per-rule strategy evaluators,
// extracted from cmd/monitor so BOTH the live paper daemon AND the web
// "current setups" scanner run the SAME trigger logic (single source of truth).
// Each Eval* checks the last CLOSED bar and returns a Trigger describing the
// order the strategy would place right now (or Fire=false if nothing sets up).
package autostrat

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/indicator"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/signal"
	"github.com/henry190927/trading-bot/validator"
)

// Trigger is what a strategy would do on the current closed bar.
type Trigger struct {
	Fire   bool
	Side   string // "long" | "short"
	Entry  float64
	Stop   float64
	TP     float64
	Market bool // true = marketable (fills at fire); false = resting limit
	Why    string
}

// FireScore100 computes the validator structural-fit score (rescaled /100) for
// an entry — a uniform quality number across ALL strategies. Best-effort: 0.
func FireScore100(ctx context.Context, client *bingx.Client, sym market.Symbol, tf, sideStr string, entry float64) float64 {
	cs, err := client.Klines(ctx, sym, market.Timeframe(tf), 300)
	if err != nil || len(cs) < 50 {
		return 0
	}
	side := signal.Long
	if sideStr == "short" {
		side = signal.Short
	}
	r := validator.Validate(sym, market.Timeframe(tf), side, entry, 6.0, cs)
	return r.Total * 10
}

// EvalAutoTrigger dispatches on strategy — the single entry point used by both
// the monitor and the scanner.
func EvalAutoTrigger(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	switch r.Strategy {
	case "range-edge":
		return evalRangeEdge(ctx, client, sym, r)
	case "engine":
		// Whatever strategyFor says this (symbol, TF) is — MR for most,
		// StructMomentum for the allowlisted alts.
		return evalEngine(ctx, client, sym, r, signal.StrategyUnset)
	case "struct-momentum":
		// Explicitly the trend-continuation strategy, allowlist or not.
		//
		// It was already REACHABLE before this, but only by accident: an
		// "engine" rule on a symbol strategyFor happened to agree with. The
		// book had one continuation-capable rule out of seventeen and nothing
		// in autotrade.json said which one — you had to cross-reference the
		// allowlist in Go source to find out it was SOL 1h. A rule names a
		// (symbol, strategy, TF) triple; the strategy should be one of them.
		return evalEngine(ctx, client, sym, r, signal.StrategyStructMomentum)
	case "sweep-reject":
		return evalSweepReject(ctx, client, sym, r)
	case "htf-snr":
		return evalHTFSNR(ctx, client, sym, r)
	default:
		return Trigger{}
	}
}

func evalHTFSNR(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	const (
		strength = 3
		tolFrac  = 0.0020
		bufATR   = 0.25
		rMult    = 2.0
	)
	tf := market.Timeframe(r.TF)
	cs, err := client.Klines(ctx, sym, tf, 300)
	if err != nil || len(cs) < 30 {
		return Trigger{}
	}
	hcs, herr := client.Klines(ctx, sym, EngineBiasTF(tf), 200)
	if herr != nil || len(hcs) < 40 {
		return Trigger{}
	}
	atrs := indicator.ATR(cs, 14)
	if len(atrs) == 0 {
		return Trigger{}
	}
	a := atrs[len(atrs)-1]
	bar := cs[len(cs)-1]
	prev := cs[len(cs)-2]
	sw := signal.FindSwingPoints(hcs, strength, 0)
	wantShort := r.Side == "short" || r.Side == "auto"
	wantLong := r.Side == "long" || r.Side == "auto"
	for _, p := range sw {
		ci := p.Index + strength
		if ci >= len(hcs) {
			ci = len(hcs) - 1
		}
		if !hcs[ci].CloseTime.Before(bar.OpenTime) {
			continue
		}
		tolAbs := p.Price * tolFrac
		if wantShort && p.IsTop && prev.Close < p.Price && bar.High >= p.Price-tolAbs && bar.Close < p.Price {
			stop := bar.High + bufATR*a
			risk := stop - bar.Close
			if risk <= 0 {
				continue
			}
			return Trigger{Fire: true, Side: "short", Entry: bar.Close, Market: true,
				Stop: stop, TP: bar.Close - rMult*risk,
				Why: fmt.Sprintf("HTF-S/R fade: reject %s swing high %.4f", EngineBiasTF(tf), p.Price)}
		}
		if wantLong && !p.IsTop && prev.Close > p.Price && bar.Low <= p.Price+tolAbs && bar.Close > p.Price {
			stop := bar.Low - bufATR*a
			risk := bar.Close - stop
			if risk <= 0 {
				continue
			}
			return Trigger{Fire: true, Side: "long", Entry: bar.Close, Market: true,
				Stop: stop, TP: bar.Close + rMult*risk,
				Why: fmt.Sprintf("HTF-S/R fade: hold %s swing low %.4f", EngineBiasTF(tf), p.Price)}
		}
	}
	return Trigger{}
}

func evalSweepReject(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	const (
		tolFrac = 0.0015
		bufATR  = 0.15
		rMult   = 2.0
	)
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 300)
	if err != nil || len(cs) < 80 {
		return Trigger{}
	}
	atrs := indicator.ATR(cs, 14)
	if len(atrs) == 0 {
		return Trigger{}
	}
	a := atrs[len(atrs)-1]
	bar := cs[len(cs)-1]
	pools := signal.FindLiquidity(cs[:len(cs)-1], 2, 20, tolFrac)
	wantShort := r.Side == "short" || r.Side == "auto"
	wantLong := r.Side == "long" || r.Side == "auto"
	for _, p := range pools {
		if wantShort && p.Kind == signal.EQH && bar.High > p.Hi && bar.Close < p.Lo {
			stop := bar.High + bufATR*a
			risk := stop - bar.Close
			if risk <= 0 {
				continue
			}
			return Trigger{Fire: true, Side: "short", Entry: bar.Close, Market: true,
				Stop: stop, TP: bar.Close - rMult*risk,
				Why: fmt.Sprintf("sweep-reject EQH %.4f (band %.4f-%.4f, %dx)", p.Price, p.Lo, p.Hi, p.Touches)}
		}
		if wantLong && p.Kind == signal.EQL && bar.Low < p.Lo && bar.Close > p.Hi {
			stop := bar.Low - bufATR*a
			risk := bar.Close - stop
			if risk <= 0 {
				continue
			}
			return Trigger{Fire: true, Side: "long", Entry: bar.Close, Market: true,
				Stop: stop, TP: bar.Close + rMult*risk,
				Why: fmt.Sprintf("sweep-reject EQL %.4f (band %.4f-%.4f, %dx)", p.Price, p.Lo, p.Hi, p.Touches)}
		}
	}
	return Trigger{}
}

func autoMinScore() int {
	if v := strings.TrimSpace(os.Getenv("AUTO_MIN_SCORE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func evalEngine(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule, force signal.StrategyKind) Trigger {
	tf := market.Timeframe(r.TF)
	candles, err := client.Klines(ctx, sym, tf, 300)
	if err != nil || len(candles) < 50 {
		return Trigger{}
	}
	bias := signal.Flat
	if bc, berr := client.Klines(ctx, sym, EngineBiasTF(tf), 100); berr == nil {
		bias = signal.Bias(bc)
	}
	sigCtx := signal.Context{}
	var markPrice float64
	if fr, ferr := client.FundingRate(ctx, sym); ferr == nil {
		sigCtx.FundingRate = fr.Rate
		markPrice = fr.MarkPrice
	}
	if oi, oerr := client.OpenInterest(ctx, sym); oerr == nil {
		sigCtx.OpenInterest = oi
	}
	s := signal.Evaluate(signal.Inputs{
		Symbol: sym, Timeframe: tf, Candles: candles, Ctx: sigCtx, Bias: bias, LiveMarkPrice: markPrice,
		ForceStrategy: force,
	})
	if s.Side == signal.Flat || s.Plan.Entry <= 0 || s.Plan.StopLoss <= 0 {
		return Trigger{}
	}
	if s.Score < autoMinScore() {
		return Trigger{}
	}
	side := "long"
	if s.Side == signal.Short {
		side = "short"
	}
	if (r.Side == "long" && side != "long") || (r.Side == "short" && side != "short") {
		return Trigger{}
	}
	tp := s.Plan.Entry
	if len(s.Plan.TakeProfit) > 0 {
		tp = s.Plan.TakeProfit[0]
	}
	anchor := s.Plan.Anchor
	if anchor == "" {
		anchor = signal.StrategyFor(sym, tf).String()
	}
	curPx := candles[len(candles)-1].Close
	if markPrice > 0 {
		curPx = markPrice
	}
	marketable := (side == "long" && s.Plan.Entry >= curPx) || (side == "short" && s.Plan.Entry <= curPx)
	return Trigger{Fire: true, Side: side, Entry: s.Plan.Entry, Stop: s.Plan.StopLoss, TP: tp, Market: marketable,
		Why: fmt.Sprintf("engine score %d — %s", s.Score, anchor)}
}

// EngineBiasTF picks the higher-TF used for directional bias in the engine call.
func EngineBiasTF(tf market.Timeframe) market.Timeframe {
	switch tf {
	case "5m", "15m", "30m", "1h":
		return "4h"
	case "2h", "4h":
		return "1d"
	default:
		return "1d"
	}
}

func evalRangeEdge(ctx context.Context, client *bingx.Client, sym market.Symbol, r autotrade.Rule) Trigger {
	cs, err := client.Klines(ctx, sym, market.Timeframe(r.TF), 250)
	if err != nil || len(cs) < 30 {
		return Trigger{}
	}
	px := cs[len(cs)-1].Close
	n := 24
	if len(cs) < n {
		n = len(cs)
	}
	seg := cs[len(cs)-n:]
	lo, hi := seg[0].Low, seg[0].High
	for _, c := range seg {
		if c.Low < lo {
			lo = c.Low
		}
		if c.High > hi {
			hi = c.High
		}
	}
	if hi <= lo {
		return Trigger{}
	}
	pos := (px - lo) / (hi - lo)
	st := signal.AnalyzeStructure(cs, 2)
	stopBuf := r.StopPct / 100.0
	wantLong := r.Side == "long" || r.Side == "auto"
	wantShort := r.Side == "short" || r.Side == "auto"
	if wantLong && pos <= 0.34 && st.Trend != signal.StructDowntrend {
		return Trigger{Fire: true, Side: "long", Entry: px, Market: true,
			Stop: lo * (1 - stopBuf), TP: hi,
			Why: fmt.Sprintf("box %.4f-%.4f pos %.0f%% bottom-third", lo, hi, pos*100)}
	}
	if wantShort && pos >= 0.66 && st.Trend != signal.StructUptrend {
		return Trigger{Fire: true, Side: "short", Entry: px, Market: true,
			Stop: hi * (1 + stopBuf), TP: lo,
			Why: fmt.Sprintf("box %.4f-%.4f pos %.0f%% top-third", lo, hi, pos*100)}
	}
	return Trigger{}
}
