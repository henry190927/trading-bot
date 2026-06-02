// Package journal is the data layer for trade records. Both the journal
// CLI and the web UI import this package so they share schema, parsing,
// time semantics, and update logic exactly.
package journal

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schema is versioned-by-column-count and auto-migrates on read:
//
//	v1 = 16 cols (legacy: no analyzed_at, no score)
//	v2 = 17 cols (adds analyzed_at after opened_at)
//	v3 = 18 cols (adds score after tf)
//	v4 = 19 cols (adds leverage at end)
//	v5 = 20 cols (adds filled_at at end) — current
var Header = []string{
	"id", "opened_at", "analyzed_at", "closed_at", "symbol", "side", "tf", "score",
	"entry", "stop", "tp1", "tp2",
	"anchor", "open_notes",
	"exit_price", "outcome", "r_realized", "close_notes",
	"leverage",
	"filled_at",
}

// Trade is one journal row.
type Trade struct {
	ID         int
	OpenedAt   time.Time
	AnalyzedAt time.Time
	ClosedAt   time.Time // zero = still open
	Symbol     string
	Side       string // "long" / "short"
	TF         string // analysis timeframe(s) — accepts comma-separated, e.g. "15m,1h"
	Score      string // free-form, see Header comment for conventions
	Entry      float64
	Stop       float64
	TP1        float64
	TP2        float64
	Anchor     string
	OpenNotes  string
	ExitPrice  float64
	Outcome    string // "tp1", "tp2", "stop", "manual", "timeout", "no-fill", "" if open
	RRealized  float64
	CloseNotes string
	// FilledAt is the time the entry price was first reached after the
	// trade was recorded. Zero = still pending (entry not yet triggered).
	// Set by the dashboard's fill-detection sweep on each refresh.
	FilledAt time.Time
	// Leverage is the position multiplier used on the perp exchange. Optional
	// (0 = not recorded). Used to translate R-multiples into actual % return:
	// a 1R win on 100x leverage gross-returns about 100× the price-move %.
	// Doesn't affect R computation — R is leverage-independent by definition.
	Leverage int
}

// IsOpen reports whether the trade is still open (no close time set).
func (t Trade) IsOpen() bool { return t.ClosedAt.IsZero() }

// IsNoFill reports whether the plan never filled — recorded for
// discipline tracking (signal-to-fill ratio) without affecting WR/R
// stats. R should be exactly 0 for no-fills.
func (t Trade) IsNoFill() bool { return t.Outcome == "no-fill" }

// IsPending reports whether the trade has been recorded but its entry
// price hasn't been touched yet — i.e. position never opened on the
// exchange. Distinct from IsOpen (still in the not-yet-closed bucket).
// A trade goes Pending → Active (filled, position live) → Closed.
func (t Trade) IsPending() bool { return t.IsOpen() && t.FilledAt.IsZero() && !t.IsNoFill() }

// IsActive reports whether the entry has filled but the position
// hasn't been closed yet — i.e. the trade is currently live in the
// market and progress against stop/TP is meaningful.
func (t Trade) IsActive() bool { return t.IsOpen() && !t.FilledAt.IsZero() }

// DefaultPath returns $JOURNAL_PATH or ./journal.csv.
func DefaultPath() string {
	if p := os.Getenv("JOURNAL_PATH"); p != "" {
		return p
	}
	return "journal.csv"
}

