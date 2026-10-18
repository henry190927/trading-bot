package heartbeat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStaleAfter(t *testing.T) {
	cases := []struct {
		every int
		want  time.Duration
	}{
		{0, 0},                  // no cadence, nothing to judge
		{25, 2 * time.Minute},   // 3×25s = 75s, below the floor
		{60, 3 * time.Minute},   // 3×60s = 180s
		{300, 15 * time.Minute}, // 3×5m
	}
	for _, c := range cases {
		if got := (Loop{EverySec: c.every}).StaleAfter(); got != c.want {
			t.Errorf("every=%ds: StaleAfter = %v, want %v", c.every, got, c.want)
		}
	}
}

func TestStalled(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		l    Loop
		want bool
	}{
		{"past threshold", Loop{Running: true, EverySec: 60, LastTick: now.Add(-181 * time.Second)}, true},
		{"inside threshold", Loop{Running: true, EverySec: 60, LastTick: now.Add(-179 * time.Second)}, false},
		{"not running is off, not stalled", Loop{Running: false, EverySec: 60, LastTick: now.Add(-time.Hour)}, false},
		{"no cadence never stalls", Loop{Running: true, LastTick: now.Add(-time.Hour)}, false},
		// Idle loops still tick every cycle, so idleness alone is not a stall.
		{"idle but ticking", Loop{Running: true, Idle: true, EverySec: 25, LastTick: now.Add(-10 * time.Second)}, false},
	}
	for _, c := range cases {
		if got := c.l.Stalled(now); got != c.want {
			t.Errorf("%s: Stalled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAlive(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		b    Beat
		want bool
	}{
		{"on the boundary", Beat{WrittenAt: now.Add(-60 * time.Second)}, true},
		{"one second past", Beat{WrittenAt: now.Add(-61 * time.Second)}, false},
		{"never written", Beat{}, false},
	}
	for _, c := range cases {
		if got := c.b.Alive(now); got != c.want {
			t.Errorf("%s: Alive = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRegistryLifecycle(t *testing.T) {
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	r := New()
	r.now = func() time.Time { return clock }

	r.Start("zonealert", 25*time.Second, "zone watch")
	l := r.Snapshot().Loops["zonealert"]
	if !l.Running || l.EverySec != 25 || !l.LastTick.Equal(clock) || l.Reason != "zone watch" {
		t.Fatalf("after Start: %+v", l)
	}

	clock = clock.Add(30 * time.Second)
	r.Tick("zonealert")
	r.SetIdle("zonealert", true, "enabled=false")
	l = r.Snapshot().Loops["zonealert"]
	if !l.LastTick.Equal(clock) || !l.Idle || l.Reason != "enabled=false" {
		t.Fatalf("after Tick+SetIdle: %+v", l)
	}

	// Back to active with no new reason keeps the last one rather than blanking it.
	r.SetIdle("zonealert", false, "")
	if l = r.Snapshot().Loops["zonealert"]; l.Idle || l.Reason != "enabled=false" {
		t.Fatalf("after SetIdle(false): %+v", l)
	}

	r.Off("bracket", "MONITOR_BRACKET=0")
	b := r.Snapshot().Loops["bracket"]
	if b.Running || b.Reason != "MONITOR_BRACKET=0" {
		t.Fatalf("Off: %+v", b)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hb.json")
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	in := Beat{
		PID:       42,
		StartedAt: at,
		WrittenAt: at.Add(time.Minute),
		Loops:     map[string]Loop{"bracket": {Running: true, EverySec: 30, LastTick: at}},
	}
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	// Overwrite once more: the second write must replace, not append.
	in.PID = 43
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.PID != 43 || !out.WrittenAt.Equal(in.WrittenAt) || out.Loops["bracket"].EverySec != 30 {
		t.Fatalf("round trip: %+v", out)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
	// No temp file left behind after a successful rename.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir has %v, want only hb.json", names)
	}
}

func TestReadMissing(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "absent.json")); !os.IsNotExist(err) {
		t.Fatalf("missing file: err = %v, want not-exist", err)
	}
}

// Every loop goroutine writes while the writer goroutine snapshots. Meaningful
// only under -race.
func TestRegistryConcurrent(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			r.Start(n, time.Second, "")
			for i := 0; i < 200; i++ {
				r.Tick(n)
				r.SetIdle(n, i%2 == 0, "")
			}
		}(name)
	}
	for i := 0; i < 200; i++ {
		_ = r.Snapshot()
	}
	wg.Wait()
	if got := len(r.Snapshot().Loops); got != 4 {
		t.Fatalf("loops = %d, want 4", got)
	}
}

// A loop that never started has no tick. With omitempty the zero time.Time
// was still emitted as "0001-01-01T00:00:00Z", which a reader would take as
// a real (very old) tick.
func TestOffLoopOmitsLastTick(t *testing.T) {
	data, err := json.Marshal(Loop{Running: false, Reason: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"running":false,"reason":"off"}`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}
