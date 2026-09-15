package autotrade

// Blocked-candidate log — the missing half of the allocation picture (C7).
//
// The firing path logs one line when the global caps reject a candidate:
//
//	autoexec: ETH 1h/htf-snr BLOCKED by caps — max-concurrent-total: 4 open >= cap 4
//
// which says a slot was denied but not WHAT was denied. Entry, stop, target
// and score are all gone, so there is no way to ask the question C7 exists to
// answer: would filling the four slots best-first have beaten first-come?
//
// Measured 2026-09-07, the accepted fires alone give r(score, R) = +0.31 over
// n=30 with a 95% CI of (-0.05, +0.60) — it includes zero, and 27 of the 30
// are ETH. That is not enough to build an allocator on, and it cannot be
// improved by analysis: the accepted set is exactly the set first-come already
// chose. The counterfactual needs the REJECTED candidates, and they were never
// written down.
//
// DELIBERATELY A SEPARATE FILE, not a flag on autotrade_paper.jsonl. Five
// things read the paper log (the /ops panel's P&L, BuildBook, the equity
// curve, the distribution chart, ad-hoc replays); adding a `blocked: true`
// row there would mean every one of them needs a filter, and the first one
// missed corrupts a displayed P&L. A file nothing reads yet cannot lie to
// anybody.

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// BlockedCandidate is a fire that passed every per-rule guard and was then
// denied a slot. Same shape as the fire that would have been placed, plus why.
type BlockedCandidate struct {
	Fire   PaperFire `json:"fire"`
	Reason string    `json:"reason"`
	// BarTime is the closed bar the candidate belongs to, so replays can
	// group same-tick competitors — which is the unit an allocator ranks.
	BarTime time.Time `json:"bar_time"`
	// OpenCount/OpenMargin are the book state that rejected it, so a replay
	// can reconstruct how full the book was without re-deriving it.
	OpenCount  int     `json:"open_count"`
	OpenMargin float64 `json:"open_margin"`
}

// BlockedLogPath is the on-disk JSONL store (env AUTOTRADE_BLOCKED_LOG).
func BlockedLogPath() string {
	if p := strings.TrimSpace(os.Getenv("AUTOTRADE_BLOCKED_LOG")); p != "" {
		return p
	}
	return "/opt/trading/autotrade_blocked.jsonl"
}

// AppendBlocked records one denied candidate (best-effort; never fatal).
func AppendBlocked(bc BlockedCandidate) {
	f, err := os.OpenFile(BlockedLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(bc)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}
