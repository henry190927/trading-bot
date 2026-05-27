package backtest

import (
	"fmt"
	"math"
	"time"

	"myFirstGo/trading-bot/dxy"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
)

// Options configures a backtest run. Zero values mean "no fees, no filter".
type Options struct {
	// FeeBpsRoundTrip is the round-trip fee in basis points (1 bp = 0.01%).
	// BingX perp default: 2bp maker + 2bp maker = 4 if both legs limit;
	// 2bp maker + 5bp taker = 7 if exit is taker (stop/timeout); 10 if both taker.
	// 6 is a sensible all-in default.
	FeeBpsRoundTrip float64

	// SweepOnly skips trades whose Plan.Anchor isn't a liquidity sweep.
	// BOLL still contributes to the confluence vote at the engine level,
	// but no BOLL/fib-anchored entries get simulated.
	SweepOnly bool

	// DXYCandles is optional historical DXY data. When provided, XAU/XAG
	// signals are veto'd when fighting the live USD trend (long into
	// strengthening dollar, short into weakening dollar). Pass nil to
	// disable the macro veto (e.g. for crypto-only backtests).
	DXYCandles []market.Candle
}

type Trade struct {
	Symbol     market.Symbol
	Side       signal.Side
	Score      int
	SignaledAt time.Time
	Entry      float64
	Stop       float64
	Exit       float64
	ExitedAt   time.Time
	RGross     float64 // R before fees
	FeeR       float64 // fee impact in R units (always positive)
	R          float64 // RGross - FeeR
	Outcome    string  // "stop" | "tp2" | "timeout" | "no-fill"
	Anchor     string
}

type Result struct {
	Symbol     market.Symbol
	Timeframe  market.Timeframe
	Threshold  int
	Opts       Options
	Trades     []Trade
	NumSignals int
	WinRate    float64
	AvgR       float64 // net of fees
	AvgRGross  float64
	TotalR     float64
	TotalGross float64
	MaxDDR     float64
	BestR      float64
	WorstR     float64
}

// Run replays Signal.Evaluate over candles and simulates every score>=threshold
// setup. Fees and sweep-only filter applied via Options.
//
// `biasCandles` is the optional higher-timeframe series for MTF filtering.
// Pass nil to disable. Length need not match `candles`; bias for each base
// candle is taken from the most recently closed bias candle, so there is no
// look-ahead leakage.
//
// Known limitations:
//   - Limit fills assumed perfect (no slippage, no queue position)
//   - TP1 not modeled separately — only TP2 vs Stop vs timeout
//   - Funding cost on open positions not modeled (small at intraday horizons)
//   - Signals are non-overlapping: while a trade is open, new signals are skipped
func Run(sym market.Symbol, tf market.Timeframe, candles []market.Candle, biasCandles []market.Candle, threshold, maxHoldBars int, opts Options) Result {
	res := Result{Symbol: sym, Timeframe: tf, Threshold: threshold, Opts: opts}
	if len(candles) < 120 {
		return res
	}
	biases := precomputeBiases(candles, biasCandles)
	dxyTrends := precomputeDXYTrends(candles, opts.DXYCandles)
	const warmup = 100
	openTradeUntil := -1

	for i := warmup; i < len(candles)-1; i++ {
		if i <= openTradeUntil {
			continue
		}
		slice := candles[:i+1]
		sig := signal.Evaluate(signal.Inputs{
			Symbol:    sym,
			Timeframe: tf,
			Candles:   slice,
			Bias:      biases[i],
			DXYTrend:  dxyTrends[i],
		})
		if sig.Side == signal.Flat || sig.Score < threshold || sig.Plan.Entry == 0 {
			continue
		}
		if opts.SweepOnly && !sig.Plan.IsSweepAnchored() {
			continue
		}
		res.NumSignals++
		end := i + 1 + maxHoldBars
		if end > len(candles) {
			end = len(candles)
		}
		tr, lastIdx := simulate(sym, sig, candles[i+1:end], opts.FeeBpsRoundTrip)
		if tr == nil {
			continue
		}
		tr.SignaledAt = candles[i].CloseTime
		res.Trades = append(res.Trades, *tr)
		openTradeUntil = i + 1 + lastIdx
	}
	computeStats(&res)
	return res
}

// precomputeBiases returns one signal.Side per base candle. For each base
// candle at time t, it looks up the latest biasCandle whose CloseTime <= t,
// then reads the sign of MACD histogram at that biasCandle's index.
// Returns a slice of Flat values (no filter) when biasCandles is nil/empty
// or insufficient.
func precomputeBiases(base, bias []market.Candle) []signal.Side {
	out := make([]signal.Side, len(base))
	if len(bias) < 35 {
		return out
	}
	biasCloses := market.Closes(bias)
	macd := indicator.MACD(biasCloses, 12, 26, 9)

	bi := 0
	for ci, c := range base {
		for bi+1 < len(bias) && !bias[bi+1].CloseTime.After(c.OpenTime) {
			bi++
		}
		if bias[bi].CloseTime.After(c.OpenTime) {
			continue // no closed bias bar yet
		}
		h := macd[bi].Histogram
		switch {
		case h > 0:
			out[ci] = signal.Long
		case h < 0:
			out[ci] = signal.Short
		}
	}
	return out
}

