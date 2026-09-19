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
//	v5 = 20 cols (adds filled_at at end)
//	v6 = 21 cols (adds signal_ctx — analyst snapshot at signal time)
//	v7 = 23 cols (adds tp1_auto + tp1_order_id — auto-TP1 placement state)
//	v8 = 29 cols (adds margin_usdt + entry/stop/tp2 auto-placement state)
//	v9 = 30 cols (adds equity_usdt — account equity at entry) — current
//
// equity_usdt is the one column that makes the row say anything about RISK
// rather than about the read. R is leverage-independent by definition, so the
// R series is structurally incapable of showing an account going to zero —
// this file has read 66 settled trades at a healthy gross R while the balance
// read 0.00000000, and both numbers were correct. Bucketing the filled rows by
// leverage confirms it is definitional and not a data gap — 100x+ averaged
// +0.435R and leverage-not-recorded averaged +0.450R, identical, because the
// position size cancels out of R.
//
// margin_usdt x leverage already gave NOTIONAL on 48% of filled rows, but
// notional alone answers nothing: 16,500u is a rounding error on a large
// account and a liquidation on a small one. Only notional/equity — account
// leverage — is comparable across time, and the fatal pair sat at 117.9x with
// a 0.848% kill distance that nothing in the system computed.
var Header = []string{
	"id", "opened_at", "analyzed_at", "closed_at", "symbol", "side", "tf", "score",
	"entry", "stop", "tp1", "tp2",
	"anchor", "open_notes",
	"exit_price", "outcome", "r_realized", "close_notes",
	"leverage",
	"filled_at",
	"signal_ctx",
	"tp1_auto",
	"tp1_order_id",
	"margin_usdt",
	"entry_order_id",
	"stop_auto",
	"stop_order_id",
	"tp2_auto",
	"tp2_order_id",
	"equity_usdt",
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
	Outcome    string // "tp1", "tp2", "stop", "manual", "timeout", "no-fill", "liquidated", "" if open
	RRealized  float64
	CloseNotes string
	// FilledAt is the time the entry price was first reached after the
	// trade was recorded. Zero = still pending (entry not yet triggered).
	// Set by the dashboard's fill-detection sweep on each refresh.
	FilledAt time.Time
	// SignalCtx is a compact key=value snapshot of the validator + engine
	// context at signal time, captured when the user clicks +record from
	// the dashboard or /validate. Format: semicolon-delimited pairs, e.g.
	//   "v=5.5;ver=TAKE;d=up;st=1;va=at_VAL;f=-0.0004"
	// Keys (all optional):
	//   v       validator.Result.Total as float
	//   ver     short verdict: STRONG / TAKE / NEUTRAL / WEAK / AVOID
	//   d       POC drift direction: up / down / flat
	//   st      1 if drift is stacked (POC50 > POC100 > POC200 monotonic)
	//   va      VA position: at_VAH / at_VAL / in / above / below
	//   f       funding rate as raw fraction (per 8h)
	//   knife   1 if RecentFlashBarBearish
	//   squeeze 1 if RecentFlashBarBullish
	// Purpose: lets us bucket realized trades by validator band post-hoc,
	// the same way the backtest's --replay-validator does for historical
	// signals. Free to leave blank for manually-keyed trades.
	SignalCtx string
	// Leverage is the position multiplier used on the perp exchange. Optional
	// (0 = not recorded). Used to translate R-multiples into actual % return:
	// a 1R win on 100x leverage gross-returns about 100× the price-move %.
	// Doesn't affect R computation — R is leverage-independent by definition.
	Leverage int
	// TP1Auto is the persistent intent flag for auto-placing a reduce-only
	// TP1 LIMIT order on BingX. Set when the user checks "Place TP1" on the
	// open/edit form. Survives across refreshes so the fill-detection sweep
	// can retry placement once the live position appears.
	TP1Auto bool
	// TP1OrderID is the BingX orderId of the placed TP1 reduce-only LIMIT,
	// or "" if not yet placed. Sweep skips re-placement once non-empty.
	TP1OrderID string
	// MarginUSDT is the user-specified collateral for the trade. Used to
	// compute qty = floor((margin × leverage) / entry) at placement time.
	// Optional (0 = not recorded; only required when auto-opening on BingX).
	MarginUSDT float64
	// EntryOrderID is the BingX orderId of the placed LIMIT entry order
	// when /journal/open did auto-open. Empty means the user opened
	// manually on BingX.
	EntryOrderID string
	// StopAuto is the persistent intent to auto-place a reduce-only
	// STOP_MARKET when the position is detected. Same retry semantics as
	// [[TP1Auto]] — sweep keeps trying until StopOrderID is non-empty.
	StopAuto bool
	// StopOrderID is the BingX orderId of the placed stop, or "" if not
	// yet placed.
	StopOrderID string
	// TP2Auto is the persistent intent to auto-place a reduce-only LIMIT
	// at TP2 for the remaining (post-TP1) position size.
	TP2Auto bool
	// TP2OrderID is the BingX orderId of the placed TP2 limit, or "" if
	// not yet placed.
	TP2OrderID string
	// EquityUSDT is total account equity AT ENTRY, read from BingX when the
	// trade was opened. 0 = not recorded (every row before 2026-09-08, and
	// any row whose balance read failed).
	//
	// It is here so the row can answer "what fraction of the account was at
	// risk", which neither R nor notional can. See the Header comment.
	EquityUSDT float64
}

