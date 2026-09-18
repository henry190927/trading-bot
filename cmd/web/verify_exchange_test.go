package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/bracket"
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
	// once StopOrderID was set. A bundled stop has failed to attach this way.
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

	// A live row: 100x SNDK, no stop recorded anywhere, no stop on the exchange.
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
// FilledAt is zero, and there is no position to find yet.
//
// The stale-journal branch is what would fire here, and it is gated on
// FilledAt being set. Ungating it would make every resting entry render
// "journal says filled but the exchange has no position" — a false alarm on
// the one page whose value is that its alarms are real.
func TestPendingEntryIsNotAStaleJournal(t *testing.T) {
	pending := journal.Trade{
		ID: 1, Symbol: "ETH", Side: "long",
		Entry: 2400, Stop: 2384, TP2: 2440,
		Leverage: 125, MarginUSDT: 60,
		OpenedAt: time.Now().UTC(),
		StopAuto: true, TP2Auto: true,
		EntryOrderID: "1234567890123456789",
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

// A bundled stop is protection that exists on the exchange without being a
// separate reduce-only order, so HasStop — which counts those — is false for a
// resting entry. The row used to render a red "🚨 NO STOP" over a trade that
// was in fact protected, which is the worst direction for this badge to be
// wrong in: it teaches the eye to skip the one alarm the page exists for.
//
// PendingStop is matched by ORDER ID. Two rows on the same symbol would
// otherwise each claim the other's protection, and the second one to be wrong
// is the one that is actually naked.
func TestPendingStopMatchesByOrderIDNotSymbol(t *testing.T) {
	mine := journal.Trade{ID: 1, Symbol: "ETH", Side: "long", EntryOrderID: "AAA"}
	ords := []bingx.OpenOrder{
		{OrderID: "AAA", Type: "LIMIT", Side: "BUY", Price: 2400, BundledStop: 2384, BundledTP: 2440},
		{OrderID: "BBB", Type: "LIMIT", Side: "BUY", Price: 2350, BundledStop: 2330},
	}

	find := func(tr journal.Trade) (stop, tp float64) {
		for _, o := range ords {
			if o.OrderID == tr.EntryOrderID && !o.ReduceOnly {
				return o.BundledStop, o.BundledTP
			}
		}
		return 0, 0
	}
	if stop, tp := find(mine); stop != 2384 || tp != 2440 {
		t.Errorf("own order: stop/tp = %v/%v, want 2384/2440", stop, tp)
	}
	// A row whose order is gone must report nothing rather than borrow the
	// other resting order's stop.
	gone := journal.Trade{ID: 2, Symbol: "ETH", Side: "long", EntryOrderID: "CCC"}
	if stop, _ := find(gone); stop != 0 {
		t.Errorf("a row with no matching order reported stop %v — it borrowed another order's", stop)
	}
	// A reduce-only row is protection for something else, not an entry.
	ords[0].ReduceOnly = true
	if stop, _ := find(mine); stop != 0 {
		t.Errorf("a reduce-only order was read as a pending entry (stop %v)", stop)
	}
}

// /ops/entry must NOT set StopAuto. The flag reads as "place the journal stop
// for me" (bracket.Decide), not "a stop already exists" — and the stop this
// route relies on is BUNDLED onto the entry order, so there is nothing for the
// sweep to place.
//
// Setting it true turned every trade from this route into one the daemon kept
// re-arming. On 2026-09-15 it re-placed a deliberately cancelled stop seven
// times and the seventh filled two seconds after it landed, closing a position
// its owner had chosen to keep. A guard may shout; it may not overrule.
func TestEntryRouteDoesNotArmTheStopSweep(t *testing.T) {
	// The row /ops/entry writes, as journalEntry builds it.
	row := journal.Trade{
		ID: 1, Symbol: "ETH", Side: "long",
		Entry: 2400, Stop: 2384, TP2: 2440,
		Leverage: 125, MarginUSDT: 60,
		EntryOrderID: "1234567890123456789",
		StopAuto:     false,
		TP2Auto:      false,
	}
	if row.StopAuto {
		t.Error("StopAuto is set — the sweep will re-place a cancelled stop")
	}
	if row.TP2Auto {
		t.Error("TP2Auto is set — the sweep will re-place a cancelled take-profit")
	}
	// And with it clear, bracket.Decide must not propose a placement in the
	// default alert mode even when the position is naked.
	pos := &bingx.Position{Symbol: "ETH-USDT", Side: "long", Quantity: 2.89, EntryPrice: 2400}
	d := bracket.Decide(row, pos, nil, bracket.ModeAlert)
	if d.PlaceStop != 0 {
		t.Errorf("alert mode proposed placing %v — StopAuto is clear, so it must only report", d.PlaceStop)
	}
	if d.State != bracket.StateNaked {
		t.Errorf("state = %v, want naked — it must still SAY the position is unprotected", d.State)
	}
	// The opt-in still works for rows that ask for it.
	armed := row
	armed.StopAuto = true
	if got := bracket.Decide(armed, pos, nil, bracket.ModeAlert).PlaceStop; got != 2384 {
		t.Errorf("StopAuto=true proposed %v, want 2384 — the opt-in must be unchanged", got)
	}
}

// TestVerifyExchangeRendersOrphans executes the WHOLE page with a non-empty
// Orphans slice.
//
// TestTemplatesParse proves verify_exchange.html parses; it cannot prove the
// orphan card renders, because a field that does not exist on the range
// element is resolved at EXECUTE time. The card carried {{if and (not
// .HasStop) (not .PendingStop)}} — copied from the verifyTrade card above it,
// where PendingStop is real — while orphanPos has no such field. Execution
// stopped mid-attribute, so the served page ended at `<div class="verify-card`
// with no </body>: the 🚨 count banner rendered above the break, the card
// naming the naked position did not, and gin had already written 200.
//
// A page whose entire job is showing an unjournaled naked position went a full
// session unable to show one. Rendering it with real data is the only check
// that would have caught it, so this test asserts the page is COMPLETE, not
// merely that Execute returned nil.
func TestVerifyExchangeRendersOrphans(t *testing.T) {
	tpl := template.Must(template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html"))

	orphans := []orphanPos{
		// Naked: no resting order, order book read cleanly. Red border.
		{Symbol: "BTC", Side: "long", Qty: 0.0615, Entry: 76400, Notional: 4698.6,
			Leverage: 125, MarginMode: "cross"},
		// Protected but unjournaled. Green badge, no red border.
		{Symbol: "ETH", Side: "short", Qty: 1.5, Entry: 2470, Notional: 3705,
			Leverage: 20, MarginMode: "cross", HasStop: true},
		// Order book unreadable — "unknown", NOT "naked". exchangeOrphans
		// deliberately leaves these out of nakedN; the border must agree.
		{Symbol: "SOL", Side: "long", Qty: 30, Entry: 100.1, Notional: 3003,
			Leverage: 10, MarginMode: "cross", ReadErr: "read orders: 429 rate limited"},
	}

	var buf bytes.Buffer
	err := tpl.ExecuteTemplate(&buf, "verify_exchange.html", map[string]any{
		"Rows": []verifyTrade{}, "Orphans": orphans,
		"OrphanNakedN": 1, "WarnN": 0, "SessionWarnN": 0, "NoClient": false,
		"UpdatedUTC": "2026-09-18 09:00:00 UTC+8",
	})
	if err != nil {
		t.Fatalf("verify_exchange.html failed to execute with orphans: %v", err)
	}
	out := buf.String()

	// Completeness first: a mid-execution failure truncates rather than
	// erroring once gin is streaming, so "it ended" is the real assertion.
	if !strings.Contains(out, "</html>") {
		t.Fatalf("page truncated — no </html>; last 200 bytes:\n%s", tail(out, 200))
	}
	if strings.Contains(out, `<div class="verify-card`) && !strings.Contains(out, "</div>") {
		t.Errorf("verify-card opened but never closed — the 2026-09-18 truncation")
	}

	// Each orphan must actually reach the page. Without this the test would
	// pass against a template that renders no cards at all.
	for _, want := range []string{"BTC", "0.0615", "76400", "4699", "125x", "ETH", "SOL"} {
		if !strings.Contains(out, want) {
			t.Errorf("orphan card missing %q from the rendered page", want)
		}
	}

	// The red border tracks the 🚨 branch and nakedN: naked only. An
	// unreadable order book rendering as naked is the cry-wolf direction
	// exchangeOrphans explicitly refuses.
	if n := strings.Count(out, "verify-card b-red"); n != 1 {
		t.Errorf("b-red applied to %d cards, want exactly 1 (the naked BTC);"+
			" a read error or a protected position must not paint red", n)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
