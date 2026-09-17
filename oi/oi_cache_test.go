package oi

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Load memoizes the parsed store and resumes from a byte offset, because the
// file is append-only and re-parsing ~3MB on every call was costing seconds of
// CPU per web request. These tests pin the cases where "resume from an offset"
// could go wrong: an append, a half-written record, a rewrite, and concurrent
// readers.

// tmpStore points OI_LOG at an empty per-test file and drops any cache state
// left by an earlier test.
func tmpStore(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oi.jsonl")
	t.Setenv("OI_LOG", p)
	invalidateLoadCache()
	t.Cleanup(invalidateLoadCache)
	return p
}

func TestLoadPicksUpAppendsWithoutDuplicating(t *testing.T) {
	tmpStore(t)
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: 1000})

	first := Load()
	if len(first) != 1 {
		t.Fatalf("after 1 append: %d snapshots, want 1", len(first))
	}
	// A second call with nothing appended must not re-read or re-add.
	if again := Load(); len(again) != 1 {
		t.Fatalf("second Load with no change: %d snapshots, want 1", len(again))
	}

	Append(Snapshot{Time: base.Add(time.Hour), Symbol: "BTC-USDT", OI: 1100})
	got := Load()
	if len(got) != 2 {
		t.Fatalf("after 2nd append: %d snapshots, want 2 (the append must be picked up exactly once)", len(got))
	}
	if got[0].OI != 1000 || got[1].OI != 1100 {
		t.Errorf("got OI %v, %v; want 1000, 1100 in time order", got[0].OI, got[1].OI)
	}
}

// The sampler writes a record with one Write, but a reader can still arrive
// between the bytes and the newline. A partial line must be left unconsumed —
// not parsed, and not skipped over — so the completed record still lands.
//
// The load MUST contain a complete record ahead of the partial one. With only
// a partial line the tail holds no newline at all and an earlier guard returns
// before the offset is touched, so that case cannot tell a correct
// implementation from one that consumes the whole tail.
func TestLoadLeavesPartialTrailingLineForNextCall(t *testing.T) {
	p := tmpStore(t)
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: 1000})
	if n := len(Load()); n != 1 {
		t.Fatalf("baseline: %d, want 1", n)
	}

	whole := `{"time":"2026-09-10T13:00:00Z","symbol":"BTC-USDT","oi":1500}` + "\n"
	half := `{"time":"2026-09-10T14:00:00Z","symbol":"BTC-USDT","oi":`
	rest := "2000}\n"

	// One write carrying a complete record followed by a half-written one.
	appendRaw(t, p, whole+half)

	got := Load()
	if len(got) != 2 {
		t.Fatalf("complete + partial record: %d snapshots, want 2 (the complete one only)", len(got))
	}
	if got[1].OI != 1500 {
		t.Errorf("second snapshot OI = %v, want 1500", got[1].OI)
	}

	// Completing the record must surface it — i.e. the offset stopped at the
	// newline and did not run past the partial bytes.
	appendRaw(t, p, rest)

	got = Load()
	if len(got) != 3 {
		t.Fatalf("after the record was completed: %d snapshots, want 3 — the offset skipped past a partial line", len(got))
	}
	if got[2].OI != 2000 {
		t.Errorf("completed record OI = %v, want 2000", got[2].OI)
	}
}

