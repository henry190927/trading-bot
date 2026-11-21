package main

import (
	"errors"
	"fmt"

	"github.com/henry190927/trading-bot/journal"
)

// Every change to journal.csv goes through updateJournal, which re-reads the
// file, applies the change and writes it back while holding journalMu.
//
// Why a second lock instead of sweepMu: the background sweeps hold sweepMu
// across exchange round trips (seconds), so a form submit that queued behind
// it would hang. journalMu is held only around the file read-modify-write,
// never across a network call. The sweeps keep working on a copy and merge
// their own fields back through updateJournal, so a row recorded or edited
// meanwhile is never overwritten.
//
// Lock order is sweepMu → journalMu. Nothing may take sweepMu while holding
// journalMu, and fn must not call back into updateJournal.

// errNoChange from an updateJournal callback means "nothing to write".
var errNoChange = errors.New("journal: no change")

// errTradeNotFound is returned when the row a change targets no longer exists
// (deleted or unwound meanwhile).
var errTradeNotFound = errors.New("journal: trade not found")

// updateJournal reads the journal, passes it to fn, and writes back what fn
// returns, all under journalMu.
func (s *server) updateJournal(fn func(trades []journal.Trade) ([]journal.Trade, error)) error {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	trades, err := journal.ReadAll("")
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	out, err := fn(trades)
	if errors.Is(err, errNoChange) {
		return nil
	}
	if err != nil {
		return err
	}
	return journal.WriteAll("", out)
}

// errStaleEdit means the row changed after the edit form was rendered.
var errStaleEdit = errors.New("journal: row changed since the form was loaded")

// updateTrade applies fn to the current version of row id. If fn returns an
// error nothing is written and the error is returned.
func (s *server) updateTrade(id int, fn func(t *journal.Trade) error) error {
	return s.updateJournal(func(trades []journal.Trade) ([]journal.Trade, error) {
		for i := range trades {
			if trades[i].ID == id {
				if err := fn(&trades[i]); err != nil {
					return nil, err
				}
				return trades, nil
			}
		}
		return nil, fmt.Errorf("%w: #%d", errTradeNotFound, id)
	})
}

// appendTrade adds t as a new row. The ID is assigned from the journal as it
// is at write time, so two concurrent submits can never get the same ID.
// Returns t with its ID set.
func (s *server) appendTrade(t journal.Trade) (journal.Trade, error) {
	err := s.updateJournal(func(trades []journal.Trade) ([]journal.Trade, error) {
		t.ID = journal.NextID(trades)
		return append(trades, t), nil
	})
	return t, err
}

// deleteTrade removes row id. Deleting a row that is already gone is not an
// error: the outcome the caller asked for holds.
func (s *server) deleteTrade(id int) error {
	return s.updateJournal(func(trades []journal.Trade) ([]journal.Trade, error) {
		out := make([]journal.Trade, 0, len(trades))
		for _, t := range trades {
			if t.ID != id {
				out = append(out, t)
			}
		}
		if len(out) == len(trades) {
			return nil, errNoChange
		}
		return out, nil
	})
}

// copyOrderState copies the fields the exchange helpers (placeEntryOnBingX,
// cancelTrackedOrdersOnBingX) set on their working copy of a row onto the
// current version of that row. EquityUSDT is only filled, never replaced.
func copyOrderState(dst, src *journal.Trade) {
	dst.EntryOrderID = src.EntryOrderID
	dst.StopOrderID = src.StopOrderID
	dst.TP1OrderID = src.TP1OrderID
	dst.TP2OrderID = src.TP2OrderID
	if dst.EquityUSDT == 0 {
		dst.EquityUSDT = src.EquityUSDT
	}
}
