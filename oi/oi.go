// Package oi persists open-interest snapshots so the engine can compare
// current OI against a prior reading.
//
// Why this exists. signal.Context has carried PrevOpenInterest since the
// 2026-05-28 OI-semantics review, and annotateContextWarnings acts on it:
//
//	OI falling on a down-move  = longs unwinding      -> confirms a short
//	OI rising  on a down-move  = new shorts piling in -> squeeze risk
//
// That second line is "散戶在地板空,有機會往上桶" expressed as a measurement.
// The logic shipped; the field never did. `PrevOpenInterest` appears in
// exactly three places in the tree — its declaration and the two reads inside
// engine.go — so `ctx.PrevOpenInterest > 0` has been false on every call ever
// made and neither warning has fired once. Current OI *is* supplied by all
// three callers; only the prior reading was missing.
//
// It has to be on disk. The engine is evaluated from short-lived processes —
// a one-shot cmd/analyze run, a per-request web handler — that have no memory
// of the previous bar. Only the monitor daemon is long-lived, so the daemon
// samples and everyone else reads.
//
// Fail-safe direction: when no snapshot exists near the requested time,
// PriorTo reports not-found and the caller leaves PrevOpenInterest at zero,
// which is exactly today's behaviour (warnings silent). A store with a gap
// must never invent a delta — a fabricated "OI up 8%" would read as evidence.
package oi

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// Snapshot is one open-interest reading, with the funding rate observed at
// the same instant.
//
// Funding rides along because the two only mean something together. OI says
// how many positions exist, funding says which side is paying to hold them:
// OI falling on a down-move is a long unwind, but OI RISING while funding
// goes negative is shorts piling in — the "everyone is short the floor" setup
// — and neither number alone separates those. It also costs one extra public
// GET on a loop that is already making one, and a series beats the snapshot
// the venue hands out (funding sliding for three hours is the signal; a
// single print is not).
//
// omitempty on Funding so rows written before this field existed, and rows
// where the funding fetch failed while the OI one succeeded, stay
// distinguishable from a genuine zero.
type Snapshot struct {
	Time    time.Time `json:"time"`
	Symbol  string    `json:"symbol"`            // exchange symbol, e.g. "BTC-USDT"
	OI      float64   `json:"oi"`                // quote-currency (USDT) notional
	Funding float64   `json:"funding,omitempty"` // fraction per interval; >0 = longs pay shorts
	// Price is the mark price at the same instant. Recorded because open
	// interest alone cannot say which side the new positions are on: OI
	// rising means contracts were opened, and only the PRICE direction over
	// the same window says whether buyers or sellers opened them. Free to
	// collect — the funding call already returns it.
	Price float64 `json:"price,omitempty"`
}

// Retain is how much history Prune keeps. Seven days covers every timeframe
// the engine evaluates with room to spare, and bounds the file at roughly
// 16k lines for a 14-symbol roster on a 5-minute tick — small enough that
// readers can load the whole thing.
const Retain = 7 * 24 * time.Hour

// Path is the on-disk JSONL store (env OI_LOG or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("OI_LOG")); p != "" {
		return p
	}
	return "/opt/trading/oi.jsonl"
}

