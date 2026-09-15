package main

import (
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
)

// The interesting rows cannot be produced against the live API on demand, so
// these drive classifyVerify — the same function the handler calls — with
// hand-built exchange payloads. The page's whole purpose is being trusted over
// local bookkeeping, so a false "🛡️ stop live" is the failure that matters.
func verifyVerdict(t journal.Trade, pos *bingx.Position, ords []bingx.OpenOrder) (bool, string) {
	return classifyVerify(t, pos, ords)
}

func vexTrade() journal.Trade {
	return journal.Trade{
		ID: 67, Symbol: "BTC", Side: "long", Entry: 79006, Stop: 78760,
		OpenedAt: time.Date(2026, 9, 7, 23, 40, 0, 0, time.UTC),
		FilledAt: time.Date(2026, 9, 7, 23, 41, 0, 0, time.UTC),
	}
}

func vexPos() *bingx.Position {
	return &bingx.Position{
		Symbol: "BTC-USDT", PositionSide: "BOTH", Side: "long",
		Quantity: 0.131, EntryPrice: 79006, Leverage: 125,
	}
}

func TestVerifyExchangeVerdicts(t *testing.T) {
	realStop := bingx.OpenOrder{
		Symbol: "BTC-USDT", OrderID: "sl", Type: "STOP_MARKET", Side: "SELL",
		PositionSide: "BOTH", StopPrice: 78760, Quantity: 0.131, ReduceOnly: true,
	}

	t.Run("a genuine reduce-only stop reads protected", func(t *testing.T) {
		hasStop, warn := verifyVerdict(vexTrade(), vexPos(), []bingx.OpenOrder{realStop})
		if !hasStop || warn != "" {
			t.Errorf("hasStop=%v warn=%q, want true and no warning", hasStop, warn)
		}
	})

	// The old check was strings.Contains(type, "STOP") with no reduce-only
	// test, so this order rendered as "🛡️ stop live" while being an order
	// that could OPEN a position.
	t.Run("a STOP that is not reduce-only must NOT read protected", func(t *testing.T) {
		o := realStop
		o.ReduceOnly = false
		hasStop, warn := verifyVerdict(vexTrade(), vexPos(), []bingx.OpenOrder{o})
		if hasStop {
			t.Error("a non-reduce-only STOP rendered as live protection")
		}
		if !strings.Contains(warn, "裸單") {
			t.Errorf("warn = %q, want a naked warning", warn)
		}
	})

	t.Run("a stop on the wrong side must NOT read protected", func(t *testing.T) {
		o := realStop
		o.Side = "BUY"
		if hasStop, _ := verifyVerdict(vexTrade(), vexPos(), []bingx.OpenOrder{o}); hasStop {
			t.Error("a BUY stop rendered as protection for a long")
		}
	})

	// The row cmd/web could never reach before, because it stopped checking
	// once StopOrderID was set. #60's bundled stop failed to attach this way.
	t.Run("journal names a stop the exchange does not have", func(t *testing.T) {
		tr := vexTrade()
		tr.StopOrderID = "2094351178603393024"
		hasStop, warn := verifyVerdict(tr, vexPos(), nil)
		if hasStop {
			t.Error("a ghost order id rendered as live protection")
		}
		if !strings.Contains(warn, "2094351178603393024") || !strings.Contains(warn, "裸單") {
			t.Errorf("warn = %q, want a naked warning naming the ghost order", warn)
		}
	})

	// #62: 100x SNDK, no stop recorded anywhere, no stop on the exchange.
	// Decide skips it for lack of intent, so the page must catch it itself.
	t.Run("no journal stop and no exchange stop still warns", func(t *testing.T) {
		tr := vexTrade()
		tr.Stop = 0
		hasStop, warn := verifyVerdict(tr, vexPos(), nil)
		if hasStop {
			t.Error("hasStop true with no stop anywhere")
		}
		if !strings.Contains(warn, "也沒記停損價") {
			t.Errorf("warn = %q, want the no-journal-stop variant", warn)
		}
	})

	t.Run("stale journal warns with or without a recorded stop", func(t *testing.T) {
		for _, stop := range []float64{78760, 0} {
			tr := vexTrade()
			tr.Stop = stop
			_, warn := verifyVerdict(tr, nil, nil)
			if !strings.Contains(warn, "交易所沒有部位") {
				t.Errorf("stop=%g: warn = %q, want the stale-journal warning", stop, warn)
			}
		}
	})

	t.Run("a pending limit is not a warning", func(t *testing.T) {
		tr := vexTrade()
		tr.FilledAt = time.Time{}
		entryOrder := bingx.OpenOrder{
			Symbol: "BTC-USDT", OrderID: "entry", Type: "LIMIT", Side: "BUY",
			Price: 79006, Quantity: 0.131, ReduceOnly: false,
		}
		hasStop, warn := verifyVerdict(tr, nil, []bingx.OpenOrder{entryOrder})
		if hasStop || warn != "" {
			t.Errorf("hasStop=%v warn=%q, want false and no warning for an unfilled limit", hasStop, warn)
		}
	})
}

// A journal row for a RESTING limit — recorded, not yet filled — must produce
// no warning. /ops/entry writes exactly this shape: the order is on the book,
// FilledAt is zero, and there is no position to find.
//
// The stale-journal branch is what would fire here, and it is gated on
// FilledAt being set. Ungating it would make every resting entry render
// "journal says filled but the exchange has no position" — a false alarm on
// the one page whose value is that its alarms are real.
func TestPendingEntryIsNotAStaleJournal(t *testing.T) {
	pending := journal.Trade{
		ID: 71, Symbol: "ETH", Side: "long",
		Entry: 2441, Stop: 2424.88, TP2: 2480.50,
		Leverage: 125, MarginUSDT: 56.44,
		OpenedAt: time.Now().UTC(),
		StopAuto: true, TP2Auto: true,
		EntryOrderID: "2099785767551463424",
		// FilledAt deliberately zero — this is the whole point.
	}
	if !pending.IsPending() {
		t.Fatalf("fixture is not pending: open=%v filled=%v", pending.IsOpen(), pending.FilledAt)
	}
	_, warn := classifyVerify(pending, nil, nil)
	if warn != "" {
		t.Errorf("a resting entry warned: %q", warn)
	}

	// Once it fills, the same row with no position IS stale and must warn.
	filled := pending
	filled.FilledAt = time.Now().UTC()
	if _, w := classifyVerify(filled, nil, nil); w == "" {
		t.Error("a filled row with no position must still warn")
	}
}
