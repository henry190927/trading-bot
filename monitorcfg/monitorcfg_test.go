package monitorcfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMissingIsUnmuted(t *testing.T) {
	c, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || c.PushMuted {
		t.Fatalf("missing file: %+v, %v — want unmuted, no error", c, err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-config.json")
	for _, want := range []bool{true, false} {
		if err := Write(path, Config{PushMuted: want}); err != nil {
			t.Fatal(err)
		}
		c, err := Read(path)
		if err != nil || c.PushMuted != want {
			t.Fatalf("after Write(%v): %+v, %v", want, c, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the config (no temp files left)", len(entries))
	}
}

// A corrupt file must not mute: the naked-position alarm shares the channel.
func TestReadCorruptFailsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor-config.json")
	if err := os.WriteFile(path, []byte(`{"push_muted": tru`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Read(path)
	if err == nil {
		t.Fatal("corrupt file: want an error to surface")
	}
	if c.PushMuted {
		t.Fatal("corrupt file: muted — want push left on")
	}
}