// precomputeDXYTrends returns one dxy.Trend per base candle, aligned so the
// trend at index i is what would have been observable at base[i].OpenTime
// (no look-ahead). Returns all-Flat when dxyCandles is nil/empty.
func precomputeDXYTrends(base, dxyCandles []market.Candle) []dxy.Trend {
	out := make([]dxy.Trend, len(base))
	if len(dxyCandles) < 25 {
		return out
	}
	di := 0
	for ci, c := range base {
		for di+1 < len(dxyCandles) && !dxyCandles[di+1].OpenTime.After(c.OpenTime) {
			di++
		}
		if dxyCandles[di].OpenTime.After(c.OpenTime) || di < 24 {
			continue
		}
		out[ci] = dxy.Classify(dxyCandles[:di+1])
	}
	return out
}

func simulate(sym market.Symbol, sig signal.Signal, future []market.Candle, feeBps float64) (*Trade, int) {
	plan := sig.Plan
	risk := plan.Risk()
	if risk == 0 || len(plan.TakeProfit) < 2 {
		return nil, 0
	}
	tp := plan.TakeProfit[len(plan.TakeProfit)-1]

	// Fee impact in R units. Notional per 1R of risk = entry / risk_price.
	// Round-trip fee in account-currency = (feeBps/10000) * 2 * notional? No:
	// feeBps is already round-trip, so fee = (feeBps/10000) * notional.
	// In R units: fee_R = fee / account_risk = (feeBps/10000) * entry / risk.
	feeR := (feeBps / 10000.0) * plan.Entry / risk

	mkTrade := func(rGross float64, exit float64, exitTime time.Time, outcome string) *Trade {
		return &Trade{
			Symbol: sym, Side: sig.Side, Score: sig.Score,
			Entry: plan.Entry, Stop: plan.StopLoss, Exit: exit,
			ExitedAt: exitTime, RGross: rGross, FeeR: feeR,
			R: rGross - feeR, Outcome: outcome, Anchor: plan.Anchor,
		}
	}

	filled := false
	var fillIdx int
	for i, c := range future {
		if !filled {
			if c.Low <= plan.Entry && plan.Entry <= c.High {
				filled = true
				fillIdx = i
			}
			continue
		}
		if sig.Side == signal.Long {
			if c.Low <= plan.StopLoss {
				return mkTrade(-1, plan.StopLoss, c.CloseTime, "stop"), i
			}
			if c.High >= tp {
				return mkTrade(2, tp, c.CloseTime, "tp2"), i
			}
		} else {
			if c.High >= plan.StopLoss {
				return mkTrade(-1, plan.StopLoss, c.CloseTime, "stop"), i
			}
			if c.Low <= tp {
				return mkTrade(2, tp, c.CloseTime, "tp2"), i
			}
		}
	}
	if !filled {
		// No fill, no fee — order never executed.
		return &Trade{
			Symbol: sym, Side: sig.Side, Score: sig.Score,
			Entry: plan.Entry, Stop: plan.StopLoss,
			Outcome: "no-fill", Anchor: plan.Anchor,
		}, len(future) - 1
	}
	last := future[len(future)-1]
	var pnl float64
	if sig.Side == signal.Long {
		pnl = last.Close - plan.Entry
	} else {
		pnl = plan.Entry - last.Close
	}
	return mkTrade(pnl/risk, last.Close, last.CloseTime, "timeout"), len(future) - 1 - fillIdx
}

func computeStats(res *Result) {
	if len(res.Trades) == 0 {
		return
	}
	var wins int
	var cum, peak, dd float64
	res.BestR = -math.MaxFloat64
	res.WorstR = math.MaxFloat64
	filledN := 0
	for _, t := range res.Trades {
		if t.Outcome == "no-fill" {
			continue
		}
		filledN++
		res.TotalR += t.R
		res.TotalGross += t.RGross
		if t.R > 0 {
			wins++
		}
		if t.R > res.BestR {
			res.BestR = t.R
		}
		if t.R < res.WorstR {
			res.WorstR = t.R
		}
		cum += t.R
		if cum > peak {
			peak = cum
		}
		if peak-cum > dd {
			dd = peak - cum
		}
	}
	if filledN == 0 {
		return
	}
	res.WinRate = float64(wins) / float64(filledN)
	res.AvgR = res.TotalR / float64(filledN)
	res.AvgRGross = res.TotalGross / float64(filledN)
	res.MaxDDR = dd
}

func (r Result) Summary() string {
	filter := "all"
	if r.Opts.SweepOnly {
		filter = "sweep-only"
	}
	return fmt.Sprintf("%s %s [%s, fee=%.1fbp] | n=%d sig, %d filled | WR=%.1f%% | grossR=%+.2f netR=%+.2f avgNet=%+.2f maxDD=%.2fR | best=%+.2f worst=%+.2f",
		r.Symbol, r.Timeframe, filter, r.Opts.FeeBpsRoundTrip,
		r.NumSignals, len(r.Trades),
		r.WinRate*100, r.TotalGross, r.TotalR, r.AvgR, r.MaxDDR, r.BestR, r.WorstR)
}
