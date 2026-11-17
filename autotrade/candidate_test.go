package autotrade

import (
	"path/filepath"
	"testing"
	"time"
)

// A candidate fire must be invisible to every paper-log reader (book, caps,
// breaker, balance, panel) and visible to the candidate readers.
func TestCandidateLogIsSeparate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUTOTRADE_PAPER_LOG", filepath.Join(dir, "paper.jsonl"))
	t.Setenv("AUTOTRADE_CANDIDATE_LOG", "")
	at := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	AppendFire(PaperFire{Time: at, Symbol: "BTC", Strategy: "htf-trend", Side: "long", Entry: 1, Stop: 0.9})
	AppendCandidate(PaperFire{Time: at.Add(time.Hour), Symbol: "ETH", Strategy: "engine", Side: "short", Entry: 2, Stop: 2.1, TP: 1.8})
	AppendCandidate(PaperFire{Time: at.Add(2 * time.Hour), Symbol: "ETH", Strategy: "engine", Side: "long", Entry: 2, Stop: 1.9, TP: 2.2})

	if got := CandidateLogPath(); got != filepath.Join(dir, "autotrade_candidates.jsonl") {
		t.Fatalf("candidate log %s, want next to the paper log", got)
	}
	for _, f := range append(ReadFires(100), ReadFiresWithOpenTrails(100)...) {
		if f.Candidate || f.Symbol == "ETH" {
			t.Fatalf("paper reader saw a candidate: %+v", f)
		}
	}
	if _, ok := NewestFire("ETH", "engine"); ok {
		t.Fatal("NewestFire (paper slot lookup) saw a candidate")
	}
	cs := ReadCandidates(10)
	if len(cs) != 2 || !cs[0].Candidate || cs[0].Side != "long" {
		t.Fatalf("ReadCandidates = %+v, want 2 newest-first, flagged", cs)
	}
	if f, ok := NewestCandidate("ETH", "engine"); !ok || f.Side != "long" || !f.Candidate {
		t.Fatalf("NewestCandidate = %+v %v", f, ok)
	}
	if _, ok := NewestCandidate("BTC", "htf-trend"); ok {
		t.Fatal("candidate lookup saw a paper fire")
	}
}
