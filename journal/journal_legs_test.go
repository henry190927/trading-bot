package journal

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func close9(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Each expected value is traced by hand from the legBlend contract.
func TestLegBlend(t *testing.T) {
	cases := []struct {
		name      string
		legs      []Exit
		avg, frac float64
	}{
		{"no legs", nil, 0, 0},
		{"one 50% leg at 82", []Exit{{Price: 82, Pct: 50}}, 82, 0.5},
		// (30*110 + 20*120) / 50 = 5700/50 = 114; frac (30+20)/100 = 0.5
		{"two legs", []Exit{{Price: 110, Pct: 30}, {Price: 120, Pct: 20}}, 114, 0.5},
		// the zero-price leg is dropped, not averaged in
		{"an empty leg is ignored", []Exit{{Price: 0, Pct: 50}, {Price: 110, Pct: 30}}, 110, 0.3},
		{"a zero-pct leg is ignored", []Exit{{Price: 999, Pct: 0}, {Price: 110, Pct: 30}}, 110, 0.3},
		// frac 120/100 capped to 1; avg uses the uncapped weights:
		// (70*110 + 50*120) / 120 = 13700/120 = 114.1666…
		{"legs over 100% cap at 1", []Exit{{Price: 110, Pct: 70}, {Price: 120, Pct: 50}}, 13700.0 / 120, 1},
	}
	for _, c := range cases {
		avg, frac := legBlend(c.legs)
		if !close9(avg, c.avg) || !close9(frac, c.frac) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, avg, frac, c.avg, c.frac)
		}
	}
}

// Long 100, stop 90 (1R = 10). Legs 30% @110 (+1R) and 20% @120 (+2R),
// remainder 50% @95 (-0.5R):
//
//	R = 0.3*1 + 0.2*2 + 0.5*(-0.5) = 0.45
//	blend = 0.5*114 + 0.5*95 = 104.5 → (104.5-100)/10 = 0.45, the same number
//	locked = 0.3*1 + 0.2*2 = 0.7
func TestBlendedExitAndLockedROverLegs(t *testing.T) {
	tr := Trade{Side: "long", Entry: 100, Stop: 90, Exits: []Exit{{Price: 110, Pct: 30}, {Price: 120, Pct: 20}}}
	if got := BlendedExit(tr, 95); !close9(got, 104.5) {
		t.Errorf("BlendedExit = %v, want 104.5", got)
	}
	if got := RealizedR(tr, BlendedExit(tr, 95)); !close9(got, 0.45) {
		t.Errorf("R = %v, want 0.45", got)
	}
	if got := LockedR(tr); !close9(got, 0.7) {
		t.Errorf("LockedR = %v, want 0.7", got)
	}
	if got := ClosedFrac(tr); !close9(got, 0.5) {
		t.Errorf("ClosedFrac = %v, want 0.5", got)
	}
}

// The legacy TP1 columns are a leg too, ahead of Exits:
// TP1 50% @82 + exit 20% @84, rest 30% @79.2 →
// 0.5*82 + 0.2*84 + 0.3*79.2 = 41 + 16.8 + 23.76 = 81.56
func TestLegacyTP1LegCountsWithExits(t *testing.T) {
	tr := Trade{Side: "long", Entry: 80.424, Stop: 79.2, TP1FillPrice: 82, TP1FillPct: 50,
		Exits: []Exit{{Price: 84, Pct: 20}}}
	if n := len(Legs(tr)); n != 2 {
		t.Fatalf("Legs = %d, want 2", n)
	}
	if got := BlendedExit(tr, 79.2); !close9(got, 81.56) {
		t.Errorf("BlendedExit = %v, want 81.56", got)
	}
}

// Exits survive a write and a read; a row without exits writes an empty cell.
func TestExitsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.csv")
	at := time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)
	in := []Trade{
		{ID: 1, OpenedAt: at, Symbol: "SUI", Side: "long", Entry: 1, Stop: 0.9,
			Exits: []Exit{{Price: 1.1, Qty: 300, Pct: 30, At: at, OrderID: "a", Src: "exchange"}}},
		{ID: 2, OpenedAt: at, Symbol: "BTC", Side: "short", Entry: 100, Stop: 110},
	}
	if err := WriteAll(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Exits) != 1 || out[0].Exits[0] != in[0].Exits[0] {
		t.Errorf("exits = %+v, want %+v", out[0].Exits, in[0].Exits)
	}
	if len(out[1].Exits) != 0 {
		t.Errorf("row 2 exits = %+v, want none", out[1].Exits)
	}
	if got := rowFromTrade(in[1]); got[len(got)-1] != "" {
		t.Errorf("empty exits cell = %q, want \"\"", got[len(got)-1])
	}
}

func TestExitsCellThatDoesNotParseIsAnError(t *testing.T) {
	row := rowFromTrade(Trade{ID: 1, OpenedAt: time.Now(), Symbol: "SUI", Side: "long"})
	row[33] = "[{not json"
	if _, err := parseRow(row); err == nil {
		t.Fatal("a corrupt exits cell must not read as no exits")
	}
}
