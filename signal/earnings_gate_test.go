package signal

import (
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/earnings"
	"github.com/henry190927/trading-bot/market"
)

// buildTestCandles makes n 1h candles ending at endUTC with a gentle
// deterministic walk — enough for Evaluate to run without panicking.
func buildTestCandles(endUTC time.Time, n int) []market.Candle {
	out := make([]market.Candle, n)
	base := 100.0
	for i := 0; i < n; i++ {
		close := endUTC.Add(time.Duration(-(n-1-i)) * time.Hour)
		open := close.Add(-time.Hour)
		p := base + float64(i%7) - 3 // oscillate ±3, no randomness
		out[i] = market.Candle{
			OpenTime:    open,
			CloseTime:   close,
			Open:        p,
			High:        p + 1.5,
			Low:         p - 1.5,
			Close:       p + 0.5,
			Volume:      1000,
			QuoteVolume: 100000,
		}
	}
	return out
}

const earningsFixture = `{
  "updated_utc":"2026-08-18T00:00:00Z",
  "events":[{"symbol":"SNDK","datetime_utc":"2026-08-27T20:05:00Z","when":"amc","blackout_before_min":120,"blackout_after_min":960}]
}`

func loadFixtureCal(t *testing.T) {
	t.Helper()
	if err := earnings.Default().LoadJSON([]byte(earningsFixture)); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	t.Cleanup(func() { _ = earnings.Default().LoadJSON([]byte(`{"events":[]}`)) })
}

func hasEarningsBlackout(sig Signal) bool {
	for _, r := range sig.Reasons {
		if strings.Contains(r, "earnings blackout") {
			return true
		}
	}
	return false
}

func TestEarningsGate_StockInsideWindow(t *testing.T) {
	loadFixtureCal(t)
	// SNDK window is [2026-08-27T18:05Z, 2026-08-28T12:05Z); bar closes at 19:00Z → inside.
	candles := buildTestCandles(mustT(t, "2026-08-27T19:00:00Z"), 80)
	sig := Evaluate(Inputs{
		Symbol:    market.Symbol("NCSKSNDK2USD-USDT"),
		Timeframe: market.TF1h,
		Candles:   candles,
	})
	if sig.Side != Flat {
		t.Errorf("Side = %v, want Flat inside earnings window", sig.Side)
	}
	if !hasEarningsBlackout(sig) {
		t.Errorf("Reasons = %v, want an 'earnings blackout' reason", sig.Reasons)
	}
	// VP should still be populated (chart visualisation preserved), like macro.
	if sig.VP.POC == 0 {
		t.Errorf("VP.POC = 0, want populated during blackout (>=60 candles)")
	}
}

func TestEarningsGate_StockOutsideWindow(t *testing.T) {
	loadFixtureCal(t)
	// Bar closes 2 days before the window → gate must NOT fire.
	candles := buildTestCandles(mustT(t, "2026-08-25T19:00:00Z"), 80)
	sig := Evaluate(Inputs{
		Symbol:    market.Symbol("NCSKSNDK2USD-USDT"),
		Timeframe: market.TF1h,
		Candles:   candles,
	})
	if hasEarningsBlackout(sig) {
		t.Errorf("Reasons = %v, want NO earnings blackout outside window", sig.Reasons)
	}
}

func TestEarningsGate_NonStockNeverGated(t *testing.T) {
	loadFixtureCal(t)
	// Same in-window bar time, but BTC/XAU are not companies → never gated.
	inWindow := mustT(t, "2026-08-27T19:00:00Z")
	for _, sym := range []string{"BTC-USDT", "NCCOGOLD2USD-USDT"} {
		candles := buildTestCandles(inWindow, 80)
		sig := Evaluate(Inputs{
			Symbol:    market.Symbol(sym),
			Timeframe: market.TF1h,
			Candles:   candles,
		})
		if hasEarningsBlackout(sig) {
			t.Errorf("%s: got earnings blackout, want none (non-stock)", sym)
		}
	}
}

func TestEarningsGate_EmptyCalendarNoOp(t *testing.T) {
	// No fixture loaded (reset to empty) → even a stock symbol in-window is fine.
	_ = earnings.Default().LoadJSON([]byte(`{"events":[]}`))
	candles := buildTestCandles(mustT(t, "2026-08-27T19:00:00Z"), 80)
	sig := Evaluate(Inputs{
		Symbol:    market.Symbol("NCSKSNDK2USD-USDT"),
		Timeframe: market.TF1h,
		Candles:   candles,
	})
	if hasEarningsBlackout(sig) {
		t.Errorf("empty calendar should never gate; Reasons = %v", sig.Reasons)
	}
}

func mustT(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm.UTC()
}
