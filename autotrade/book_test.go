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
		bk, un := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, openBar, bkNow, 6)
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
		}, openBar, bkNow, 6)
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
		}, openBar, bkNow, 6)
		if bk.OpenCount != 1 {
			t.Fatalf("open count = %d, want 1 (one position per rule)", bk.OpenCount)
		}
		if bk.OpenMargin != 35 {
			t.Fatalf("margin = %.0f, want 35 from the NEWEST fire", bk.OpenMargin)
		}
	})

	t.Run("a settled stop frees the slot and books -1R", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, stopBar, bkNow, 6)
		if bk.OpenCount != 0 || bk.OpenMargin != 0 {
			t.Errorf("a stopped position must not occupy the book, got %+v", bk)
		}
		if bk.RealizedRToday != -1.0 {
			t.Errorf("realized = %+.2fR, want -1.00R", bk.RealizedRToday)
		}
	})

	t.Run("a settled tp books +2R", func(t *testing.T) {
		bk, _ := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, tpBar, bkNow, 6)
		if bk.RealizedRToday != 2.0 {
			t.Errorf("realized = %+.2fR, want +2.00R", bk.RealizedRToday)
		}
	})

	t.Run("R settled on a previous UTC day is excluded", func(t *testing.T) {
		yFire := bookLongFire("BTC", "engine", bkYFire, 35)
		bk, _ := BuildBook([]PaperFire{yFire}, bookFixed(bookBar(bkYBar, 85, 105, 100)), bkNow, 6)
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
		}, stopBar, bkNow, 6)
		if bk.RealizedRToday != -2.0 {
			t.Errorf("two stops today = -2.00R, got %+.2f", bk.RealizedRToday)
		}
	})

	t.Run("FAIL-SAFE: an unscoreable newest fire counts as OPEN, not skipped", func(t *testing.T) {
		// No candles (API hiccup / unknown symbol). Skipping would undercount
		// the book and let the caps under-enforce — the wrong way to fail.
		none := func(string, string) []market.Candle { return nil }
		bk, un := BuildBook([]PaperFire{bookLongFire("BTC", "engine", bkFire, 35)}, none, bkNow, 6)
		if bk.OpenCount != 1 || bk.OpenMargin != 35 {
			t.Fatalf("must assume open, got %+v", bk)
		}
		if un != 1 {
			t.Errorf("unscored = %d, want 1 so the caller can warn", un)
		}
	})

	t.Run("an empty log is an empty book", func(t *testing.T) {
		bk, un := BuildBook(nil, openBar, bkNow, 6)
		if bk.OpenCount != 0 || bk.OpenMargin != 0 || bk.RealizedRToday != 0 || un != 0 {
			t.Errorf("want zero book, got %+v unscored=%d", bk, un)
		}
	})
}

// The bug this guards: a rule whose setup persists re-fires every bar while its
// position is open, so ONE trade leaves several fires in the log (seen live:
// BTC range-edge 09-01 16:00→17:00, ETH htf-snr 08-30 17:00→18:00). Counting
// each fire's -1R separately would trip the daily breaker EARLIER than its
// configured threshold — the dangerous direction for a control the trader
// relies on. Realized R therefore comes from DedupFires, not from raw fires.
func TestBuildBookDedupesRepeatFiresForRealizedR(t *testing.T) {
	// Two fires of the SAME rule an hour apart. Traced by hand:
	//   fire1 @00:00 → bar 00:59 has Low 85 ≤ stop 90 → stop, ExitAt 00:59, -1R.
	//     DedupFires holds the rule until 00:59 + 6 cooldown bars = 06:59.
	//   fire2 @01:00 is Before(06:59) → ABSORBED, contributes nothing.
	// Without the dedup, fire2's own forward bar (01:59, Low 85) would settle
	// it too and the day would read -2R for one trade.
	f1 := bookLongFire("BTC", "range-edge", time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 35)
	f2 := bookLongFire("BTC", "range-edge", time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC), 35)
	cs := bookFixed(
		bookBar(time.Date(2026, 9, 2, 0, 59, 0, 0, time.UTC), 85, 105, 100),
		bookBar(time.Date(2026, 9, 2, 1, 59, 0, 0, time.UTC), 85, 105, 100),
	)

	// newest-first input, as ReadFires supplies.
	bk, _ := BuildBook([]PaperFire{f2, f1}, cs, bkNow, 6)

	if bk.RealizedRToday != -1.0 {
		t.Fatalf("realized = %+.2fR, want -1.00R — the repeat fire must be absorbed, not double-counted", bk.RealizedRToday)
	}

	// Sanity-check the fixture really would have double-counted: both fires
	// settle to -1R when scored independently.
	for i, f := range []PaperFire{f1, f2} {
		if oc := EvaluateFire(f, cs("BTC", "1h"), 6); oc.Status != OutStop || oc.NetR != -1 {
			t.Fatalf("fixture fire %d: got %s %+.2fR, want stop -1.00R — the test would prove nothing otherwise", i+1, oc.Status, oc.NetR)
		}
	}
}

// A genuinely separate second trade of the same rule — the first settled and
// the cooldown elapsed — must still count. Over-collapsing would let a bad day
// hide from the breaker, which is the opposite failure.
func TestBuildBookCountsGenuineSecondTrade(t *testing.T) {
	f1 := bookLongFire("ETH", "htf-snr", time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 35)
	f2 := bookLongFire("ETH", "htf-snr", time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC), 35)
	cs := bookFixed(
		bookBar(time.Date(2026, 9, 2, 0, 59, 0, 0, time.UTC), 85, 105, 100), // stops f1
		bookBar(time.Date(2026, 9, 2, 9, 59, 0, 0, time.UTC), 85, 105, 100), // stops f2
	)
	// f1 stops 00:59, +6 cooldown bars → free at 06:59; f2 at 09:00 is after.
	bk, _ := BuildBook([]PaperFire{f2, f1}, cs, bkNow, 6)
	if bk.RealizedRToday != -2.0 {
		t.Fatalf("realized = %+.2fR, want -2.00R — two real trades must both count", bk.RealizedRToday)
	}
}