func appendRaw(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A shrunken file means the store was replaced, so the offset no longer refers
// to the same bytes and the cache must be rebuilt from zero.
func TestLoadRebuildsWhenFileShrinks(t *testing.T) {
	p := tmpStore(t)
	for i := 0; i < 5; i++ {
		Append(Snapshot{Time: base.Add(time.Duration(i) * time.Hour), Symbol: "BTC-USDT", OI: float64(1000 + i)})
	}
	if n := len(Load()); n != 5 {
		t.Fatalf("baseline: %d, want 5", n)
	}

	if err := os.WriteFile(p, []byte(`{"time":"2026-09-10T12:00:00Z","symbol":"ETH-USDT","oi":77}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if len(got) != 1 {
		t.Fatalf("after the file was replaced with one record: %d snapshots, want 1 (stale cache)", len(got))
	}
	if got[0].Symbol != "ETH-USDT" || got[0].OI != 77 {
		t.Errorf("got %+v, want the ETH-USDT/77 record", got[0])
	}
}

// Prune rewrites the store rather than appending to it.
//
// Note what this does NOT pin: Prune only ever removes records, so the file
// shrinks and TestLoadRebuildsWhenFileShrinks' size check would catch a stale
// cache on its own. Prune's invalidateLoadCache call is for a future caller
// that replaces the store WITHOUT shrinking it, which no code does today.
// TestInvalidateLoadCacheForcesAFullReread covers the primitive instead of
// pretending this test covers its use here.
func TestLoadAfterPruneSeesThePrunedFile(t *testing.T) {
	tmpStore(t)
	now := base.Add(Retain).Add(time.Hour) // makes `base` older than the cutoff
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: 1000})
	Append(Snapshot{Time: now.Add(-time.Minute), Symbol: "BTC-USDT", OI: 1100})

	if n := len(Load()); n != 2 {
		t.Fatalf("baseline: %d, want 2", n)
	}
	if err := Prune(now); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if len(got) != 1 {
		t.Fatalf("after Prune: %d snapshots, want 1 — Load returned a pre-prune cache", len(got))
	}
	if got[0].OI != 1100 {
		t.Errorf("kept OI = %v, want 1100 (the recent one)", got[0].OI)
	}
}

// A same-size replacement is invisible to the size check, so a caller that
// swaps the store's contents has to say so.
func TestInvalidateLoadCacheForcesAFullReread(t *testing.T) {
	p := tmpStore(t)
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: 1000})
	if got := Load(); len(got) != 1 || got[0].Symbol != "BTC-USDT" {
		t.Fatalf("baseline: %+v", got)
	}

	// Same byte length, different contents — the size check cannot see this.
	swapped := `{"time":"2026-09-10T12:00:00Z","symbol":"ETH-USDT","oi":1000}` + "\n"
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(swapped) != len(orig) {
		t.Skipf("fixture no longer byte-equal (%d vs %d); adjust the record", len(swapped), len(orig))
	}
	if err := os.WriteFile(p, []byte(swapped), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got[0].Symbol != "BTC-USDT" {
		t.Fatalf("precondition: a same-size swap should be invisible, got %v", got[0].Symbol)
	}

	invalidateLoadCache()
	got := Load()
	if len(got) != 1 || got[0].Symbol != "ETH-USDT" {
		t.Errorf("after invalidateLoadCache: %+v, want the ETH-USDT record", got)
	}
}

// The slice is shared between callers, so an append by one of them must not be
// able to write into the cache's backing array.
func TestLoadReturnsSliceThatCannotBeAppendedInto(t *testing.T) {
	tmpStore(t)
	for i := 0; i < 3; i++ {
		Append(Snapshot{Time: base.Add(time.Duration(i) * time.Hour), Symbol: "BTC-USDT", OI: float64(1000 + i)})
	}
	got := Load()
	if len(got) == 0 {
		t.Fatal("no snapshots")
	}
	if cap(got) != len(got) {
		t.Fatalf("cap %d != len %d: a caller's append would write into the shared cache", cap(got), len(got))
	}
	// Doing exactly that must not disturb the next Load.
	_ = append(got, Snapshot{Time: base, Symbol: "XXX", OI: 1})
	for _, s := range Load() {
		if s.Symbol == "XXX" {
			t.Fatal("a caller's append leaked into the cached slice")
		}
	}
}

func TestLoadIsSafeUnderConcurrentCallers(t *testing.T) {
	tmpStore(t)
	for i := 0; i < 20; i++ {
		Append(Snapshot{Time: base.Add(time.Duration(i) * time.Minute), Symbol: "BTC-USDT", OI: float64(1000 + i)})
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n := len(Load()); n != 20 {
				t.Errorf("concurrent Load = %d snapshots, want 20", n)
			}
		}()
	}
	wg.Wait()
}
