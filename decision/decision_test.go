package decision

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/autotrade"
)

func fire(at time.Time, tf, side string) autotrade.PaperFire {
	return autotrade.PaperFire{Time: at, Symbol: "BTC", TF: tf, Strategy: "engine", Side: side, Entry: 100, Stop: 98, TP: 104}
}

func TestDeadlineClamp(t *testing.T) {
	at := Since.Add(time.Hour)
	for tf, want := range map[string]time.Duration{"1m": 30 * time.Minute, "1h": time.Hour, "2h": 2 * time.Hour, "4h": 4 * time.Hour, "1d": 4 * time.Hour} {
		if got := Deadline(fire(at, tf, "long")).Sub(at); got != want {
			t.Errorf("%s: deadline +%v, want +%v", tf, got, want)
		}
	}
}

func TestRMoved(t *testing.T) {
	l := fire(Since, "1h", "long") // entry 100 stop 98: 1R = 2
	if r := RMoved(l, 103); r != 1.5 {
		t.Errorf("long +3 = %v R, want 1.5", r)
	}
	s := autotrade.PaperFire{Side: "short", Entry: 100, Stop: 102}
	if r := RMoved(s, 101); r != -0.5 {
		t.Errorf("short +1 against = %v R, want -0.5", r)
	}
	if r := RMoved(l, 0); r != 0 {
		t.Errorf("unknown mark = %v, want 0", r)
	}
}

func TestPendingAndGroup(t *testing.T) {
	now := Since.Add(10 * time.Hour)
	old := fire(Since.Add(-time.Hour), "1h", "long")          // before Since: never counted
	expired := fire(now.Add(-2*time.Hour), "1h", "long")      // deadline passed, unanswered
	open := fire(now.Add(-20*time.Minute), "1h", "short")     // still decidable
	taken := fire(now.Add(-10*time.Minute), "1h", "long")     // decided
	fires := []autotrade.PaperFire{taken, open, expired, old} // newest-first, as the log reads
	decided := map[string]Record{ID(taken): {ID: ID(taken), Decision: Take}}

	p := Pending(fires, decided, now)
	if len(p) != 1 || ID(p[0]) != ID(open) {
		t.Fatalf("pending = %v, want only the open one", p)
	}
	cases := []struct {
		f     autotrade.PaperFire
		group string
		ok    bool
	}{{old, "", false}, {expired, "none", true}, {open, "", false}, {taken, Take, true}}
	for _, c := range cases {
		if g, ok := Group(c.f, decided, now); g != c.group || ok != c.ok {
			t.Errorf("%s: group %q/%v, want %q/%v", ID(c.f), g, ok, c.group, c.ok)
		}
	}
}

func TestLatestLastWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.jsonl")
	f := fire(Since, "1h", "long")
	for _, d := range []string{Take, Skip} {
		if err := Append(p, Record{ID: ID(f), Decision: d, Fire: f}); err != nil {
			t.Fatal(err)
		}
	}
	m, bad, err := Latest(p)
	if err != nil || bad != 0 || m[ID(f)].Decision != Skip {
		t.Fatalf("latest = %+v bad %d err %v; want skip (the correction)", m, bad, err)
	}
	if m, _, err := Latest(filepath.Join(t.TempDir(), "missing")); err != nil || len(m) != 0 {
		t.Fatalf("missing file: %v %v", m, err)
	}
}