// ReadAll loads all trades from the journal CSV at path (or DefaultPath if empty).
func ReadAll(path string) ([]Trade, error) {
	if path == "" {
		path = DefaultPath()
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]Trade, 0, len(rows)-1)
	for i, row := range rows[1:] {
		t, err := parseRow(row)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+2, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// WriteAll rewrites the journal CSV at path (or DefaultPath) with all trades.
func WriteAll(path string, trades []Trade) error {
	if path == "" {
		path = DefaultPath()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(Header); err != nil {
		return err
	}
	for _, t := range trades {
		if err := w.Write(rowFromTrade(t)); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// NextID returns the next unique ID for an Add operation.
func NextID(trades []Trade) int {
	next := 1
	for _, t := range trades {
		if t.ID >= next {
			next = t.ID + 1
		}
	}
	return next
}

// FindOpenBySymbol returns the index of the most-recently-opened open trade
// matching sym, or -1 if none.
func FindOpenBySymbol(trades []Trade, sym string) int {
	sym = strings.ToUpper(sym)
	best := -1
	var bestAt time.Time
	for i, t := range trades {
		if t.Symbol == sym && t.IsOpen() {
			if best < 0 || t.OpenedAt.After(bestAt) {
				best = i
				bestAt = t.OpenedAt
			}
		}
	}
	return best
}

// FindByID returns the index of trade with id, or -1.
func FindByID(trades []Trade, id int) int {
	for i, t := range trades {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// RealizedR computes the R-multiple of t against the given exit price.
// Returns 0 if risk is zero.
func RealizedR(t Trade, exit float64) float64 {
	risk := math.Abs(t.Entry - t.Stop)
	if risk == 0 {
		return 0
	}
	if t.Side == "long" {
		return (exit - t.Entry) / risk
	}
	return (t.Entry - exit) / risk
}

// ApplyUpdate mutates t with field=value. r_realized is auto-recomputed by
// callers after entry/stop/exit changes on closed trades.
func ApplyUpdate(t *Trade, field, value string) error {
	parseF := func() (float64, error) { return strconv.ParseFloat(value, 64) }
	switch field {
	case "score":
		t.Score = value
	case "analyzed_at", "analyzed-at", "at":
		ts, err := ParseTimeSpec(value, time.Now())
		if err != nil {
			return fmt.Errorf("analyzed_at: %w", err)
		}
		t.AnalyzedAt = ts
	case "opened_at", "opened-at", "opened":
		ts, err := ParseTimeSpec(value, time.Now())
		if err != nil {
			return fmt.Errorf("opened_at: %w", err)
		}
		t.OpenedAt = ts
	case "closed_at", "closed-at", "closed":
		ts, err := ParseTimeSpec(value, time.Now())
		if err != nil {
			return fmt.Errorf("closed_at: %w", err)
		}
		t.ClosedAt = ts
	case "side":
		v := strings.ToLower(value)
		if v != "long" && v != "short" {
			return fmt.Errorf("side must be long|short, got %q", value)
		}
		t.Side = v
	case "tf":
		t.TF = value
	case "entry":
		v, err := parseF()
		if err != nil {
			return fmt.Errorf("entry: %w", err)
		}
		t.Entry = v
	case "stop":
		v, err := parseF()
		if err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		t.Stop = v
	case "tp1":
		v, err := parseF()
		if err != nil {
			return fmt.Errorf("tp1: %w", err)
		}
		t.TP1 = v
	case "tp2":
		v, err := parseF()
		if err != nil {
			return fmt.Errorf("tp2: %w", err)
		}
		t.TP2 = v
	case "anchor":
		t.Anchor = value
	case "open_notes", "open-notes", "notes":
		t.OpenNotes = value
	case "exit_price", "exit":
		v, err := parseF()
		if err != nil {
			return fmt.Errorf("exit_price: %w", err)
		}
		t.ExitPrice = v
	case "outcome":
		v := strings.ToLower(value)
		switch v {
		case "tp1", "tp2", "stop", "manual", "timeout":
		default:
			return fmt.Errorf("outcome must be tp1|tp2|stop|manual|timeout, got %q", value)
		}
		t.Outcome = v
	case "close_notes", "close-notes":
		t.CloseNotes = value
	case "leverage", "lev":
		if value == "" {
			t.Leverage = 0
			break
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("leverage: %w", err)
		}
		if n < 0 || n > 500 {
			return fmt.Errorf("leverage must be 0-500, got %d", n)
		}
		t.Leverage = n
	default:
		return fmt.Errorf("unknown field %q", field)
	}
	return nil
}

// ParseTimeSpec converts a user time spec to a time. Accepts:
//
//	"now", "-2h", "-30m", "-1d", RFC3339 with Z or +08:00,
//	"2006-01-02T15:04[:05]" (naive → local), "2006-01-02 15:04[:05]" (naive → local),
//	"2006-01-02" (date only → local 00:00)
func ParseTimeSpec(spec string, now time.Time) (time.Time, error) {
	now = now.UTC()
	if spec == "" || spec == "now" {
		return now, nil
	}
	if strings.HasPrefix(spec, "-") {
		if strings.HasSuffix(spec, "d") {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(spec, "-"), "d"))
			if err != nil {
				return time.Time{}, fmt.Errorf("bad day duration %q: %w", spec, err)
			}
			return now.Add(time.Duration(-n) * 24 * time.Hour), nil
		}
		dur, err := time.ParseDuration(spec)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad duration %q: %w", spec, err)
		}
		return now.Add(dur), nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, spec, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"could not parse %q (try: now | -2h/-30m/-1d | 2026-05-24 17:15 | 2026-05-24T17:15 | 2026-05-24T17:15:00Z | 2026-05-24T17:15:00+08:00)",
		spec)
}

// --- CSV row codec --------------------------------------------------------

func parseRow(row []string) (Trade, error) {
	switch len(row) {
	case 16:
		return parseRowV1(row), nil
	case 17:
		return parseRowV2(row), nil
	case 18:
		return parseRowV3(row), nil
	case 19:
		return parseRowV4(row), nil
	case 20:
		return parseRowV5(row), nil
	}
	return Trade{}, fmt.Errorf("expected 16/17/18/19/20 columns, got %d", len(row))
}

func parseRowV1(row []string) Trade {
	id, _ := strconv.Atoi(row[0])
	openedAt, _ := time.Parse(time.RFC3339, row[1])
	var closedAt time.Time
	if row[2] != "" {
		closedAt, _ = time.Parse(time.RFC3339, row[2])
	}
	entry, _ := strconv.ParseFloat(row[6], 64)
	stop, _ := strconv.ParseFloat(row[7], 64)
	tp1, _ := strconv.ParseFloat(row[8], 64)
	tp2, _ := strconv.ParseFloat(row[9], 64)
	var exit, rR float64
	if row[12] != "" {
		exit, _ = strconv.ParseFloat(row[12], 64)
	}
	if row[14] != "" {
		rR, _ = strconv.ParseFloat(row[14], 64)
	}
	return Trade{
		ID: id, OpenedAt: openedAt, AnalyzedAt: openedAt, ClosedAt: closedAt,
		Symbol: row[3], Side: row[4], TF: row[5],
		Entry: entry, Stop: stop, TP1: tp1, TP2: tp2,
		Anchor: row[10], OpenNotes: row[11],
		ExitPrice: exit, Outcome: row[13], RRealized: rR, CloseNotes: row[15],
	}
}

func parseRowV2(row []string) Trade {
	id, _ := strconv.Atoi(row[0])
	openedAt, _ := time.Parse(time.RFC3339, row[1])
	var analyzedAt, closedAt time.Time
	if row[2] != "" {
		analyzedAt, _ = time.Parse(time.RFC3339, row[2])
	}
	if analyzedAt.IsZero() {
		analyzedAt = openedAt
	}
	if row[3] != "" {
		closedAt, _ = time.Parse(time.RFC3339, row[3])
	}
	entry, _ := strconv.ParseFloat(row[7], 64)
	stop, _ := strconv.ParseFloat(row[8], 64)
	tp1, _ := strconv.ParseFloat(row[9], 64)
	tp2, _ := strconv.ParseFloat(row[10], 64)
	var exit, rR float64
	if row[13] != "" {
		exit, _ = strconv.ParseFloat(row[13], 64)
	}
	if row[15] != "" {
		rR, _ = strconv.ParseFloat(row[15], 64)
	}
	return Trade{
		ID: id, OpenedAt: openedAt, AnalyzedAt: analyzedAt, ClosedAt: closedAt,
		Symbol: row[4], Side: row[5], TF: row[6],
		Entry: entry, Stop: stop, TP1: tp1, TP2: tp2,
		Anchor: row[11], OpenNotes: row[12],
		ExitPrice: exit, Outcome: row[14], RRealized: rR, CloseNotes: row[16],
	}
}

func parseRowV3(row []string) Trade {
	id, _ := strconv.Atoi(row[0])
	openedAt, _ := time.Parse(time.RFC3339, row[1])
	var analyzedAt, closedAt time.Time
	if row[2] != "" {
		analyzedAt, _ = time.Parse(time.RFC3339, row[2])
	}
	if analyzedAt.IsZero() {
		analyzedAt = openedAt
	}
	if row[3] != "" {
		closedAt, _ = time.Parse(time.RFC3339, row[3])
	}
	entry, _ := strconv.ParseFloat(row[8], 64)
	stop, _ := strconv.ParseFloat(row[9], 64)
	tp1, _ := strconv.ParseFloat(row[10], 64)
	tp2, _ := strconv.ParseFloat(row[11], 64)
	var exit, rR float64
	if row[14] != "" {
		exit, _ = strconv.ParseFloat(row[14], 64)
	}
	if row[16] != "" {
		rR, _ = strconv.ParseFloat(row[16], 64)
	}
	return Trade{
		ID: id, OpenedAt: openedAt, AnalyzedAt: analyzedAt, ClosedAt: closedAt,
		Symbol: row[4], Side: row[5], TF: row[6], Score: row[7],
		Entry: entry, Stop: stop, TP1: tp1, TP2: tp2,
		Anchor: row[12], OpenNotes: row[13],
		ExitPrice: exit, Outcome: row[15], RRealized: rR, CloseNotes: row[17],
	}
}

// parseRowV4 is v3 + a trailing `leverage` column (index 18).
func parseRowV4(row []string) Trade {
	t := parseRowV3(row[:18]) // first 18 cols are identical to v3
	if row[18] != "" {
		t.Leverage, _ = strconv.Atoi(row[18])
	}
	return t
}

// parseRowV5 is v4 + a trailing `filled_at` column (index 19).
func parseRowV5(row []string) Trade {
	t := parseRowV4(row[:19])
	if row[19] != "" {
		if at, err := time.Parse(time.RFC3339, row[19]); err == nil {
			t.FilledAt = at
		}
	}
	return t
}

func rowFromTrade(t Trade) []string {
	closedAt := ""
	if !t.ClosedAt.IsZero() {
		closedAt = t.ClosedAt.Local().Format(time.RFC3339)
	}
	analyzedAt := ""
	if !t.AnalyzedAt.IsZero() {
		analyzedAt = t.AnalyzedAt.Local().Format(time.RFC3339)
	}
	exit, rR := "", ""
	if t.ExitPrice != 0 {
		exit = strconv.FormatFloat(t.ExitPrice, 'f', -1, 64)
	}
	if !t.ClosedAt.IsZero() {
		rR = strconv.FormatFloat(t.RRealized, 'f', 4, 64)
	}
	lev := ""
	if t.Leverage > 0 {
		lev = strconv.Itoa(t.Leverage)
	}
	filledAt := ""
	if !t.FilledAt.IsZero() {
		filledAt = t.FilledAt.Local().Format(time.RFC3339)
	}
	return []string{
		strconv.Itoa(t.ID),
		t.OpenedAt.Local().Format(time.RFC3339),
		analyzedAt,
		closedAt,
		t.Symbol,
		t.Side,
		t.TF,
		t.Score,
		strconv.FormatFloat(t.Entry, 'f', -1, 64),
		strconv.FormatFloat(t.Stop, 'f', -1, 64),
		strconv.FormatFloat(t.TP1, 'f', -1, 64),
		strconv.FormatFloat(t.TP2, 'f', -1, 64),
		t.Anchor,
		t.OpenNotes,
		exit,
		t.Outcome,
		rR,
		t.CloseNotes,
		lev,
		filledAt,
	}
}

// SortByOpenedDesc sorts the slice newest-first by OpenedAt.
func SortByOpenedDesc(trades []Trade) {
	sort.Slice(trades, func(i, j int) bool { return trades[i].OpenedAt.After(trades[j].OpenedAt) })
}
