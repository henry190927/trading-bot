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

// #66 as it was actually recorded, plus the equity that was NOT recorded at
// the time and is the whole point of v9.
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

	// The pair that took the account to zero on 2026-09-08, as a single
	// synthetic position: 16,463u notional against 139.66u of equity.
	fatal := Trade{MarginUSDT: 139.66, Leverage: 1, EquityUSDT: 139.66}
	fatal.MarginUSDT, fatal.Leverage = 16463, 1
	if got := fatal.AccountLeverage(); !close2(got, 117.87913504224545) {
		t.Errorf("fatal AccountLeverage() = %v, want 117.87913504224545", got)
	}
	if got := fatal.KillDistancePct(); !close2(got, 0.8483265504464557) {
		t.Errorf("fatal KillDistancePct() = %v, want 0.8483265504464557 (the 0.848%% from the post-mortem)", got)
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
	// #61 as recorded, trimmed to the v8 column count.
	v8Row := "61,2026-09-03T15:50:31+08:00,2026-09-03T15:50:31+08:00,2026-09-03T21:38:53+08:00," +
		"BTC,long,1h,zone,77640,77380,78900,79222.3,weekopen,notes,78552.6,manual,3.5100,closenotes," +
		"125,2026-09-03T15:50:31+08:00,,,,75,,,,,"
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
	if got[0].ID != 61 || got[0].Symbol != "BTC" || got[0].MarginUSDT != 75 || got[0].Leverage != 125 {
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
	if again[0].ID != 61 || again[0].MarginUSDT != 75 {
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
