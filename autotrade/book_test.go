package autotrade

import (
	"testing"
	"time"

	"myFirstGo/trading-bot/market"
)

// Fixtures are traced by hand against autotrade.EvaluateFire's actual model
// before any expectation is written: a Market=true long fills at the fire, the
// exit scan checks STOP before TP, and NetR is (exit-entry)/risk.
//
// Long entry 100 / stop 90 / tp 120 → risk 10, so a stop is -1.0R and a tp is
// +2.0R.
var (
	bkNow   = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	bkFire  = time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	bkBar   = time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC) // after bkFire, same UTC day
	bkYFire = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bkYBar  = time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC) // yesterday
)

func bookBar(closeTime time.Time, low, high, close float64) market.Candle {
	return market.Candle{Low: low, High: high, Close: close, CloseTime: closeTime}
}

func bookLongFire(sym, strat string, at time.Time, margin float64) PaperFire {
	return PaperFire{
		Time: at, Symbol: sym, TF: "1h", Strategy: strat, Side: "long", Market: true,
		Entry: 100, Stop: 90, TP: 120, Margin: margin,
	}
}

// fixed returns the same candles for every (symbol, tf).
func bookFixed(cs ...market.Candle) CandleFn {
	return func(string, string) []market.Candle { return cs }
}

func TestBuildBook(t *testing.T) {
	// L=95 H=105 touches neither 90 nor 120 → still OPEN.
	openBar := bookFixed(bookBar(bkBar, 95, 105, 100))
	// L=85 breaches the 90 stop → OutStop, NetR -1.0, ExitAt = bkBar.
	stopBar := bookFixed(bookBar(bkBar, 85, 105, 100))
	// H=125 with L=95 clears TP without touching the stop → OutTP, NetR +2.0.
	tpBar := bookFixed(bookBar(bkBar, 95, 125, 120))

	t.Run("one open position occupies a slot and its margin", func(t *testing.T) {
		bk, un := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, openBar, bkNow)
		if bk.OpenCount != 1 || bk.OpenMargin != 35 {
			t.Fatalf("got %+v, want open=1 margin=35", bk)
		}
		if bk.RealizedRToday != 0 {
			t.Errorf("an open position must not contribute realized R, got %+.2f", bk.RealizedRToday)
		}
		if un != 0 {
			t.Errorf("unscored = %d, want 0", un)
		}
	})

	t.Run("distinct rules each take a slot", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{
			bookLongFire("BTC", "engine", bkFire, 35),
			bookLongFire("ETH", "sweep-reject", bkFire, 35),
		}, openBar, bkNow)
		if bk.OpenCount != 2 || bk.OpenMargin != 70 {
			t.Fatalf("got %+v, want open=2 margin=70", bk)
		}
	})

	t.Run("only the NEWEST fire per rule counts — pins the newest-first contract", func(t *testing.T) {
		// Same rule twice. If the ordering contract were violated (or the
		// dedup keyed wrongly) the 999 margin would leak into the book.
		bk, _ := BuildBook([]PaperFire{
			bookLongFire("BTC", "engine", bkFire, 35),                    // newest
			bookLongFire("BTC", "engine", bkFire.Add(-2*time.Hour), 999), // older, same rule
		}, openBar, bkNow)
		if bk.OpenCount != 1 {
			t.Fatalf("open count = %d, want 1 (one position per rule)", bk.OpenCount)
		}
		if bk.OpenMargin != 35 {
			t.Fatalf("margin = %.0f, want 35 from the NEWEST fire", bk.OpenMargin)
		}
	})

	t.Run("a settled stop frees the slot and books -1R", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, stopBar, bkNow)
		if bk.OpenCount != 0 || bk.OpenMargin != 0 {
			t.Errorf("a stopped position must not occupy the book, got %+v", bk)
		}
		if bk.RealizedRToday != -1.0 {
			t.Errorf("realized = %+.2fR, want -1.00R", bk.RealizedRToday)
		}
	})

	t.Run("a settled tp books +2R", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, tpBar, bkNow)
		if bk.RealizedRToday != 2.0 {
			t.Errorf("realized = %+.2fR, want +2.00R", bk.RealizedRToday)
		}
	})

	t.Run("R settled on a previous UTC day is excluded", func(t *testing.T) {
		yFire := bookLongFire("BTC", "engine", bkYFire, 35)
		bk, _ := BuildBook([]PaperFire{yFire}, bookFixed(bookBar(bkYBar, 85, 105, 100)), bkNow)
		if bk.RealizedRToday != 0 {
			t.Errorf("yesterday's -1R must not count toward today, got %+.2f", bk.RealizedRToday)
		}
		if bk.OpenCount != 0 {
			t.Errorf("it settled, so no slot: got %+v", bk)
		}
	})

	t.Run("closed rules across symbols accumulate today's R", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{
			bookLongFire("BTC", "engine", bkFire, 35),
			bookLongFire("ETH", "engine", bkFire, 35),
		}, stopBar, bkNow)
		if bk.RealizedRToday != -2.0 {
			t.Errorf("two stops today = -2.00R, got %+.2f", bk.RealizedRToday)
		}
	})

	t.Run("FAIL-SAFE: an unscoreable newest fire counts as OPEN, not skipped", func(t *testing.T) {
		// No candles (API hiccup / unknown symbol). Skipping would undercount
		// the book and let the caps under-enforce — the wrong way to fail.
		none := func(string, string) []market.Candle { return nil }
		bk, un := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, none, bkNow)
		if bk.OpenCount != 1 || bk.OpenMargin != 35 {
			t.Fatalf("must assume open, got %+v", bk)
		}
		if un != 1 {
			t.Errorf("unscored = %d, want 1 so the caller can warn", un)
		}
	})

	t.Run("an empty log is an empty book", func(t *testing.T) {
		bk, un := BuildBook(nil, openBar, bkNow)
		if bk.OpenCount != 0 || bk.OpenMargin != 0 || bk.RealizedRToday != 0 || un != 0 {
			t.Errorf("want zero book, got %+v unscored=%d", bk, un)
		}
	})
}