// Notional is the position's face value in USDT — margin x leverage. Zero when
// either input is unrecorded, which is 52% of filled rows historically.
func (t Trade) Notional() float64 {
	if t.MarginUSDT <= 0 || t.Leverage <= 0 {
		return 0
	}
	return t.MarginUSDT * float64(t.Leverage)
}

// AccountLeverage is notional / equity-at-entry: how many times the whole
// account this single position represented. Zero when either is unrecorded.
//
// This is the number that separates a survivable trade from a fatal one, and
// it is invisible to R. For scale, from this journal's own history: the pair
// in the fatal configuration sits at 117.9x combined, while the trades that
// produced the best results ran at 30-40x.
func (t Trade) AccountLeverage() float64 {
	n := t.Notional()
	if n <= 0 || t.EquityUSDT <= 0 {
		return 0
	}
	return n / t.EquityUSDT
}

// KillDistancePct is the adverse move, in percent, that would take equity to
// zero if this position were the account's only exposure. The reciprocal of
// AccountLeverage. Zero when not computable.
//
// Reported as a percentage because that is the unit a chart is read in: 0.85%
// is inside a single 1h bar on BTC 19% of the time and on ETH 34% of the time,
// which is what made 117.9x fatal rather than merely aggressive.
func (t Trade) KillDistancePct() float64 {
	al := t.AccountLeverage()
	if al <= 0 {
		return 0
	}
	return 100 / al
}

// IsOpen reports whether the trade is still open (no close time set).
func (t Trade) IsOpen() bool { return t.ClosedAt.IsZero() }

// IsNoFill reports whether the plan never filled — recorded for
// discipline tracking (signal-to-fill ratio) without affecting WR/R
// stats. R should be exactly 0 for no-fills.
func (t Trade) IsNoFill() bool { return t.Outcome == "no-fill" }

