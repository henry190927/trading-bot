package journal

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const eps = 1e-9

func close2(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// A trade as it was actually recorded, plus the equity that was NOT recorded
// at the time and is the whole point of v9.
func trade66() Trade {
	return Trade{
		ID: 66, Symbol: "BTC", Side: "long", TF: "1h",
		OpenedAt:   time.Date(2026, 9, 7, 23, 40, 58, 0, time.UTC),
		Entry:      79006,
		Stop:       78760,
		TP1:        79407.2,
		TP2:        79564.1,
		Leverage:   125,
		MarginUSDT: 83,
		EquityUSDT: 257.79,
	}
}

func TestRiskDerivations(t *testing.T) {
	tr := trade66()
	if got := tr.Notional(); !close2(got, 10375) {
		t.Errorf("Notional() = %v, want 10375 (83u x 125x)", got)
	}
	// 10375 / 257.79
	if got := tr.AccountLeverage(); !close2(got, 40.245936615074285) {
		t.Errorf("AccountLeverage() = %v, want 40.245936615074285", got)
	}
	// 100 / 40.2459...
	if got := tr.KillDistancePct(); !close2(got, 2.484722891566265) {
		t.Errorf("KillDistancePct() = %v, want 2.484722891566265", got)
	}

	// The fatal configuration as a single synthetic position:
	// 16,500u of notional against 140u of equity. 16500/140 = 117.857...x, and
	// its reciprocal as a percentage is the kill distance.
	fatal := Trade{MarginUSDT: 16500, Leverage: 1, EquityUSDT: 140}
	if got := fatal.AccountLeverage(); !close2(got, 117.85714285714286) {
		t.Errorf("fatal AccountLeverage() = %v, want 117.85714285714286", got)
	}
	if got := fatal.KillDistancePct(); !close2(got, 0.8484848484848485) {
		t.Errorf("fatal KillDistancePct() = %v, want 0.8484848484848485", got)
	}
}

// Every unrecorded input must yield 0, never a NaN, an Inf, or a plausible
// wrong number. Half the historical rows are missing at least one of these,
// so a divide-by-zero here would render as a risk figure on the journal page.
func TestRiskDerivationsDegradeToZero(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Trade)
	}{
		{"no equity (every row before v9)", func(tr *Trade) { tr.EquityUSDT = 0 }},
		{"no margin", func(tr *Trade) { tr.MarginUSDT = 0 }},
		{"no leverage", func(tr *Trade) { tr.Leverage = 0 }},
		{"negative equity", func(tr *Trade) { tr.EquityUSDT = -5 }},
		{"nothing at all", func(tr *Trade) { tr.MarginUSDT, tr.Leverage, tr.EquityUSDT = 0, 0, 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := trade66()
			c.mut(&tr)
			for name, got := range map[string]float64{
				"AccountLeverage": tr.AccountLeverage(),
				"KillDistancePct": tr.KillDistancePct(),
			} {
				if got != 0 {
					t.Errorf("%s() = %v, want exactly 0", name, got)
				}
				if math.IsNaN(got) || math.IsInf(got, 0) {
					t.Errorf("%s() = %v — NaN/Inf would render as a risk figure", name, got)
				}
			}
		})
	}
}

func TestHeaderIsV9(t *testing.T) {
	if len(Header) != 30 {
		t.Fatalf("len(Header) = %d, want 30 (v9)", len(Header))
	}
	if Header[29] != "equity_usdt" {
		t.Errorf("Header[29] = %q, want \"equity_usdt\"", Header[29])
	}
}

func TestRoundTripPreservesEquity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "journal.csv")
	in := []Trade{trade66()}
	if err := WriteAll(p, in); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	out, err := ReadAll(p)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("read %d trades, want 1", len(out))
	}
	if math.Abs(out[0].EquityUSDT-257.79) > eps {
		t.Errorf("EquityUSDT round-tripped as %v, want 257.79", out[0].EquityUSDT)
	}
	if !close2(out[0].AccountLeverage(), 40.245936615074285) {
		t.Errorf("AccountLeverage after round-trip = %v", out[0].AccountLeverage())
	}
}

// A blank equity must come back as 0 rather than as a parse artifact, and must
// be written blank rather than as "0" — a row that predates the column has to
// stay distinguishable from a genuinely zero account.
func TestUnrecordedEquityStaysBlank(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "journal.csv")
	tr := trade66()
	tr.EquityUSDT = 0
	if err := WriteAll(p, []Trade{tr}); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !strings.HasSuffix(lines[1], ",") {
		t.Errorf("row does not end in an empty equity field: %q", lines[1][max(0, len(lines[1])-40):])
	}
	out, err := ReadAll(p)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if out[0].EquityUSDT != 0 {
		t.Errorf("EquityUSDT = %v, want 0", out[0].EquityUSDT)
	}
}

