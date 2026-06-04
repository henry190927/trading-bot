package backtest

import (
	"fmt"
	"math"
	"sort"
	"time"

	"myFirstGo/trading-bot/dxy"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/market"
	"myFirstGo/trading-bot/signal"
	"myFirstGo/trading-bot/validator"
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

	// StopBufferR widens the stop by this fraction of the original
	// risk distance. E.g. 0.3 = stop moves 0.3R further from entry.
	// Entry unchanged → R risk per trade grows; TPs at 1R/2R from
	// entry are re-derived against the NEW (wider) risk. Default 0.
	StopBufferR float64

	// SlideOffsetPct slides BOTH entry and stop in the side's
	// "away" direction by this fraction of entry. Risk distance
	// unchanged (same R). Lets the typical stop-hunt wick play out
	// before filling, so the stop sits past the cluster instead of
	// at it. Default 0. E.g. 0.002 = 0.2% slide. For LONG entry/
	// stop move DOWN; for SHORT they move UP.
	SlideOffsetPct float64

	// ReplayValidator runs validator.Validate on each emitted signal
	// and records the resulting Total + Verdict on the Trade. Used
	// to A/B the predictive-correlation of validator weights: does
	// "/10 says STRONG TAKE" actually predict winners? Adds ~50ms
	// per signal but doesn't change strategy.
	ReplayValidator bool
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

	// Stop-hunt diagnostics (populated only when Outcome == "stop").
	// WickPastStop = how far the stop-out bar wicked past the stop in
	// price units (positive). WickPastStopR = same in R units (e.g.
	// 0.15 means price went 15% of one R past the stop). Reclaimed =
	// did price come back through entry within ReclaimWindow bars?
	// ReclaimBars = bars to reclaim (1-N) or 0 if never within window.
	WickPastStop   float64
	WickPastStopR  float64
	Reclaimed      bool
	ReclaimBars    int

	// Validator-replay diagnostic (populated only when ReplayValidator
	// option is on). Records what validator.Validate would have said
	// about this signal at the moment it fired. Used to A/B whether
	// validator-weight changes improve predictive correlation with R.
	ValidatorScore   float64
	ValidatorVerdict string
}

