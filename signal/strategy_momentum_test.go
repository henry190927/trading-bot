package signal

import (
	"testing"

	"myFirstGo/trading-bot/market"
)

// Every currently-live symbol must default to the MR engine — StructMomentum
// is opt-in per symbol and none are assigned yet (Phase 1). Locks the
// "zero day-one behavior change" invariant.
func TestStrategyForDefaultsToMR(t *testing.T) {
	for _, s := range []market.Symbol{
		market.BTCUSDT, market.ETHUSDT, market.XAUUSDT, market.XAGUSDT,
		market.SNDKUSDT, market.NVDAUSDT,
	} {
		if got := strategyFor(s); got != StrategyMR {
			t.Errorf("strategyFor(%s) = %v, want StrategyMR", s, got)
		}
	}
}

// The A/B force-switch must default OFF so backtest/live behavior is the MR
// engine unless explicitly enabled.
func TestStructMomentumDisabledByDefault(t *testing.T) {
	if StructMomentumEnabled {
		t.Fatal("StructMomentumEnabled must default to false")
	}
}

// evaluateStructMomentum returns Flat (no side, no plan) when there isn't
// enough history — never panics, never emits a half-formed plan.
func TestStructMomentumFlatOnInsufficientData(t *testing.T) {
	cs := make([]market.Candle, 10) // < smEMASlow+5
	sig := evaluateStructMomentum(Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: cs})
	if sig.Side != Flat {
		t.Errorf("side = %v, want Flat on insufficient data", sig.Side)
	}
	if sig.Plan.Entry != 0 {
		t.Errorf("plan.Entry = %v, want 0 (no plan) on insufficient data", sig.Plan.Entry)
	}
}

// When it DOES emit, the output must satisfy the invariants downstream
// surfaces (dashboard/setups/journal/backtest) rely on: a directional side,
// score>=3 (MIN_SCORE), all-momentum scoring, a bracketed plan with >=2 TPs,
// and stop/target on the correct sides of entry.
func TestStructMomentumOutputInvariants(t *testing.T) {
	// Synthesize a clean uptrend that pulls back — a series that rises in
	// higher-highs/higher-lows then retraces, so AnalyzeStructure sees an
	// up-leg with an active zone the latest close sits inside.
	cs := syntheticUptrendPullback()
	sig := evaluateStructMomentum(Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: cs})
	if sig.Side == Flat {
		t.Skip("synthetic series did not trigger a setup; invariant check needs a firing signal")
	}
	if sig.Score < 3 {
		t.Errorf("score = %d, want >= 3 (MIN_SCORE)", sig.Score)
	}
	if sig.MRScore != 0 || sig.MomentumScore != sig.Score {
		t.Errorf("expected all-momentum scoring: MR=%d MOM=%d Score=%d", sig.MRScore, sig.MomentumScore, sig.Score)
	}
	if len(sig.Plan.TakeProfit) < 2 {
		t.Errorf("plan needs >=2 TPs for the backtest R-model, got %d", len(sig.Plan.TakeProfit))
	}
	if sig.Plan.Entry == 0 || sig.Plan.StopLoss == 0 {
		t.Errorf("plan must have entry+stop, got entry=%v stop=%v", sig.Plan.Entry, sig.Plan.StopLoss)
	}
	if sig.Side == Long && !(sig.Plan.StopLoss < sig.Plan.Entry) {
		t.Errorf("long: stop %.2f must be below entry %.2f", sig.Plan.StopLoss, sig.Plan.Entry)
	}
	if sig.Side == Short && !(sig.Plan.StopLoss > sig.Plan.Entry) {
		t.Errorf("short: stop %.2f must be above entry %.2f", sig.Plan.StopLoss, sig.Plan.Entry)
	}
}

// syntheticUptrendPullback builds a candle series with a clear up-leg then a
// retrace, enough bars for EMA50 + AnalyzeStructure.
func syntheticUptrendPullback() []market.Candle {
	var cs []market.Candle
	price := 100.0
	// 70 bars up (HH-HL), then a pullback of 8 bars.
	for i := 0; i < 70; i++ {
		o := price
		price += 1.0
		cs = append(cs, market.Candle{Open: o, High: price + 0.5, Low: o - 0.3, Close: price})
	}
	for i := 0; i < 8; i++ {
		o := price
		price -= 0.6
		cs = append(cs, market.Candle{Open: o, High: o + 0.2, Low: price - 0.3, Close: price})
	}
	return cs
}