// The migration that matters: the production file is v8 (29 columns) and the
// new binary must read it without complaint, then write it back as v9. If this
// breaks, every process that reads journal.csv fails at once.
func TestReadsV8AndUpgradesToV9(t *testing.T) {
	v8Header := strings.Join(Header[:29], ",")
	// A v8 row in the shape v8 actually wrote, trimmed to its column count.
	v8Row := "7,2026-01-02T15:00:00+08:00,2026-01-02T15:00:00+08:00,2026-01-02T21:00:00+08:00," +
		"BTC,long,1h,zone,77600,77400,78900,79200,weekopen,notes,78500,manual,3.5000,closenotes," +
		"125,2026-01-02T15:00:00+08:00,,,,75,,,,,"
	if n := strings.Count(v8Row, ",") + 1; n != 29 {
		t.Fatalf("fixture has %d columns, want 29 — the test's own premise is wrong", n)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "journal.csv")
	if err := os.WriteFile(p, []byte(v8Header+"\n"+v8Row+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAll(p)
	if err != nil {
		t.Fatalf("ReadAll on a v8 file: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d trades, want 1", len(got))
	}
	if got[0].ID != 7 || got[0].Symbol != "BTC" || got[0].MarginUSDT != 75 || got[0].Leverage != 125 {
		t.Errorf("v8 row mis-parsed: %+v", got[0])
	}
	if got[0].EquityUSDT != 0 {
		t.Errorf("EquityUSDT = %v on a v8 row, want 0", got[0].EquityUSDT)
	}
	if got[0].AccountLeverage() != 0 {
		t.Errorf("AccountLeverage = %v with no equity, want 0", got[0].AccountLeverage())
	}

	// Write it back: now v9, and still readable.
	if err := WriteAll(p, got); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	raw, _ := os.ReadFile(p)
	if n := strings.Count(strings.Split(string(raw), "\n")[0], ",") + 1; n != 30 {
		t.Errorf("rewritten header has %d columns, want 30", n)
	}
	again, err := ReadAll(p)
	if err != nil {
		t.Fatalf("ReadAll after upgrade: %v", err)
	}
	if again[0].ID != 7 || again[0].MarginUSDT != 75 {
		t.Errorf("data lost across the v8 -> v9 upgrade: %+v", again[0])
	}
}

// Column counts between the known versions must still be rejected, so a
// hand-edited row that dropped a comma fails loudly instead of shifting every
// field by one.
func TestParseRowRejectsUnknownWidths(t *testing.T) {
	for _, n := range []int{15, 22, 24, 28, 31} {
		row := make([]string, n)
		for i := range row {
			row[i] = "0"
		}
		if _, err := parseRow(row); err == nil {
			t.Errorf("%d columns was accepted; want an error", n)
		}
	}
}

// WriteAll is a WHOLE-FILE rewrite, and until 2026-09-15 it opened
// journal.csv itself with os.Create — which truncates before the first row is
// written. A crash, a full disk or an OOM kill anywhere in the loop left a
// short file with nothing to recover from. It now writes a sibling .tmp and
// renames.
//
// The failure is injected by making the temp PATH a directory, so the open
// fails with EISDIR before a single byte moves. Under the old code the journal
// was ALREADY truncated by that point; under this one it must be untouched.
func TestWriteAllLeavesTheJournalIntactWhenTheWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.csv")
	orig := []Trade{trade66()}
	if err := WriteAll(path, orig); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatalf("blocking the temp path: %v", err)
	}
	doomed := trade66()
	doomed.ID, doomed.Entry = 99, 1234.5
	if err := WriteAll(path, []Trade{doomed}); err == nil {
		t.Fatal("WriteAll reported success with its temp path blocked")
	}

	got, err := ReadAll(path)
	if err != nil {
		t.Fatalf("journal unreadable after the failed write: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("journal holds %d trades, want the original 1", len(got))
	}
	if got[0].ID != 66 || !close2(got[0].Entry, 79006) {
		t.Errorf("journal = #%d entry %.1f, want the untouched entry 79006 "+
			"— the failed write reached the live file", got[0].ID, got[0].Entry)
	}
}

// A successful write must not leave its scratch file next to the real one —
// a stale journal.csv.tmp beside journal.csv reads as a half-finished write —
// and it must not re-permission the journal. os.Create applies 0666&^umask
// every time, so the mode was whatever the writing process's umask said; the
// live file is 0664 and a silent drop to 0644 is how a second writer loses
// access with nothing reporting an error.
func TestWriteAllKeepsTheModeAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.csv")

	// A journal that does not exist yet gets 0644.
	if err := WriteAll(path, []Trade{trade66()}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("journal.csv.tmp still present after a successful write (stat err = %v)", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("new journal mode = %04o, want 0644", got)
	}

	// An existing journal keeps whatever it already had — 0664 is the mode on
	// the live VPS file.
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(path, []Trade{trade66()}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o664 {
		t.Errorf("rewritten journal mode = %04o, want the 0664 it already had", got)
	}
}

// A trade taken WITHOUT a stop has no R — not an R of zero.
//
// RealizedR divides by the risk distance and returns 0 when there is none,
// which is the right thing for a formula and the wrong thing for a statistic:
// zero reads as break-even, counts in the denominator of every average, and
// drags the series toward the middle. HasR is the gate every statistic checks
// before using that number.
//
// This matters because a stopless position is the state most worth recording.
// The journal refused to store one until 2026-09-16, so three real trades —
// two of them the day's only winners — stayed off the book entirely.
func TestHasRSeparatesUndefinedFromZero(t *testing.T) {
	for _, c := range []struct {
		name  string
		trade Trade
		want  bool
	}{
		{"normal long", Trade{Entry: 2400, Stop: 2384, Side: "long"}, true},
		{"normal short", Trade{Entry: 2400, Stop: 2416, Side: "short"}, true},
		{"no stop recorded", Trade{Entry: 2400, Stop: 0, Side: "long"}, false},
		{"no entry", Trade{Entry: 0, Stop: 2384, Side: "long"}, false},
		// Same divisor-is-zero condition arriving by a different route.
		{"stop equals entry", Trade{Entry: 2400, Stop: 2400, Side: "long"}, false},
		{"neither", Trade{Side: "long"}, false},
	} {
		if got := c.trade.HasR(); got != c.want {
			t.Errorf("%s: HasR() = %v, want %v", c.name, got, c.want)
		}
	}

	// The trap in one assertion: a stopless trade that LOST money still
	// reports RealizedR 0, indistinguishable from a break-even trade with a
	// stop. Only HasR tells them apart.
	stopless := Trade{Entry: 0.691, Stop: 0, Side: "long"}
	breakeven := Trade{Entry: 2400, Stop: 2384, Side: "long"}
	// Before HasR gated it, this returned -0.0045: |0.691 - 0| is the ENTRY
	// PRICE, a finite divisor that produces a small plausible number rather
	// than an obvious zero. Nothing in -0.0045R makes a reader suspicious.
	if r := RealizedR(stopless, 0.6879); r != 0 {
		t.Errorf("RealizedR with no stop = %v, want 0 — |entry-0| is not zero risk", r)
	}
	if r := RealizedR(breakeven, 2400); r != 0 {
		t.Errorf("RealizedR at entry = %v, want 0", r)
	}
	if stopless.HasR() == breakeven.HasR() {
		t.Error("a stopless loser and a genuine break-even are indistinguishable — " +
			"that is exactly what HasR exists to prevent")
	}
}

// The r_realized COLUMN must be blank for a trade with no stop, not "0.0000".
// HasR protects the statistics; this protects the reader. A spreadsheet, or
// anyone opening journal.csv, has no way to know that a zero there means
// "undefined" rather than "broke even".
func TestStoplessTradeWritesBlankR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.csv")
	now := time.Now().UTC()
	stopless := Trade{
		ID: 1, Symbol: "SUI", Side: "long", Entry: 0.691, Stop: 0,
		ExitPrice: 0.6879, Outcome: "manual", OpenedAt: now, ClosedAt: now,
	}
	withStop := Trade{
		ID: 2, Symbol: "ETH", Side: "long", Entry: 2400, Stop: 2384,
		ExitPrice: 2440, Outcome: "tp1", OpenedAt: now, ClosedAt: now,
	}
	withStop.RRealized = RealizedR(withStop, withStop.ExitPrice)
	stopless.RRealized = RealizedR(stopless, stopless.ExitPrice)

	if err := WriteAll(path, []Trade{stopless, withStop}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header + 2", len(lines))
	}
	col := 16 // r_realized, per Header
	if Header[col] != "r_realized" {
		t.Fatalf("Header[%d] = %q, want r_realized — the index moved", col, Header[col])
	}
	if got := strings.Split(lines[1], ",")[col]; got != "" {
		t.Errorf("stopless r_realized = %q, want empty", got)
	}
	// 2440-2400 = 40 over a risk of 16 → +2.5000
	if got := strings.Split(lines[2], ",")[col]; got != "2.5000" {
		t.Errorf("with-stop r_realized = %q, want 2.5000", got)
	}

	// Round-trip: the blank comes back as undefined, not as a zero that now
	// looks measurable.
	back, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if back[0].HasR() {
		t.Error("the stopless row reads as having an R after a round-trip")
	}
	if !back[1].HasR() || back[1].RRealized != 2.5 {
		t.Errorf("with-stop row round-tripped to HasR=%v R=%v", back[1].HasR(), back[1].RRealized)
	}
}
