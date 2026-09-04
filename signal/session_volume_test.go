package signal

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

func volCandle(startUTC time.Time, vol float64) market.Candle {
	return market.Candle{
		OpenTime: startUTC, CloseTime: startUTC.Add(time.Hour),
		Open: 100, High: 100, Low: 100, Close: 100, Volume: vol,
	}
}

// The flag was A/B'd and REJECTED (see the var comment). The thing a future
// edit must not break is that it stays a no-op when off: the trailing value
// passes through untouched, so the shipped engine is byte-identical to what
// the historical A/B tables in engine.go were measured against.
func TestRelVolForIsPassThroughWhenFlagOff(t *testing.T) {
	t.Cleanup(func() { SessionVolBaseline = false })
	SessionVolBaseline = false

	var cs []market.Candle
	for i, v := range []float64{10, 20, 30, 40, 50} {
		cs = append(cs, volCandle(time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC), v))
	}
	cs = append(cs, volCandle(time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC), 60))

	for _, trailing := range []float64{0, 1, 1.7, 42.5} {
		got, base := relVolFor(cs, len(cs)-1, trailing)
		if got != trailing {
			t.Errorf("trailing %v → %v, want pass-through", trailing, got)
		}
		if base != "20-bar avg" {
			t.Errorf("label = %q, want the old baseline named", base)
		}
	}
}

// And that with the flag ON it actually substitutes — an inert A/B flag is a
// bug, not robustness, and this one was only trustworthy because the arms
// provably differed.
func TestRelVolForSubstitutesWhenFlagOn(t *testing.T) {
	t.Cleanup(func() { SessionVolBaseline = false })

	var cs []market.Candle
	for i, v := range []float64{10, 20, 30, 40, 50} {
		cs = append(cs, volCandle(time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC), v))
	}
	cs = append(cs, volCandle(time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC), 60))
	i := len(cs) - 1

	SessionVolBaseline = false
	off, offBase := relVolFor(cs, i, 99.0)
	SessionVolBaseline = true
	on, onBase := relVolFor(cs, i, 99.0)

	if off == on {
		t.Errorf("both arms returned %v — the flag is inert", off)
	}
	if on != 2.0 { // 60 / median(10,20,30,40,50)=30
		t.Errorf("session baseline = %v, want 2.0", on)
	}
	if offBase == onBase {
		t.Errorf("both arms labelled %q — a backtest log could not tell them apart", offBase)
	}
}

// Below the per-bucket sample floor the flag must fall back rather than act on
// a one-sample median. This is what makes it a deliberate no-op on 5m/15m
// (288/96 buckets a day) instead of a randomiser.
func TestRelVolForFallsBackOnThinBucket(t *testing.T) {
	t.Cleanup(func() { SessionVolBaseline = false })
	SessionVolBaseline = true

	var cs []market.Candle
	for i, v := range []float64{10, 20} { // far below sessionVolMinSamples
		cs = append(cs, volCandle(time.Date(2026, 9, 7+i, 13, 0, 0, 0, time.UTC), v))
	}
	cs = append(cs, volCandle(time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC), 60))

	got, base := relVolFor(cs, len(cs)-1, 7.5)
	if got != 7.5 {
		t.Errorf("got %v, want the trailing 7.5 back", got)
	}
	if base != "20-bar avg" {
		t.Errorf("label = %q, want the fallback named", base)
	}
}
