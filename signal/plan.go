package signal

import (
	"fmt"
	"strings"

	"myFirstGo/trading/analyzer"
	"myFirstGo/trading/indicator"
	"myFirstGo/trading/market"
)

type OrderType int

const (
	OrderLimit OrderType = iota
	OrderMarket
)

func (o OrderType) String() string {
	if o == OrderMarket {
		return "MARKET"
	}
	return "LIMIT"
}

// Plan is the concrete trade instruction: where to enter, where to be wrong,
// where to take profit. All prices are in quote currency (USDT).
//
// Anchor describes which signal produced the entry level — useful for
// post-trade attribution.
type Plan struct {
	OrderType  OrderType
	Entry      float64
	StopLoss   float64
	TakeProfit []float64
	RR         []float64 // R-multiple of each TP relative to risk
	Anchor     string    // what the entry is keyed off ("sweep low @ 4535.74", etc.)
	Note       string    // human-readable caveat
}

func (p Plan) Risk() float64 {
	d := p.Entry - p.StopLoss
	if d < 0 {
		d = -d
	}
	return d
}

// IsSweepAnchored reports whether the entry was keyed to a liquidity sweep.
// Sweep-anchored entries are empirically the highest-quality setups; the
// backtester and live monitor both expose a "sweep-only" filter.
func (p Plan) IsSweepAnchored() bool {
	return strings.HasPrefix(p.Anchor, "sweep")
}

// BuildPlan turns a directional Signal into an executable Plan. It picks the
// best entry anchor in this priority order:
//  1. A liquidity sweep on the most recent candle (highest-quality intraday entry)
//  2. Fib 0.618 level
//  3. Bollinger band touch
//  4. Fallback: market at current price (flagged in Note)
//
// StopATRMul is the multiple of ATR(14) added beyond the anchor to set the
// stop. Wider stops increase win rate and shrink fee-in-R-units, at the cost
// of larger absolute risk per trade. Empirically 1.5 is the sweet spot for
// 1h intraday on perpetuals — 0.3 was too tight and made fees dominate.
var StopATRMul = 1.5

// Stops are sized to StopATRMul * ATR(14) beyond the anchor. TP1=1R, TP2=2R.
func BuildPlan(sig Signal, candles []market.Candle, sweeps []analyzer.LiquiditySweep) Plan {
	if sig.Side == Flat || len(candles) < 30 {
		return Plan{}
	}
	atr := indicator.ATR(candles, 14)
	last := len(candles) - 1
	a := atr[last]
	if a <= 0 {
		return Plan{}
	}

	plan := Plan{OrderType: OrderLimit}

	// 1. Recent sweep takes priority — limit at the swept level.
	for _, sw := range sweeps {
		if sw.SweepIdx != last {
			continue
		}
		if sig.Side == Long && sw.Side == analyzer.SweepLow {
			plan.Entry = sw.Level
			plan.StopLoss = candles[last].Low - StopATRMul*a
			plan.Anchor = fmt.Sprintf("sweep low @ %.4f", sw.Level)
			return finalize(plan, sig, candles, a)
		}
		if sig.Side == Short && sw.Side == analyzer.SweepHigh {
			plan.Entry = sw.Level
			plan.StopLoss = candles[last].High + StopATRMul*a
			plan.Anchor = fmt.Sprintf("sweep high @ %.4f", sw.Level)
			return finalize(plan, sig, candles, a)
		}
	}

	// 2. Fib 0.618.
	// If price has already crossed past the level in the trade's direction
	// (long: price below level), a "limit at level" would fill immediately
	// at market — so we use market + current price instead and keep the
	// level only as the anchor label.
	price := candles[last].Close
	for _, lvl := range sig.Fib.Levels {
		if lvl.Ratio != 0.618 {
			continue
		}
		if sig.Side == Long && sig.Fib.Uptrend {
			entry, ot := pickEntry(Long, lvl.Price, price)
			plan.Entry = entry
			plan.OrderType = ot
			plan.StopLoss = entry - StopATRMul*a
			plan.Anchor = fmt.Sprintf("fib 0.618 @ %.4f (uptrend)", lvl.Price)
			return finalize(plan, sig, candles, a)
		}
		if sig.Side == Short && !sig.Fib.Uptrend {
			entry, ot := pickEntry(Short, lvl.Price, price)
			plan.Entry = entry
			plan.OrderType = ot
			plan.StopLoss = entry + StopATRMul*a
			plan.Anchor = fmt.Sprintf("fib 0.618 @ %.4f (downtrend)", lvl.Price)
			return finalize(plan, sig, candles, a)
		}
	}

	// 3. Bollinger band touch.
	// BOLL fires when price has already pierced the band — by definition
	// "limit at band" would be a buy-above-market (long) or sell-below-market
	// (short), i.e. an effective market order. We use Market + current price
	// for an honest plan, and ATR-from-entry for a consistent stop distance.
	boll := indicator.Bollinger(market.Closes(candles), 20, 2)
	b := boll[last]
	if b.Lower != 0 {
		if sig.Side == Long {
			plan.Entry = price
			plan.OrderType = OrderMarket
			plan.StopLoss = price - StopATRMul*a
			plan.Anchor = fmt.Sprintf("BOLL lower @ %.4f", b.Lower)
			return finalize(plan, sig, candles, a)
		}
		if sig.Side == Short {
			plan.Entry = price
			plan.OrderType = OrderMarket
			plan.StopLoss = price + StopATRMul*a
			plan.Anchor = fmt.Sprintf("BOLL upper @ %.4f", b.Upper)
			return finalize(plan, sig, candles, a)
		}
	}

	// 4. Market fallback — explicitly flagged as suboptimal.
	plan.OrderType = OrderMarket
	plan.Entry = candles[last].Close
	if sig.Side == Long {
		plan.StopLoss = plan.Entry - StopATRMul*a
	} else {
		plan.StopLoss = plan.Entry + StopATRMul*a
	}
	plan.Anchor = "market (no clean anchor)"
	plan.Note = "no high-quality entry level; consider skipping"
	return finalize(plan, sig, candles, a)
}

// pickEntry returns the appropriate entry price and order type for an
// anchor-based mean-reversion plan. If the anchor level is "behind" current
// price (a long with anchor above market, or a short with anchor below),
// a literal limit at the anchor would fill instantly — so we use Market +
// current price. Otherwise we honor the anchor as a Limit waiting for
// price to come to us.
func pickEntry(side Side, anchorLevel, price float64) (float64, OrderType) {
	if side == Long {
		if price <= anchorLevel {
			return price, OrderMarket
		}
		return anchorLevel, OrderLimit
	}
	if price >= anchorLevel {
		return price, OrderMarket
	}
	return anchorLevel, OrderLimit
}

func finalize(p Plan, sig Signal, candles []market.Candle, atr float64) Plan {
	// Stop refinement: push past nearby HVN / equal-highs/lows clusters so
	// we don't get swept out by obvious stop-hunt flows. See stop_refine.go.
	if newStop, note := refineStopLoss(sig.Side, p.StopLoss, atr, candles, sig.VP); newStop != p.StopLoss {
		p.StopLoss = newStop
		if note != "" {
			if p.Note != "" {
				p.Note += "; " + note
			} else {
				p.Note = note
			}
		}
	}
	risk := p.Risk()
	if risk == 0 {
		return p
	}
	if sig.Side == Long {
		p.TakeProfit = []float64{p.Entry + risk, p.Entry + 2*risk}
	} else {
		p.TakeProfit = []float64{p.Entry - risk, p.Entry - 2*risk}
	}
	p.RR = []float64{1, 2}
	return p
}
