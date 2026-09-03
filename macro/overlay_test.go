package macro

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetOverlay clears package state between cases. The overlay is package-level
// (All() has ten callers and none of them wants to thread a handle), so tests
// have to be explicit about isolation.
func resetOverlay(t *testing.T) {
	t.Helper()
	ovMu.Lock()
	ovEvents, ovSuppress, ovStatus = nil, nil, Status{}
	ovModTime, ovLoaded = time.Time{}, false
	ovMu.Unlock()
	t.Cleanup(func() {
		ovMu.Lock()
		ovEvents, ovSuppress, ovStatus = nil, nil, Status{}
		ovModTime, ovLoaded = time.Time{}, false
		ovMu.Unlock()
	})
}

func writeOverlay(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "macro-overlay.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The motivating case: blackout a Fed speech announced days ahead, without a
// rebuild. Waller spoke 2026-09-03 12:30Z.
func TestOverlayAddsAnEvent(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[
	  {"name":"Fed Waller speaks","datetime_utc":"2026-09-03T12:30:00Z","blackout_before_min":30,"blackout_after_min":90}
	]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	st := OverlayStatus()
	if !st.Present || st.Added != 1 || st.Skipped != 0 {
		t.Fatalf("status = %+v", st)
	}

	// It must be visible through the SAME accessors every consumer uses.
	during := time.Date(2026, 9, 3, 12, 45, 0, 0, time.UTC)
	act := ActiveAt(during)
	if act == nil {
		t.Fatal("ActiveAt did not see the overlay event — the merge is the whole feature")
	}
	if act.Name != "Fed Waller speaks" {
		t.Errorf("active = %q", act.Name)
	}
	// Window math: -30/+90 around 12:30.
	before := time.Date(2026, 9, 3, 11, 59, 0, 0, time.UTC)
	after := time.Date(2026, 9, 3, 14, 1, 0, 0, time.UTC)
	if ActiveAt(before) != nil {
		t.Error("31 minutes early must not be in blackout")
	}
	if a := ActiveAt(after); a != nil && a.Name == "Fed Waller speaks" {
		t.Error("91 minutes late must not be in blackout")
	}
	// And it must NOT have leaked into the embedded list.
	for _, e := range Embedded() {
		if e.Name == "Fed Waller speaks" {
			t.Fatal("overlay event contaminated Embedded()")
		}
	}
	if len(All()) != len(Embedded())+1 {
		t.Errorf("All()=%d Embedded()=%d, want exactly one more", len(All()), len(Embedded()))
	}
}

// Suppress neutralises a wrongly-dated committed entry without a redeploy.
func TestOverlaySuppressesEmbedded(t *testing.T) {
	resetOverlay(t)
	emb := Embedded()
	if len(emb) == 0 {
		t.Skip("no embedded events to suppress")
	}
	target := emb[0].Name
	p := writeOverlay(t, `{"events":[{"name":"`+target+`","suppress":true}]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	if st := OverlayStatus(); st.Suppress != 1 {
		t.Fatalf("suppressed = %d, want 1", st.Suppress)
	}
	for _, e := range All() {
		if e.Name == target {
			t.Fatalf("%q should have been suppressed", target)
		}
	}
	if len(All()) != len(emb)-1 {
		t.Errorf("All()=%d, want %d", len(All()), len(emb)-1)
	}
	// Suppression is a view, not a mutation.
	found := false
	for _, e := range Embedded() {
		if e.Name == target {
			found = true
		}
	}
	if !found {
		t.Error("Embedded() must still contain the suppressed event")
	}
}

// A missing overlay is the normal state and must not be an error, nor disturb
// the embedded calendar.
func TestOverlayAbsentIsNormal(t *testing.T) {
	resetOverlay(t)
	n := len(Embedded())
	if err := LoadOverlay(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("absent overlay errored: %v", err)
	}
	st := OverlayStatus()
	if st.Present || st.Err != "" {
		t.Errorf("status = %+v, want absent and clean", st)
	}
	if len(All()) != n {
		t.Errorf("All()=%d, want the embedded %d", len(All()), n)
	}
}

// The dangerous direction: a malformed overlay must NOT be able to empty the
// blackout calendar, and must not clear a previously-good overlay (an editor
// mid-save writes broken JSON for a moment).
func TestOverlayMalformedKeepsPrevious(t *testing.T) {
	resetOverlay(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "o.json")

	good := `{"events":[{"name":"Ad-hoc A","datetime_utc":"2026-09-10T12:30:00Z"}]}`
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	if OverlayStatus().Added != 1 {
		t.Fatal("setup failed")
	}

	if err := os.WriteFile(p, []byte(`{"events":[{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := LoadOverlay(p)
	if err == nil {
		t.Error("malformed JSON must report an error")
	}
	if OverlayStatus().Err == "" {
		t.Error("status must carry the parse error so /ops can show it")
	}
	// The previous overlay survives.
	if add, _ := overlaySnapshot(); len(add) != 1 {
		t.Errorf("overlay events = %d after a bad parse, want the previous 1 retained", len(add))
	}
	if len(All()) != len(Embedded())+1 {
		t.Error("a bad parse changed the effective calendar")
	}
}

// A bad entry is reported, never silently dropped.
func TestOverlayBadEntryIsReportedNotSwallowed(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[
	  {"name":"Good","datetime_utc":"2026-09-10T12:30:00Z"},
	  {"name":"Bad date","datetime_utc":"next tuesday"},
	  {"name":"","datetime_utc":"2026-09-11T12:30:00Z"},
	  {"suppress":true}
	]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	st := OverlayStatus()
	if st.Added != 1 {
		t.Errorf("added = %d, want 1", st.Added)
	}
	if st.Skipped != 3 {
		t.Errorf("skipped = %d, want 3 (bad date, no name, nameless suppress)", st.Skipped)
	}
	if len(st.Problems) != 3 {
		t.Errorf("problems = %v, want one per skipped entry", st.Problems)
	}
}

// enabled:false parks an entry without deleting it.
func TestOverlayDisabledEntry(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[
	  {"name":"On","datetime_utc":"2026-09-10T12:30:00Z"},
	  {"name":"Off","datetime_utc":"2026-09-11T12:30:00Z","enabled":false}
	]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	if st := OverlayStatus(); st.Added != 1 || st.Skipped != 0 {
		t.Errorf("status = %+v, want 1 added and 0 skipped (disabled is not an error)", st)
	}
	for _, e := range All() {
		if e.Name == "Off" {
			t.Error("a disabled entry must not be active")
		}
	}
}

// Defaults must apply, or an entry written without windows would get a
// zero-width blackout that never triggers.
func TestOverlayAppliesWindowDefaults(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[{"name":"No windows","datetime_utc":"2026-09-10T12:30:00Z"}]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	add, _ := overlaySnapshot()
	if len(add) != 1 {
		t.Fatal(len(add))
	}
	if add[0].BeforeMinutes != 60 || add[0].AfterMinutes != 60 {
		t.Errorf("windows = -%d/+%d, want the -60/+60 defaults (0 would never trigger)",
			add[0].BeforeMinutes, add[0].AfterMinutes)
	}
}

// A bare array is the shape a person writes by hand; accept it.
func TestOverlayAcceptsBareArray(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `[{"name":"Bare","datetime_utc":"2026-09-10T12:30:00Z"}]`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	if st := OverlayStatus(); st.Added != 1 {
		t.Errorf("added = %d, want 1", st.Added)
	}
}

// A datetime with no zone is read as UTC. Stated in a test because the
// alternative (local time) put every journal timestamp 8 hours off earlier the
// same day.
func TestOverlayBareDatetimeIsUTC(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[{"name":"Bare time","datetime_utc":"2026-09-10 12:30"}]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	add, _ := overlaySnapshot()
	if len(add) != 1 {
		t.Fatal(len(add))
	}
	want := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
	if !add[0].DatetimeUTC.Equal(want) {
		t.Errorf("parsed %s, want %s (bare = UTC, never local)", add[0].DatetimeUTC, want)
	}
}

// The watcher only re-reads when mtime moves — reads are on signal.Evaluate's
// hot path and must never touch the filesystem.
func TestMaybeReloadOverlayOnlyOnChange(t *testing.T) {
	resetOverlay(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "o.json")
	if err := os.WriteFile(p, []byte(`{"events":[{"name":"A","datetime_utc":"2026-09-10T12:30:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := MaybeReloadOverlay(p); err != nil || !changed {
		t.Fatalf("first read: changed=%v err=%v, want changed", changed, err)
	}
	if changed, err := MaybeReloadOverlay(p); err != nil || changed {
		t.Errorf("unchanged file: changed=%v err=%v, want no reload", changed, err)
	}

	// Bump mtime forward — a same-second rewrite can carry the same stamp on
	// coarse filesystems, so this sets it explicitly rather than sleeping.
	if err := os.WriteFile(p, []byte(`{"events":[{"name":"B","datetime_utc":"2026-09-11T12:30:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if changed, err := MaybeReloadOverlay(p); err != nil || !changed {
		t.Fatalf("after edit: changed=%v err=%v, want reload", changed, err)
	}
	add, _ := overlaySnapshot()
	if len(add) != 1 || add[0].Name != "B" {
		t.Errorf("overlay = %+v, want the edited entry B", add)
	}
}

func TestOverlayPathEnvOverride(t *testing.T) {
	if got := OverlayPath(); got != DefaultOverlayPath {
		t.Errorf("default = %q, want %q", got, DefaultOverlayPath)
	}
	t.Setenv("MACRO_OVERLAY_PATH", "/tmp/custom.json")
	if got := OverlayPath(); got != "/tmp/custom.json" {
		t.Errorf("override = %q", got)
	}
	t.Setenv("MACRO_OVERLAY_PATH", "   ")
	if got := OverlayPath(); got != DefaultOverlayPath {
		t.Errorf("whitespace override should fall back, got %q", got)
	}
}

// All() must stay sorted once the overlay interleaves entries, or /calendar
// renders them out of order.
func TestAllStaysSortedWithOverlay(t *testing.T) {
	resetOverlay(t)
	p := writeOverlay(t, `{"events":[
	  {"name":"Z late","datetime_utc":"2026-12-30T12:30:00Z"},
	  {"name":"A early","datetime_utc":"2026-07-01T12:30:00Z"}
	]}`)
	if err := LoadOverlay(p); err != nil {
		t.Fatal(err)
	}
	all := All()
	for i := 1; i < len(all); i++ {
		if all[i].DatetimeUTC.Before(all[i-1].DatetimeUTC) {
			t.Fatalf("All() unsorted at %d: %s before %s", i, all[i].DatetimeUTC, all[i-1].DatetimeUTC)
		}
	}
}
