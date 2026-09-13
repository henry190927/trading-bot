package signal

import (
	"testing"

	"github.com/henry190927/trading-bot/market"
)

// The core 4 + stock synthetics must run MR on EVERY timeframe — StructMomentum
// is assigned only to specific alt (symbol,TF) pairs, and never to a market.All()
// symbol. Locks "zero daemon/live-core behavior change".
func TestStrategyForCoreStaysMR(t *testing.T) {
	tfs := []market.Timeframe{"5m", "15m", "1h", "2h", "4h", "1d"}
	for _, s := range []market.Symbol{
		market.BTCUSDT, market.ETHUSDT, market.XAUUSDT, market.XAGUSDT,
		market.SNDKUSDT, market.NVDAUSDT, market.NEARUSDT,
	} {
		for _, tf := range tfs {
			if got := strategyFor(s, tf); got != StrategyMR {
				t.Errorf("strategyFor(%s, %s) = %v, want StrategyMR", s, tf, got)
			}
		}
	}
}

// The assigned alt pairs run StructMomentum on their cleared TFs, MR elsewhere.
func TestStrategyForAltAssignments(t *testing.T) {
	sm := StrategyStructMomentum
	cases := []struct {
		sym  market.Symbol
		tf   market.Timeframe
		want StrategyKind
	}{
		// Gate results from the 2026-09-03 Phase-2 validation are carried
		// inline so an edit to the allowlist has to confront the number.
		{market.SOLUSDT, "1h", sm}, // PASS 3/3
		{market.SOLUSDT, "2h", sm}, // undecided (n=4/6/12), kept
		{market.SOLUSDT, "15m", StrategyMR},
		{market.LINKUSDT, "1h", StrategyMR}, // REMOVED — 1/3
		{market.LINKUSDT, "2h", sm},         // undecided (n=3/6/11), kept
		{market.LINKUSDT, "4h", StrategyMR},
		{market.SUIUSDT, "1h", sm}, // PASS 3/3 (negative in absolute terms — see strategyFor)
		{market.SUIUSDT, "2h", StrategyMR},
		{market.HYPEUSDT, "1h", StrategyMR}, // REMOVED — FAIL 0/3
		{market.HYPEUSDT, "2h", StrategyMR},
	}
	for _, c := range cases {
		if got := strategyFor(c.sym, c.tf); got != c.want {
			t.Errorf("strategyFor(%s, %s) = %v, want %v", c.sym, c.tf, got, c.want)
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
