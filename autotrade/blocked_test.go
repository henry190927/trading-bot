package autotrade

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendBlockedRoundTrips(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blocked.jsonl")
	t.Setenv("AUTOTRADE_BLOCKED_LOG", p)
	if BlockedLogPath() != p {
		t.Fatalf("BlockedLogPath = %q, want the env override", BlockedLogPath())
	}

	bar := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	in := BlockedCandidate{
		Fire: PaperFire{
			Time: bar.Add(time.Minute), Symbol: "ETH", TF: "1h", Strategy: "htf-snr",
			Side: "long", Entry: 2503.49, Stop: 2493.49, TP: 2523.49,
			Qty: 3.55, Margin: 35, Lev: 125, Why: "htf-snr retest", Score: 77,
		},
		Reason: "max-concurrent-total: 4 open >= cap 4", BarTime: bar,
		OpenCount: 4, OpenMargin: 140,
	}
	AppendBlocked(in)
	AppendBlocked(in)

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []BlockedCandidate
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var bc BlockedCandidate
		if err := json.Unmarshal(sc.Bytes(), &bc); err != nil {
			t.Fatalf("row does not round-trip: %v", err)
		}
		got = append(got, bc)
	}
	if len(got) != 2 {
		t.Fatalf("wrote 2 rows, read %d — the writer must APPEND, not truncate", len(got))
	}
	g := got[0]
	// The whole point is that entry/stop/target/score survive, since the log
	// line this replaces had none of them.
	if g.Fire.Entry != 2503.49 || g.Fire.Stop != 2493.49 || g.Fire.TP != 2523.49 {
		t.Errorf("prices lost: entry %v stop %v tp %v", g.Fire.Entry, g.Fire.Stop, g.Fire.TP)
	}
	if g.Fire.Score != 77 {
		t.Errorf("Score = %v, want 77 — the ranking key cannot be the thing that goes missing", g.Fire.Score)
	}
	if !g.BarTime.Equal(bar) {
		t.Errorf("BarTime = %v, want %v (same-tick competitors are grouped by it)", g.BarTime, bar)
	}
	if g.OpenCount != 4 || g.OpenMargin != 140 {
		t.Errorf("book state lost: %d / %v", g.OpenCount, g.OpenMargin)
	}
	if g.Reason == "" {
		t.Error("Reason must be kept — which cap bound matters for the replay")
	}
}

// The blocked log must be a DIFFERENT file from the paper log. Five consumers
// read the paper log (panel P&L, BuildBook, equity curve, distribution,
// ad-hoc replays); a blocked row appearing there would need a filter in every
// one of them, and the first one missed reports a corrupted P&L.
func TestBlockedLogIsSeparateFromPaperLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUTOTRADE_PAPER_LOG", filepath.Join(dir, "paper.jsonl"))
	t.Setenv("AUTOTRADE_BLOCKED_LOG", filepath.Join(dir, "blocked.jsonl"))
	if PaperLogPath() == BlockedLogPath() {
		t.Fatal("the two logs must never resolve to the same path")
	}

	AppendBlocked(BlockedCandidate{Fire: PaperFire{Symbol: "ETH", Entry: 1}, Reason: "x"})
	// The paper log must not exist yet — nothing wrote to it.
	if _, err := os.Stat(PaperLogPath()); err == nil {
		t.Error("AppendBlocked touched the paper log")
	}
	// And ReadFires (which reads the paper log) must see nothing.
	if got := ReadFires(10); len(got) != 0 {
		t.Errorf("ReadFires returned %d rows from an untouched paper log", len(got))
	}
}

// Defaults must not collide either, in case both env vars are unset in prod.
func TestBlockedLogDefaultPathDiffersFromPaper(t *testing.T) {
	t.Setenv("AUTOTRADE_PAPER_LOG", "")
	t.Setenv("AUTOTRADE_BLOCKED_LOG", "")
	if PaperLogPath() == BlockedLogPath() {
		t.Fatalf("default paths collide: %q", PaperLogPath())
	}
}
