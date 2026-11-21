package main

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/henry190927/trading-bot/journal"
)

// Before journalMu, two form submits could each read the journal, append a
// row with the same NextID, and the later WriteAll erased the earlier row.
// Under updateJournal every append sees the previous one.
func TestAppendTradeConcurrentKeepsEveryRow(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	s := &server{}

	const n = 40
	var wg sync.WaitGroup
	ids := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.appendTrade(journal.Trade{Symbol: "BTC", Side: "long", Entry: 100, Stop: 90})
			ids[i], errs[i] = got.ID, err
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("append %d: %v", i, errs[i])
		}
		if seen[ids[i]] {
			t.Fatalf("id %d handed out twice", ids[i])
		}
		seen[ids[i]] = true
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != n {
		t.Fatalf("journal has %d rows, want %d — a concurrent write erased rows", len(trades), n)
	}
}

// A form edit and a sweep merge on different fields of the same row must both
// survive, whatever order they land in.
func TestUpdateTradeConcurrentEditsBothSurvive(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	s := &server{}
	row, err := s.appendTrade(journal.Trade{Symbol: "ETH", Side: "short", Entry: 2000, Stop: 2100})
	if err != nil {
		t.Fatal(err)
	}

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.updateTrade(row.ID, func(r *journal.Trade) error { r.OpenNotes += "e"; return nil })
		}()
		go func() {
			defer wg.Done()
			_ = s.updateTrade(row.ID, func(r *journal.Trade) error { r.CloseNotes += "s"; return nil })
		}()
	}
	wg.Wait()

	trades, err := journal.ReadAll("")
	if err != nil {
		t.Fatal(err)
	}
	got := trades[0]
	if len(got.OpenNotes) != n || len(got.CloseNotes) != n {
		t.Fatalf("lost updates: open_notes=%d close_notes=%d, want %d each", len(got.OpenNotes), len(got.CloseNotes), n)
	}
}

func TestUpdateTradeErrorsWriteNothing(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	s := &server{}
	row, err := s.appendTrade(journal.Trade{Symbol: "SOL", Side: "long", Entry: 150, Stop: 140, OpenNotes: "before"})
	if err != nil {
		t.Fatal(err)
	}

	err = s.updateTrade(row.ID, func(r *journal.Trade) error {
		r.OpenNotes = "after"
		return errStaleEdit
	})
	if !errors.Is(err, errStaleEdit) {
		t.Fatalf("err = %v, want errStaleEdit", err)
	}
	if err := s.updateTrade(row.ID+99, func(*journal.Trade) error { return nil }); !errors.Is(err, errTradeNotFound) {
		t.Fatalf("missing row: err = %v, want errTradeNotFound", err)
	}

	trades, _ := journal.ReadAll("")
	if trades[0].OpenNotes != "before" {
		t.Fatalf("a refused update was written: open_notes=%q", trades[0].OpenNotes)
	}
}

func TestDeleteTradeIsIdempotent(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	s := &server{}
	a, _ := s.appendTrade(journal.Trade{Symbol: "BTC", Side: "long", Entry: 1, Stop: 0.9})
	b, _ := s.appendTrade(journal.Trade{Symbol: "ETH", Side: "long", Entry: 1, Stop: 0.9})

	if err := s.deleteTrade(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteTrade(a.ID); err != nil {
		t.Fatalf("second delete of the same row: %v", err)
	}
	trades, _ := journal.ReadAll("")
	if len(trades) != 1 || trades[0].ID != b.ID {
		t.Fatalf("after delete: %+v, want only #%d", trades, b.ID)
	}
}
