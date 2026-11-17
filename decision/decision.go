// Package decision is the take/skip log for machine-found candidates.
//
// WHY. A rule written down from a winning discretionary trade (level-reclaim,
// 2026-11-17) was a coin flip once it had to fire on every instance. What the
// rule could not carry was the trader's choice of WHICH instances to take.
// Whether that choice has an edge is unmeasured; this log measures it. The
// machine proposes, the trader taps 做 / 不做 before the outcome is known,
// and the taken, skipped and unanswered groups are scored on the same bars.
//
// HINDSIGHT is the failure mode. A decision made after the trade has already
// run is not a forecast, so: a candidate is decidable only until Deadline;
// the web refuses a decision on a fire that has already settled; and every
// record keeps how far price had moved (RAtDecision) so the analysis can
// check that "taken" did not simply mean "already working".
//
// The log is append-only JSONL; the LAST record for an id wins, so a
// mis-tap is corrected by deciding again before the deadline.
package decision

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/market"
)

// Since is when the log started: unanswered candidates are counted from here,
// so fires from before the feature existed are not "ignored".
var Since = time.Date(2026, 11, 17, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))

const (
	Take = "take"
	Skip = "skip"
)

// Reasons are fixed tags so they can be counted. The trader picks any number.
var TakeReasons = []string{"trend", "location", "structure", "other"}
var SkipReasons = []string{"countertrend", "extended", "session", "news", "structure", "chop", "other"}

// Record is one decision. Fire is a snapshot: the stats must not depend on
// the paper log's read depth.
type Record struct {
	ID          string              `json:"id"`
	Decision    string              `json:"decision"`
	Reasons     []string            `json:"reasons,omitempty"`
	DecidedAt   time.Time           `json:"decided_at"`
	Mark        float64             `json:"mark,omitempty"`          // live price when decided (0 = unknown)
	RAtDecision float64             `json:"r_at_decision,omitempty"` // (mark − entry) in R, trade direction
	Fire        autotrade.PaperFire `json:"fire"`
}

// ID is stable for a fire: the same row of the paper log always maps to it.
func ID(f autotrade.PaperFire) string {
	return fmt.Sprintf("%d-%s-%s-%s-%s", f.Time.Unix(), f.Symbol, f.TF, f.Strategy, f.Side)
}

// Deadline is the last moment a decision on f counts: one bar of its TF,
// clamped to [30m, 4h] — long enough to see it away from the screen, short
// enough that the bar it fired on is still the context it is judged in.
func Deadline(f autotrade.PaperFire) time.Time {
	d := market.BarDuration(market.Timeframe(f.TF))
	d = max(d, 30*time.Minute)
	d = min(d, 4*time.Hour)
	return f.Time.Add(d)
}

// RMoved is how far price has gone from entry, in R, in the trade's direction.
func RMoved(f autotrade.PaperFire, mark float64) float64 {
	risk := f.Entry - f.Stop
	if f.Side == "short" {
		risk = -risk
	}
	if risk <= 0 || mark <= 0 {
		return 0
	}
	if f.Side == "short" {
		return (f.Entry - mark) / risk
	}
	return (mark - f.Entry) / risk
}

// Path is the log file (env DECISIONS_PATH, default next to the paper log).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("DECISIONS_PATH")); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(autotrade.PaperLogPath()), "decisions.jsonl")
}

var mu sync.Mutex

// Append writes one record.
func Append(path string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o664)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.Write(append(b, '\n'))
	return err
}

// Latest reads the log and returns the last record per id. A missing file is
// an empty log; unreadable lines are skipped and counted.
func Latest(path string) (map[string]Record, int, error) {
	out := map[string]Record{}
	mu.Lock()
	b, err := os.ReadFile(path)
	mu.Unlock()
	if os.IsNotExist(err) {
		return out, 0, nil
	}
	if err != nil {
		return out, 0, err
	}
	bad := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == "" {
			bad++
			continue
		}
		out[r.ID] = r
	}
	return out, bad, sc.Err()
}

// Pending returns the fires still awaiting a decision at now, oldest first:
// logged since Since, deadline not passed, no record yet.
func Pending(fires []autotrade.PaperFire, decided map[string]Record, now time.Time) []autotrade.PaperFire {
	var out []autotrade.PaperFire
	for i := len(fires) - 1; i >= 0; i-- { // fires are newest-first
		f := fires[i]
		if f.Time.Before(Since) || !now.Before(Deadline(f)) {
			continue
		}
		if _, ok := decided[ID(f)]; ok {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Group labels a fire for the stats: take, skip, or "none" (deadline passed
// with no decision). ok is false for a fire still open for decision or from
// before Since — it belongs to no group yet.
func Group(f autotrade.PaperFire, decided map[string]Record, now time.Time) (string, bool) {
	if f.Time.Before(Since) {
		return "", false
	}
	if r, ok := decided[ID(f)]; ok {
		return r.Decision, true
	}
	if now.Before(Deadline(f)) {
		return "", false
	}
	return "none", true
}
