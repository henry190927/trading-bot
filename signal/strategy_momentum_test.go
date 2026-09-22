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

// choppyRange builds a series with no directional structure: swing highs and
// lows oscillate inside a band, so ClassifyTrendStructure returns neutral.
func choppyRange(n int) []market.Candle {
	var cs []market.Candle
	for i := 0; i < n; i++ {
		// Period-8 oscillation between roughly 98 and 102, no drift.
		base := 100.0
		switch i % 8 {
		case 0, 1:
			base = 101.5
		case 2, 3:
			base = 98.5
		case 4, 5:
			base = 101.0
		case 6, 7:
			base = 99.0
		}
		cs = append(cs, market.Candle{Open: base, High: base + 0.6, Low: base - 0.6, Close: base})
	}
	return cs
}

// staircaseUp builds an impulse/retrace zigzag that steps upward, which is
// what ClassifyTrendStructure actually needs: it requires >= 3 swing tops AND
// >= 3 swing bottoms and checks the last three of each for strict ascent.
//
// syntheticUptrendPullback cannot serve here — it rises +1.0 every bar, and a
// monotonic ramp has no local extremes, so the fractal finder returns almost
// no swing points and the series classifies as NEUTRAL despite going straight
// up. "Price rose a lot" and "the structure is an uptrend" are different
// claims and this detector only makes the second one.
func staircaseUp(legs int) []market.Candle {
	var cs []market.Candle
	price := 100.0
	for l := 0; l < legs; l++ {
		for i := 0; i < 6; i++ { // impulse
			o := price
			price += 2.0
			cs = append(cs, market.Candle{Open: o, High: price + 0.3, Low: o - 0.2, Close: price})
		}
		for i := 0; i < 3; i++ { // shallower retrace, so each leg nets +9
			o := price
			price -= 1.0
			cs = append(cs, market.Candle{Open: o, High: o + 0.2, Low: price - 0.3, Close: price})
		}
	}
	return cs
}

func TestStaircaseFixtureIsActuallyAnUptrend(t *testing.T) {
	// Guards the fixture itself: if a change to the swing finder stops seeing
	// these turns, every test below would pass by testing nothing.
	got, tops, bots := ClassifyTrendStructure(staircaseUp(8))
	if got != StructUptrend {
		t.Fatalf("fixture classifies as %v with %d tops / %d bots, want HH-HL uptrend",
			got, len(tops), len(bots))
	}
	if regimeWantsMomentum(syntheticUptrendPullback()) {
		t.Error("a monotonic ramp has no swing structure and must NOT read as a trend")
	}
}

func TestRegimeWantsMomentumReadsStructure(t *testing.T) {
	if !regimeWantsMomentum(staircaseUp(8)) {
		t.Error("a confirmed HH-HL series must select StructMomentum")
	}
	if regimeWantsMomentum(choppyRange(90)) {
		t.Error("a rangebound series must fall back to MR")
	}
	// Degenerate input must not select the momentum path by accident.
	if regimeWantsMomentum(nil) {
		t.Error("empty input must not select StructMomentum")
	}
}

func TestStructMomentumRegimeDisabledByDefault(t *testing.T) {
	if StructMomentumRegime {
		t.Fatal("StructMomentumRegime must default to false")
	}
}

// The anti-inert check. A flag whose output is byte-identical in both
// positions does nothing, and a backtest arm built on it reports a difference
// that is really noise — this is how a false +10R survived in flipbt.
//
// BTC is NOT on the strategyFor allowlist, so the ONLY thing that can route it
// to StructMomentum is the regime selector. Same candles, same symbol, both
// positions of the flag: the dispatch has to actually move.
func TestStructMomentumRegimeChangesDispatch(t *testing.T) {
	in := Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: staircaseUp(8)}

	if strategyFor(market.BTCUSDT, "1h") != StrategyMR {
		t.Fatal("precondition: BTC 1h must be an MR symbol, or this test proves nothing")
	}

	base := Evaluate(in)

	StructMomentumRegime = true
	defer func() { StructMomentumRegime = false }()
	gated := Evaluate(in)

	if base.Side == gated.Side && base.Score == gated.Score &&
		base.MRScore == gated.MRScore && base.MomentumScore == gated.MomentumScore {
		t.Fatalf("StructMomentumRegime is inert on a trending series: "+
			"both arms returned side=%v score=%v mr=%v mom=%v",
			base.Side, base.Score, base.MRScore, base.MomentumScore)
	}
	// The MR body reports an MR component; StructMomentum reports none by
	// contract (MRScore = 0, MomentumScore = Score).
	if gated.MRScore != 0 {
		t.Errorf("gated arm returned MRScore=%v — it did not take the StructMomentum path", gated.MRScore)
	}
}