// ReclaimWindow defines how many bars after a stop-out we look for the
// price to come back through the entry. 6 bars on 1h = 6 hours — long
// enough to catch a "swept then reverted" pattern, short enough to
// rule out coincidental retraces hours later.
const ReclaimWindow = 6

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

	// Stop-hunt diagnostics: how often does price stop us out and
	// then reverse through entry within ReclaimWindow bars?
	StopHits         int     // total trades that hit stop
	StopHitsReclaim  int     // of those, how many later reclaimed entry
	StopReclaimPct   float64 // 100 * StopHitsReclaim / StopHits
	MedWickR         float64 // median wick depth past stop (in R units)
	WickBucket005    int     // 0    – 0.05R past stop
	WickBucket010    int     // 0.05 – 0.10R
	WickBucket025    int     // 0.10 – 0.25R
	WickBucket050    int     // 0.25 – 0.50R
	WickBucketBig    int     // > 0.50R
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
		// Apply stop / entry transforms BEFORE simulate so fill checks
		// and stop checks use the new levels. Order matters: slide
		// first (moves entry+stop together, keeping R), then buffer
		// (widens stop relative to whatever entry we ended up with).
		if opts.SlideOffsetPct > 0 || opts.StopBufferR > 0 {
			applyStopVariants(&sig, opts)
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
		// Validator-replay diagnostic: re-validate the signal at the
		// moment it fired (using the same candle slice the engine saw)
		// so we can later A/B whether validator weights predict outcome.
		if opts.ReplayValidator {
			vr := validator.Validate(sym, tf, sig.Side, sig.Plan.Entry, opts.FeeBpsRoundTrip, slice)
			tr.ValidatorScore = vr.Total
			tr.ValidatorVerdict = vr.Verdict
		}
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

// applyStopVariants mutates sig.Plan in place to implement the buffered-
// stop and/or sliding-entry experiments. Slide is applied first (moves
// entry and stop together, keeping R); buffer then widens the stop
// further from the (possibly slid) entry, expanding R per trade.
// TakeProfit levels are re-derived from the final (entry, risk) so the
// 1R / 2R ratios are preserved in the new geometry.
func applyStopVariants(sig *signal.Signal, opts Options) {
	plan := &sig.Plan
	if opts.SlideOffsetPct > 0 {
		delta := plan.Entry * opts.SlideOffsetPct
		switch sig.Side {
		case signal.Long:
			plan.Entry -= delta
			plan.StopLoss -= delta
		case signal.Short:
			plan.Entry += delta
			plan.StopLoss += delta
		}
	}
	if opts.StopBufferR > 0 {
		risk := plan.StopLoss - plan.Entry
		if risk < 0 {
			risk = -risk
		}
		extra := risk * opts.StopBufferR
		switch sig.Side {
		case signal.Long:
			plan.StopLoss -= extra
		case signal.Short:
			plan.StopLoss += extra
		}
	}
	// Re-derive TPs at 1R / 2R from the final entry, since either
	// transform may have changed entry and/or risk.
	risk := plan.StopLoss - plan.Entry
	if risk < 0 {
		risk = -risk
	}
	if risk > 0 {
		switch sig.Side {
		case signal.Long:
			plan.TakeProfit = []float64{plan.Entry + risk, plan.Entry + 2*risk}
		case signal.Short:
			plan.TakeProfit = []float64{plan.Entry - risk, plan.Entry - 2*risk}
		}
	}
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
				t := mkTrade(-1, plan.StopLoss, c.CloseTime, "stop")
				t.WickPastStop = plan.StopLoss - c.Low
				if risk > 0 {
					t.WickPastStopR = t.WickPastStop / risk
				}
				// Look ahead within ReclaimWindow for price to climb
				// back through the entry — "swept then reverted."
				for j := i + 1; j < len(future) && j-i <= ReclaimWindow; j++ {
					if future[j].High >= plan.Entry {
						t.Reclaimed = true
						t.ReclaimBars = j - i
						break
					}
				}
				return t, i
			}
			if c.High >= tp {
				return mkTrade(2, tp, c.CloseTime, "tp2"), i
			}
		} else {
			if c.High >= plan.StopLoss {
				t := mkTrade(-1, plan.StopLoss, c.CloseTime, "stop")
				t.WickPastStop = c.High - plan.StopLoss
				if risk > 0 {
					t.WickPastStopR = t.WickPastStop / risk
				}
				for j := i + 1; j < len(future) && j-i <= ReclaimWindow; j++ {
					if future[j].Low <= plan.Entry {
						t.Reclaimed = true
						t.ReclaimBars = j - i
						break
					}
				}
				return t, i
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

	// Stop-hunt diagnostics — aggregate over only the "stop" trades.
	var wicks []float64
	for _, t := range res.Trades {
		if t.Outcome != "stop" {
			continue
		}
		res.StopHits++
		if t.Reclaimed {
			res.StopHitsReclaim++
		}
		wicks = append(wicks, t.WickPastStopR)
		switch {
		case t.WickPastStopR < 0.05:
			res.WickBucket005++
		case t.WickPastStopR < 0.10:
			res.WickBucket010++
		case t.WickPastStopR < 0.25:
			res.WickBucket025++
		case t.WickPastStopR < 0.50:
			res.WickBucket050++
		default:
			res.WickBucketBig++
		}
	}
	if res.StopHits > 0 {
		res.StopReclaimPct = 100 * float64(res.StopHitsReclaim) / float64(res.StopHits)
		// median wick depth
		sortedWicks := append([]float64(nil), wicks...)
		sort.Float64s(sortedWicks)
		mid := len(sortedWicks) / 2
		if len(sortedWicks)%2 == 0 {
			res.MedWickR = (sortedWicks[mid-1] + sortedWicks[mid]) / 2
		} else {
			res.MedWickR = sortedWicks[mid]
		}
	}
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

// ValidatorReplaySummary buckets the run's trades by validator score
// band and computes the realized-R within each. A useful weighting
// scheme should produce monotonic-ish R across bands: STRONG TAKE
// trades should outperform AVOID trades by a wide margin. If the
// bands all sit near the global avgR, the validator isn't adding
// information; if the spread is wide, the weights are predictive.
func (r Result) ValidatorReplaySummary() string {
	bands := []struct {
		label  string
		lo, hi float64
	}{
		{"STRONG (≥8)", 8, 11},
		{"TAKE (6-8)", 6, 8},
		{"NEUTRAL (4-6)", 4, 6},
		{"WEAK (2-4)", 2, 4},
		{"AVOID (<2)", -1, 2},
	}
	type band struct {
		n     int
		totR  float64
		wins  int
	}
	stats := make(map[string]*band)
	any := false
	for _, t := range r.Trades {
		if t.Outcome == "no-fill" {
			continue
		}
		if t.ValidatorScore == 0 && t.ValidatorVerdict == "" {
			continue // replay wasn't enabled or this trade has no score
		}
		any = true
		for _, b := range bands {
			if t.ValidatorScore >= b.lo && t.ValidatorScore < b.hi {
				s, ok := stats[b.label]
				if !ok {
					s = &band{}
					stats[b.label] = s
				}
				s.n++
				s.totR += t.R
				if t.R > 0 {
					s.wins++
				}
				break
			}
		}
	}
	if !any {
		return fmt.Sprintf("%s %s | validator-replay: not run (pass --replay-validator)", r.Symbol, r.Timeframe)
	}
	parts := fmt.Sprintf("%s %s | validator-replay bands:", r.Symbol, r.Timeframe)
	for _, b := range bands {
		s := stats[b.label]
		if s == nil || s.n == 0 {
			parts += fmt.Sprintf("\n    %-15s n=0", b.label)
			continue
		}
		wr := 100 * float64(s.wins) / float64(s.n)
		avg := s.totR / float64(s.n)
		parts += fmt.Sprintf("\n    %-15s n=%-3d WR=%5.1f%% avgR=%+5.3f totalR=%+6.2f", b.label, s.n, wr, avg, s.totR)
	}
	return parts
}

// StopHuntSummary describes how often stops were swept and reverted —
// the diagnostic the user asked for after observing trade #8 wick past
// the stop then pull back. A reclaim rate above ~25% strongly suggests
// a buffered-stop A/B is worth running.
func (r Result) StopHuntSummary() string {
	if r.StopHits == 0 {
		return fmt.Sprintf("%s %s | stop-hunt: no stop-outs to analyze", r.Symbol, r.Timeframe)
	}
	return fmt.Sprintf("%s %s | stop-hunt: %d stops, %d reclaimed entry within %d bars (%.1f%%) · median wick past stop %.2fR · wick buckets <0.05R=%d <0.10R=%d <0.25R=%d <0.50R=%d ≥0.50R=%d",
		r.Symbol, r.Timeframe, r.StopHits, r.StopHitsReclaim, ReclaimWindow,
		r.StopReclaimPct, r.MedWickR,
		r.WickBucket005, r.WickBucket010, r.WickBucket025, r.WickBucket050, r.WickBucketBig)
}
