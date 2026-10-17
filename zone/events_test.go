package zone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventsAppendAndRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone-events.jsonl")
	at := func(h int) time.Time { return time.Date(2026, 10, 17, h, 0, 0, 0, time.UTC) }

	e1 := Event{At: at(10), Kind: EventFired, Symbol: "XAU", TF: "1h", Text: "fired XAU"}
	e2 := Event{At: at(9), Kind: EventSkip, Symbol: "ETH", TF: "4h", Text: "SKIP ETH"} // older, written later (a backfill)
	e3 := Event{At: at(11), Kind: EventPushFailed, Symbol: "BTC", Text: "push failed BTC"}

	for _, e := range []Event{e1, e2} {
		if err := AppendEvent(path, e); err != nil {
			t.Fatal(err)
		}
	}
	// A torn line and a blank line between real ones.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{not json\n\n")
	f.Close()
	if err := AppendEvent(path, e3); err != nil {
		t.Fatal(err)
	}

	got, skipped, err := RecentEvents(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the torn line; blank lines are not errors)", skipped)
	}
	if len(got) != 2 || got[0].Text != "fired XAU" || got[1].Text != "push failed BTC" {
		t.Fatalf("last 2 = %+v, want [fired XAU, push failed BTC]", got)
	}

	all, _, err := RecentEvents(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"SKIP ETH", "fired XAU", "push failed BTC"} // by time, not by line order
	if len(all) != len(want) {
		t.Fatalf("all = %d events, want %d", len(all), len(want))
	}
	for i, w := range want {
		if all[i].Text != w {
			t.Errorf("all[%d] = %q, want %q", i, all[i].Text, w)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
}

func TestRecentEventsMissing(t *testing.T) {
	if _, _, err := RecentEvents(filepath.Join(t.TempDir(), "absent.jsonl"), 40); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}