// On a rangebound series the selector must choose MR, so the flag is a no-op
// there. Without this, "the flag changes something" could be satisfied by a
// selector that just always says momentum.
func TestStructMomentumRegimeLeavesRangeboundOnMR(t *testing.T) {
	in := Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: choppyRange(120)}
	base := Evaluate(in)

	StructMomentumRegime = true
	defer func() { StructMomentumRegime = false }()
	gated := Evaluate(in)

	if base.Side != gated.Side || base.Score != gated.Score {
		t.Errorf("selector diverted a rangebound series off MR: base %v/%v vs gated %v/%v",
			base.Side, base.Score, gated.Side, gated.Score)
	}
}

// StructMomentumOff is the all-MR baseline every arm is measured against. If a
// selector could override it, the baseline would silently stop being one.
func TestStructMomentumOffBeatsTheRegimeSelector(t *testing.T) {
	in := Inputs{Symbol: market.SOLUSDT, Timeframe: "1h", Candles: staircaseUp(8)}

	StructMomentumOff = true
	StructMomentumRegime = true
	defer func() { StructMomentumOff, StructMomentumRegime = false, false }()

	got := Evaluate(in)

	StructMomentumOff, StructMomentumRegime = true, false
	want := Evaluate(in)

	if got.Side != want.Side || got.Score != want.Score || got.MomentumScore != want.MomentumScore {
		t.Errorf("regime selector escaped StructMomentumOff: %v/%v vs baseline %v/%v",
			got.Side, got.Score, want.Side, want.Score)
	}
}

// ForceStrategy lets an autotrade rule name its strategy instead of inheriting
// whatever strategyFor decided for the symbol. BTC is NOT on the allowlist, so
// only the override can route it to StructMomentum.
func TestForceStrategyOverridesTheAllowlist(t *testing.T) {
	cs := staircaseUp(8)
	if strategyFor(market.BTCUSDT, "1h") != StrategyMR {
		t.Fatal("precondition: BTC 1h must be MR, or this proves nothing")
	}

	auto := Evaluate(Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: cs})
	forced := Evaluate(Inputs{Symbol: market.BTCUSDT, Timeframe: "1h", Candles: cs,
		ForceStrategy: StrategyStructMomentum})

	if auto.Side == forced.Side && auto.Score == forced.Score && auto.MRScore == forced.MRScore {
		t.Fatalf("ForceStrategy is inert: both returned side=%v score=%v mr=%v",
			auto.Side, auto.Score, auto.MRScore)
	}
	if forced.MRScore != 0 {
		t.Errorf("forced arm returned MRScore=%v — it did not take the StructMomentum path", forced.MRScore)
	}
}

// The zero value must mean "ask the allowlist", not "force MR". StrategyMR
// used to be iota 0, so an omitted field would have silently forced MR on
// every caller that never heard of the override — including the daemon scan,
// which has to keep honouring strategyFor.
func TestForceStrategyZeroValueDefersToTheAllowlist(t *testing.T) {
	if StrategyUnset != 0 {
		t.Fatal("StrategyUnset must be the zero value or an omitted ForceStrategy means something")
	}
	if StrategyMR == StrategyUnset || StrategyStructMomentum == StrategyUnset {
		t.Fatal("StrategyUnset must be distinct from both real strategies")
	}
	cs := staircaseUp(8)
	in := Inputs{Symbol: market.SOLUSDT, Timeframe: "1h", Candles: cs} // SOL 1h IS on the allowlist
	omitted := Evaluate(in)
	in.ForceStrategy = StrategyUnset
	explicit := Evaluate(in)
	if omitted.Side != explicit.Side || omitted.Score != explicit.Score {
		t.Error("an omitted ForceStrategy must behave exactly like StrategyUnset")
	}
	// And it must still be SM, because the allowlist says so.
	if omitted.MRScore != 0 {
		t.Errorf("SOL 1h should still run StructMomentum via the allowlist, got MRScore=%v", omitted.MRScore)
	}
}

// Forcing MR must be expressible too, or the override is half a feature.
func TestForceStrategyCanPinMROnAnSMSymbol(t *testing.T) {
	cs := staircaseUp(8)
	sm := Evaluate(Inputs{Symbol: market.SOLUSDT, Timeframe: "1h", Candles: cs})
	mr := Evaluate(Inputs{Symbol: market.SOLUSDT, Timeframe: "1h", Candles: cs,
		ForceStrategy: StrategyMR})
	if sm.Side == mr.Side && sm.Score == mr.Score && sm.MRScore == mr.MRScore {
		t.Fatal("ForceStrategy: StrategyMR is inert on an allowlisted SM symbol")
	}
}
