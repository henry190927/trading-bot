package autotrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// CandidateLogPath is the candidate_only rules' fire log (env
// AUTOTRADE_CANDIDATE_LOG, default next to the paper log). A separate file
// rather than a flag in the paper log: every book, cap, breaker and balance
// reader reads the paper log, and keeping candidates out of it makes "a
// candidate can never affect the book" true by construction rather than by
// every reader remembering a filter.
func CandidateLogPath() string {
	if p := strings.TrimSpace(os.Getenv("AUTOTRADE_CANDIDATE_LOG")); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(PaperLogPath()), "autotrade_candidates.jsonl")
}

// AppendCandidate writes one candidate fire (Candidate is forced true).
func AppendCandidate(pf PaperFire) {
	pf.Candidate = true
	f, err := os.OpenFile(CandidateLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(pf)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// ReadCandidates returns the newest `limit` candidate fires, newest first.
func ReadCandidates(limit int) []PaperFire {
	all := readAllFiresAt(CandidateLogPath())
	out := make([]PaperFire, 0, min(limit, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		f := all[i]
		f.Candidate = true
		out = append(out, f)
	}
	return out
}

// NewestCandidate is the rule's newest candidate fire, for its one-position
// check (the candidate rule holds its own slot in its own log).
func NewestCandidate(symbol, strategy string) (PaperFire, bool) {
	all := readAllFiresAt(CandidateLogPath())
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Symbol == symbol && all[i].Strategy == strategy {
			f := all[i]
			f.Candidate = true
			return f, true
		}
	}
	return PaperFire{}, false
}
