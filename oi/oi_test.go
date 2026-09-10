package oi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// base and the fixture spacing are chosen so every expected answer below has
// exactly one nearest snapshot — no ties to break.
var base = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func seed(t *testing.T) []Snapshot {
	t.Helper()
	t.Setenv("OI_LOG", filepath.Join(t.TempDir(), "oi.jsonl"))
	for _, s := range []Snapshot{
		{Time: base.Add(-2 * time.Hour), Symbol: "BTC-USDT", OI: 1000},
		{Time: base.Add(-70 * time.Minute), Symbol: "BTC-USDT", OI: 1100}, // 10:50
		{Time: base.Add(-55 * time.Minute), Symbol: "BTC-USDT", OI: 1150}, // 11:05
		{Time: base, Symbol: "BTC-USDT", OI: 1200},
		{Time: base.Add(-time.Hour), Symbol: "ETH-USDT", OI: 500},
	} {
		Append(s)
	}
	return Load()
}

func TestLoadSortsAndSkipsJunk(t *testing.T) {
	snaps := seed(t)
	if len(snaps) != 5 {
		t.Fatalf("Load() = %d snapshots, want 5", len(snaps))
	}
	for i := 1; i < len(snaps); i++ {
		if snaps[i].Time.Before(snaps[i-1].Time) {
			t.Fatalf("Load() not sorted oldest-first at %d: %v before %v",
				i, snaps[i].Time, snaps[i-1].Time)
		}
	}

	// A truncated write and a zero reading must both be dropped, not
	// surfaced as OI=0 (which would compute a -100% delta).
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"time\":\"2026-09-10T13:0\n")
	f.WriteString(`{"time":"2026-09-10T13:00:00Z","symbol":"BTC-USDT","oi":0}` + "\n")
	f.Close()

	if got := len(Load()); got != 5 {
		t.Errorf("Load() after junk = %d, want 5", got)
	}
}

func TestAppendRejectsUnusableSnapshots(t *testing.T) {
	seed(t)
	Append(Snapshot{Time: base, Symbol: "", OI: 900})        // no symbol
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: 0})  // no reading
	Append(Snapshot{Symbol: "BTC-USDT", OI: 900})            // no timestamp
	Append(Snapshot{Time: base, Symbol: "BTC-USDT", OI: -5}) // negative
	if got := len(Load()); got != 5 {
		t.Errorf("Load() = %d, want 5 — unusable snapshots must not be written", got)
	}
}

func TestPriorTo(t *testing.T) {
	snaps := seed(t)
	at := base.Add(-time.Hour) // 11:00

	cases := []struct {
		name   string
		sym    string
		at     time.Time
		tol    time.Duration
		wantOI float64
		wantOK bool
	}{
		// 10:50 is 10m away, 11:05 is 5m away -> 11:05 wins.
		{"nearest wins", "BTC-USDT", at, 20 * time.Minute, 1150, true},
		// Nearest is 5m out; a 3m tolerance must refuse rather than reach.
		{"tolerance refuses", "BTC-USDT", at, 3 * time.Minute, 0, false},
		{"exact hit", "ETH-USDT", at, 20 * time.Minute, 500, true},
		{"unknown symbol", "SOL-USDT", at, 20 * time.Minute, 0, false},
		{"zero tolerance", "BTC-USDT", at, 0, 0, false},
		{"empty symbol", "", at, 20 * time.Minute, 0, false},
		// The gap is absolute, not backward-only: a reading slightly LATER
		// than the requested instant is still a fair reading of "then".
		{"later snapshot counts", "BTC-USDT", base.Add(-75 * time.Minute), 10 * time.Minute, 1100, true},
	}
	for _, c := range cases {
		gotOI, gotOK := PriorTo(snaps, c.sym, c.at, c.tol)
		if gotOI != c.wantOI || gotOK != c.wantOK {
			t.Errorf("%s: PriorTo(%q, %v, %v) = (%.0f, %v), want (%.0f, %v)",
				c.name, c.sym, c.at, c.tol, gotOI, gotOK, c.wantOI, c.wantOK)
		}
	}

	if oi, ok := PriorTo(nil, "BTC-USDT", at, time.Hour); ok || oi != 0 {
		t.Errorf("PriorTo(nil) = (%.0f, %v), want (0, false)", oi, ok)
	}
}

func TestPrune(t *testing.T) {
	t.Setenv("OI_LOG", filepath.Join(t.TempDir(), "oi.jsonl"))
	now := base
	Append(Snapshot{Time: now.Add(-8 * 24 * time.Hour), Symbol: "BTC-USDT", OI: 900}) // outside Retain
	Append(Snapshot{Time: now.Add(-time.Hour), Symbol: "BTC-USDT", OI: 1000})

	if err := Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	snaps := Load()
	if len(snaps) != 1 || snaps[0].OI != 1000 {
		t.Fatalf("after Prune, Load() = %+v, want only the 1h-old reading", snaps)
	}

	// Nothing to drop: a second pass must be a no-op, not a rewrite that
	// loses data.
	if err := Prune(now); err != nil {
		t.Fatalf("Prune (no-op): %v", err)
	}
	if got := len(Load()); got != 1 {
		t.Errorf("Load() after no-op Prune = %d, want 1", got)
	}
	if _, err := os.Stat(Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("Prune left a .tmp behind")
	}
}

func TestPruneMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("OI_LOG", filepath.Join(t.TempDir(), "absent.jsonl"))
	if err := Prune(base); err != nil {
		t.Errorf("Prune on missing store = %v, want nil", err)
	}
}

func TestPrevFor(t *testing.T) {
	snaps := seed(t)
	// One 1h bar back from 12:00 is 11:00, tolerance 15m: 11:05 (OI 1150)
	// is the nearest reading inside it.
	if got := PrevFor(snaps, "BTC-USDT", time.Hour, base); got != 1150 {
		t.Errorf("PrevFor(1h) = %.0f, want 1150", got)
	}
	// A 15m bar gives a 3m45s tolerance around 11:45 — nothing is that
	// close, so it must report zero rather than reach for 11:05.
	if got := PrevFor(snaps, "BTC-USDT", 15*time.Minute, base); got != 0 {
		t.Errorf("PrevFor(15m) = %.0f, want 0 (no reading within bar/4)", got)
	}
	if got := PrevFor(snaps, "BTC-USDT", 0, base); got != 0 {
		t.Errorf("PrevFor(bar=0) = %.0f, want 0", got)
	}
	if got := PrevFor(nil, "BTC-USDT", time.Hour, base); got != 0 {
		t.Errorf("PrevFor(empty store) = %.0f, want 0", got)
	}
}
