package main

import (
	"testing"
	"time"

	"github.com/henry190927/trading-bot/journal"
)

func openTrade(sym, side string, stop float64) journal.Trade {
	return journal.Trade{
		Symbol: sym, Side: side, Entry: 100, Stop: stop,
		OpenedAt: time.Now().Add(-time.Hour), // ClosedAt zero => open
	}
}

// The hole this exists to prevent: bracketCycle skips `Stop <= 0`, so if
// orphanCycle also treats such a row as covered, a journalled position with no
// analysis stop is watched by nobody. d182cfe made that row the recommended
// way to record an unplanned position, which is exactly the position that most
// needs an alarm.
func TestStoplessOpenRowStaysWithTheOrphanGuard(t *testing.T) {
	trades := []journal.Trade{openTrade("XRP", "long", 0)}
	claimed, journalled := guardCoverage(trades)

	if claimed["XRP|long"] {
		t.Error("a stopless row must NOT be claimed — bracketCycle refuses it, " +
			"so claiming it here leaves the position unwatched by both guards")
	}
	if !journalled["XRP|long"] {
		t.Error("the row exists and the alert wording depends on knowing that")
	}
}

// With a stop, bracketCycle does take it, so orphanCycle must stand down or
// the desk gets two alarms for one position.
func TestStoppedOpenRowIsClaimed(t *testing.T) {
	claimed, journalled := guardCoverage([]journal.Trade{openTrade("NEAR", "short", 3.59)})
	if !claimed["NEAR|short"] {
		t.Error("a row with an analysis stop is bracketCycle's — orphanCycle must not double-report it")
	}
	if !journalled["NEAR|short"] {
		t.Error("journalled should be a superset of claimed")
	}
}

// Closed and no-fill rows describe nothing live, so they must not suppress an
// alert about a position that happens to share the symbol and side.
func TestClosedAndNoFillRowsCoverNothing(t *testing.T) {
	closed := openTrade("BTC", "long", 76000)
	closed.ClosedAt = time.Now()
	nofill := openTrade("ETH", "long", 2400)
	nofill.Outcome = "no-fill"

	claimed, journalled := guardCoverage([]journal.Trade{closed, nofill})
	for _, k := range []string{"BTC|long", "ETH|long"} {
		if claimed[k] || journalled[k] {
			t.Errorf("%s: a closed or no-fill row must cover nothing", k)
		}
	}
}

// claimed is always a subset of journalled — if that inverts, orphanCycle
// would stand down for a position it has no record of.
func TestClaimedIsSubsetOfJournalled(t *testing.T) {
	claimed, journalled := guardCoverage([]journal.Trade{
		openTrade("XRP", "long", 0),
		openTrade("SOL", "long", 110),
		openTrade("ETH", "short", 0),
	})
	for k := range claimed {
		if !journalled[k] {
			t.Errorf("%s claimed but not journalled", k)
		}
	}
	if len(claimed) != 1 || !claimed["SOL|long"] {
		t.Errorf("claimed = %v, want only SOL|long", claimed)
	}
	if len(journalled) != 3 {
		t.Errorf("journalled = %v, want all three open rows", journalled)
	}
}