// IsLiquidated reports a position the exchange closed because margin ran out.
//
// Distinct from "stop" and from "manual", and the distinction is the point.
// It is not "stop": a liquidation happens when there is no protective order to
// hit, so filing it there corrupts the one statistic that answers "are my
// stops any good". It is not "manual" either: that bucket is the discretionary
// exit, which is where this account's edge actually lives (+0.744R mean over
// 31 trades as of 2026-09-08), and a forced close is the opposite of a
// decision. Two of the three 2026-09 blowups ended this way and both would
// otherwise have been filed as discretionary.
//
// R is NOT special-cased. With a planned stop, RealizedR gives a figure worse
// than -1R, which is exactly right — the loss exceeded the risk that was
// budgeted. With no stop, HasR already keeps it out of every R statistic.
func (t Trade) IsLiquidated() bool { return t.Outcome == "liquidated" }

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
	defer func() { _ = f.Close() }()

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
//
// Atomic: written to a sibling .tmp and renamed into place, the same shape
// oi.Save and writeSetups already use. This one matters most. WriteAll is a
// WHOLE-FILE rewrite, so the previous version of the code — os.Create on the
// live path — truncated journal.csv before the first row was written. A crash,
// a full disk or an OOM kill anywhere in the loop left a short file with no
// intermediate state to recover from, and journal.csv is the only record of
// what the account actually did.
//
// The rename is atomic only within one filesystem, which is why the temp file
// is a sibling rather than something under os.TempDir.
func WriteAll(path string, trades []Trade) error {
	if path == "" {
		path = DefaultPath()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	// The renamed file BECOMES journal.csv, so the temp file's mode is the mode
	// the journal ends up with — os.Create's 0666&^umask would silently
	// re-permission it on the next write. Carry the existing mode across
	// instead of imposing one: the live file is 0664, and quietly dropping a
	// group's write bit is how a second writer loses access without anything
	// reporting an error.
	mode := os.FileMode(0o644) // a journal that does not exist yet
	if fi, serr := os.Stat(path); serr == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	// O_CREATE's mode is an upper bound, not the result — it is masked by the
	// process umask, so a 022 umask turns a carried-over 0664 into 0644 and
	// drops the bit this is trying to preserve. Chmod is not masked.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Any failure past this point leaves a stale .tmp behind, which would then
	// be O_TRUNC'd by the next attempt — but it is removed eagerly so a
	// half-written journal is never sitting next to the real one.
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}

	w := csv.NewWriter(f)
	if err := w.Write(Header); err != nil {
		return fail(err)
	}
	for _, t := range trades {
		if err := w.Write(rowFromTrade(t)); err != nil {
			return fail(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fail(err)
	}
	// Flush only moved csv's buffer into the *os.File. Sync is what puts the
	// bytes on the device — without it the rename can land while the data
	// behind it has not, which is the one failure this whole function exists
	// to prevent. A few times a day on a 50KB file; the cost is irrelevant.
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	// Close, not deferred: a write error on the file itself (ENOSPC, EIO)
	// surfaces here, and swallowing it would rename a short file into place.
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
// HasR reports whether this trade's R is DEFINED.
//
// R is measured against the PLANNED RISK, so a position taken without a stop
// has no R at all. That is not the same as an R of zero: zero reads as a
// break-even trade, counts in the denominator of every average, and drags the
// whole series toward the middle. A trade with no stop still has a P&L, a
// symbol and a time — it just cannot answer "how many times my planned risk
// did this make", because there was no planned risk.
//
// Entry == Stop is the same condition arriving by a different route: the
// divisor is zero either way.
func (t Trade) HasR() bool { return t.Entry > 0 && t.Stop > 0 && t.Entry != t.Stop }

// RealizedR returns 0 when there is no risk distance. Callers computing
// STATISTICS must gate on HasR first — this zero is "undefined", and summing
// it into an average silently states that the trade broke even.
func RealizedR(t Trade, exit float64) float64 {
	// HasR, not risk != 0. A trade with no stop recorded has Stop == 0, and
	// |entry - 0| is the ENTRY PRICE — a perfectly finite divisor that yields
	// a small, plausible-looking R. SUI at entry 0.691 exiting 0.6879 came out
	// as -0.0045R, which reads as "almost break-even" rather than "undefined",
	// and there is nothing in that number to make a reader suspicious.
	//
	// The old guard only fired when entry == stop exactly.
	if !t.HasR() {
		return 0
	}
	risk := math.Abs(t.Entry - t.Stop)
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
		case "tp1", "tp2", "stop", "manual", "timeout", "no-fill", "liquidated":
		default:
			return fmt.Errorf("outcome must be tp1|tp2|stop|manual|timeout|no-fill|liquidated, got %q", value)
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
	case 21:
		return parseRowV6(row), nil
	case 23:
		return parseRowV7(row), nil
	case 29:
		return parseRowV8(row), nil
	case 30:
		return parseRowV9(row), nil
	}
	return Trade{}, fmt.Errorf("expected 16/17/18/19/20/21/23/29/30 columns, got %d", len(row))
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

// parseRowV6 is v5 + a trailing `signal_ctx` column (index 20).
func parseRowV6(row []string) Trade {
	t := parseRowV5(row[:20])
	t.SignalCtx = row[20]
	return t
}

// parseRowV7 is v6 + `tp1_auto` (index 21) + `tp1_order_id` (index 22).
func parseRowV7(row []string) Trade {
	t := parseRowV6(row[:21])
	t.TP1Auto = row[21] == "true"
	t.TP1OrderID = row[22]
	return t
}

// parseRowV8 is v7 + margin_usdt (23) + entry_order_id (24) +
// stop_auto (25) + stop_order_id (26) + tp2_auto (27) + tp2_order_id (28).
func parseRowV8(row []string) Trade {
	t := parseRowV7(row[:23])
	if row[23] != "" {
		t.MarginUSDT, _ = strconv.ParseFloat(row[23], 64)
	}
	t.EntryOrderID = row[24]
	t.StopAuto = row[25] == "true"
	t.StopOrderID = row[26]
	t.TP2Auto = row[27] == "true"
	t.TP2OrderID = row[28]
	return t
}

func parseRowV9(row []string) Trade {
	t := parseRowV8(row[:29])
	if row[29] != "" {
		t.EquityUSDT, _ = strconv.ParseFloat(row[29], 64)
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
	// Closed AND measurable. A trade with no stop has no R, and writing
	// "0.0000" into this column says it broke even — to a human reading the
	// CSV, to a spreadsheet, and to any future reader that does not know to
	// check HasR. Blank is the honest cell, and it round-trips: parseRow
	// leaves RRealized at 0 for an empty field, which HasR then reports as
	// undefined again.
	if !t.ClosedAt.IsZero() && t.HasR() {
		rR = strconv.FormatFloat(t.RRealized, 'f', 4, 64)
	}
	marginStr := ""
	if t.MarginUSDT > 0 {
		marginStr = strconv.FormatFloat(t.MarginUSDT, 'f', -1, 64)
	}
	lev := ""
	if t.Leverage > 0 {
		lev = strconv.Itoa(t.Leverage)
	}
	filledAt := ""
	if !t.FilledAt.IsZero() {
		filledAt = t.FilledAt.Local().Format(time.RFC3339)
	}
	// Blank rather than "0" when unknown, so a row that predates the column
	// (or one opened while the balance read failed) is distinguishable from a
	// genuinely zero account. AccountLeverage returns 0 for both, but the CSV
	// keeps the difference for anyone reading it later.
	equityStr := ""
	if t.EquityUSDT > 0 {
		equityStr = strconv.FormatFloat(t.EquityUSDT, 'f', -1, 64)
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
		t.SignalCtx,
		boolStr(t.TP1Auto),
		t.TP1OrderID,
		marginStr,
		t.EntryOrderID,
		boolStr(t.StopAuto),
		t.StopOrderID,
		boolStr(t.TP2Auto),
		t.TP2OrderID,
		equityStr,
	}
}

// boolStr renders a bool as "true" / "" so the CSV stays compact when
// the flag is unset (most legacy rows). Empty string also parses back as
// false in parseRowV7, keeping the round-trip lossless.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// SortByOpenedDesc sorts the slice newest-first by OpenedAt.
func SortByOpenedDesc(trades []Trade) {
	sort.Slice(trades, func(i, j int) bool { return trades[i].OpenedAt.After(trades[j].OpenedAt) })
}