// Append writes one snapshot (best-effort; never fatal). A dropped sample
// costs one comparison point, so it must not take the daemon down with it.
func Append(s Snapshot) {
	if s.Symbol == "" || s.OI <= 0 || s.Time.IsZero() {
		return
	}
	f, err := os.OpenFile(Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
}

// Load returns every snapshot in the store, oldest first. A missing file is
// not an error — it is the state before the sampler has run once.
func Load() []Snapshot {
	f, err := os.Open(Path())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Snapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s Snapshot
		if json.Unmarshal([]byte(line), &s) == nil && s.OI > 0 {
			out = append(out, s)
		}
	}
	_ = sc.Err()
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// PriorTo returns the OI reading for sym nearest to `at`, provided it is
// within tol of it.
//
// Nearest-within-tolerance rather than exact-match because the two clocks do
// not line up: the sampler ticks on wall time while bars close on exchange
// time, so a snapshot exactly one bar old will essentially never exist. tol
// is what makes the answer honest — it says how stale a reading the caller is
// willing to treat as "then".
func PriorTo(snaps []Snapshot, sym string, at time.Time, tol time.Duration) (float64, bool) {
	if len(snaps) == 0 || sym == "" || tol <= 0 {
		return 0, false
	}
	at = at.UTC()
	best, bestGap := 0.0, time.Duration(-1)
	for _, s := range snaps {
		if s.Symbol != sym {
			continue
		}
		gap := s.Time.UTC().Sub(at)
		if gap < 0 {
			gap = -gap
		}
		if gap > tol {
			continue
		}
		if bestGap < 0 || gap < bestGap {
			best, bestGap = s.OI, gap
		}
	}
	if bestGap < 0 {
		return 0, false
	}
	return best, true
}

// Prune rewrites the store keeping only snapshots newer than now-Retain.
//
// tmp -> rename so a reader never sees a half-written file: Load treats a
// truncated final line as a skippable parse failure, but a reader that opened
// the file mid-truncate would silently see an empty history and report "no
// prior OI" instead of a real delta.
func Prune(now time.Time) error {
	snaps := Load()
	if len(snaps) == 0 {
		return nil
	}
	cutoff := now.UTC().Add(-Retain)
	kept := make([]Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if s.Time.UTC().After(cutoff) {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(snaps) {
		return nil
	}

	p := Path()
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, s := range kept {
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// PrevFor is the reading callers hand to signal.Context.PrevOpenInterest:
// open interest one `bar` ago, or zero when the store cannot answer.
//
// Tolerance is bar/4 — tight enough that "an hour ago" means roughly an hour
// ago rather than any reading in the neighbourhood, since the engine acts on a
// 2% delta and a comparison point half a bar off would distort it.
//
// A consequence worth stating: on timeframes shorter than four sample
// intervals, nothing will ever land inside the tolerance and this returns 0,
// leaving the crowding warnings silent on those timeframes. That is the
// intended failure. Widening the window to force an answer would mean
// reporting a delta measured over the wrong span, and a wrong number here
// reads as evidence about positioning.
func PrevFor(snaps []Snapshot, sym string, bar time.Duration, now time.Time) float64 {
	if bar <= 0 {
		return 0
	}
	v, ok := PriorTo(snaps, sym, now.Add(-bar), bar/4)
	if !ok {
		return 0
	}
	return v
}

// Latest returns the most recent snapshot held for sym.
//
// Used as "now" by readers that must not make network calls — the /ops card
// renders the whole roster from this, so a page load costs zero requests and
// cannot be rate-limited. The tradeoff is that "now" is as old as the last
// sample, which is why callers show the timestamp rather than implying the
// reading is live.
func Latest(snaps []Snapshot, sym string) (Snapshot, bool) {
	var best Snapshot
	found := false
	for _, s := range snaps {
		if s.Symbol != sym {
			continue
		}
		if !found || s.Time.After(best.Time) {
			best, found = s, true
		}
	}
	return best, found
}

// PriceChangeOver returns the fractional mark-price change for sym across the
// same window PrevFor compares OI over, and whether both endpoints existed.
//
// This is the half that makes an OI delta mean anything. OI rising says
// contracts were opened; it does not say by whom. Price falling while OI
// rises means the opening pressure was on the sell side (shorts building);
// price rising while OI rises means the opposite. The engine gets away with
// reading OI alone because its warnings only fire under an existing Long or
// Short signal, which supplies the direction. A standalone readout has no
// such signal and must measure it.
func PriceChangeOver(snaps []Snapshot, sym string, bar time.Duration, now time.Time) (float64, bool) {
	if bar <= 0 {
		return 0, false
	}
	cur, ok := Latest(snaps, sym)
	if !ok || cur.Price <= 0 {
		return 0, false
	}
	prevSnap, ok := priorSnapshot(snaps, sym, now.Add(-bar), bar/4)
	if !ok || prevSnap.Price <= 0 {
		return 0, false
	}
	return (cur.Price - prevSnap.Price) / prevSnap.Price, true
}

// priorSnapshot is PriorTo's logic returning the whole row. PriorTo stays as
// it is because signal.Context only ever wanted the one number.
func priorSnapshot(snaps []Snapshot, sym string, at time.Time, tol time.Duration) (Snapshot, bool) {
	if len(snaps) == 0 || sym == "" || tol <= 0 {
		return Snapshot{}, false
	}
	at = at.UTC()
	var best Snapshot
	bestGap := time.Duration(-1)
	for _, s := range snaps {
		if s.Symbol != sym {
			continue
		}
		gap := s.Time.UTC().Sub(at)
		if gap < 0 {
			gap = -gap
		}
		if gap > tol {
			continue
		}
		if bestGap < 0 || gap < bestGap {
			best, bestGap = s, gap
		}
	}
	if bestGap < 0 {
		return Snapshot{}, false
	}
	return best, true
}
