package main

import (
	"strings"
	"testing"
	"time"
)

// The .env rewrite is the only part of the mute toggle that can silently
// destroy something (the topic value lives nowhere else once blanked), so
// every branch is traced by hand against a realistic file shape.
func TestToggleNtfyEnv(t *testing.T) {
	const live = "BINGX_API_KEY=abc\nNTFY_TOPIC=uuid-1234\nMONITOR_ZONE_ONLY=1\n"
	const muted = "BINGX_API_KEY=abc\nNTFY_TOPIC=\nNTFY_TOPIC_MUTED=uuid-1234\nMONITOR_ZONE_ONLY=1\n"

	t.Run("mute stashes the topic in place", func(t *testing.T) {
		out, changed, err := toggleNtfyEnv(live, true)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if out != muted {
			t.Fatalf("got:\n%q\nwant:\n%q", out, muted)
		}
	})

	t.Run("unmute restores it and drops the stash", func(t *testing.T) {
		out, changed, err := toggleNtfyEnv(muted, false)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if out != live {
			t.Fatalf("got:\n%q\nwant:\n%q", out, live)
		}
	})

	t.Run("round trip is lossless", func(t *testing.T) {
		m, _, _ := toggleNtfyEnv(live, true)
		back, _, err := toggleNtfyEnv(m, false)
		if err != nil {
			t.Fatal(err)
		}
		if back != live {
			t.Fatalf("round trip lost content:\n%q", back)
		}
	})

	t.Run("both directions are idempotent", func(t *testing.T) {
		if _, changed, err := toggleNtfyEnv(muted, true); changed || err != nil {
			t.Errorf("re-mute: changed=%v err=%v, want no-op", changed, err)
		}
		if _, changed, err := toggleNtfyEnv(live, false); changed || err != nil {
			t.Errorf("re-unmute: changed=%v err=%v, want no-op", changed, err)
		}
	})

	t.Run("unmute without a stash refuses rather than inventing a topic", func(t *testing.T) {
		_, changed, err := toggleNtfyEnv("BINGX_API_KEY=abc\nNTFY_TOPIC=\n", false)
		if err == nil {
			t.Fatal("want an error when there is nothing to restore")
		}
		if changed {
			t.Error("must not report a change on failure")
		}
	})

	t.Run("no NTFY_TOPIC line at all is silent, not an error", func(t *testing.T) {
		src := "BINGX_API_KEY=abc\n"
		out, changed, err := toggleNtfyEnv(src, true)
		if err != nil || changed || out != src {
			t.Fatalf("want untouched no-op, got changed=%v err=%v out=%q", changed, err, out)
		}
	})

	t.Run("unmute with only a stash line reuses that line", func(t *testing.T) {
		// The NTFY_TOPIC= line was hand-deleted while muted; the stash is the
		// only record of the topic, so it becomes the live key.
		out, changed, err := toggleNtfyEnv("A=1\nNTFY_TOPIC_MUTED=uuid-1234\nB=2\n", false)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if out != "A=1\nNTFY_TOPIC=uuid-1234\nB=2\n" {
			t.Fatalf("got %q", out)
		}
		if strings.Contains(out, "NTFY_TOPIC_MUTED") {
			t.Error("stash line should be gone")
		}
	})

	t.Run("a stale stash is overwritten, not duplicated", func(t *testing.T) {
		// Muting while an old stash exists must capture the CURRENT topic.
		out, changed, err := toggleNtfyEnv("NTFY_TOPIC=new-uuid\nNTFY_TOPIC_MUTED=old-uuid\n", true)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if !strings.Contains(out, "NTFY_TOPIC_MUTED=new-uuid") {
			t.Fatalf("stash not refreshed: %q", out)
		}
		if strings.Contains(out, "old-uuid") {
			t.Errorf("stale topic survived: %q", out)
		}
		if strings.Count(out, "NTFY_TOPIC_MUTED=") != 1 {
			t.Errorf("stash duplicated: %q", out)
		}
	})

	t.Run("credentials and other keys are never touched", func(t *testing.T) {
		src := "BINGX_API_SECRET=s3cret\nNTFY_TOPIC=t\nGEMINI_API_KEY=g\n"
		out, _, _ := toggleNtfyEnv(src, true)
		for _, keep := range []string{"BINGX_API_SECRET=s3cret", "GEMINI_API_KEY=g"} {
			if !strings.Contains(out, keep) {
				t.Errorf("lost %q from:\n%s", keep, out)
			}
		}
	})
}

// monitorLoops must mirror cmd/monitor/main.go's gating. The case that
// actually bites: muting ntfy under MONITOR_ZONE_ONLY=1 leaves autoexec as
// the ONLY live loop — the panel has to show that, not imply the monitor is
// still watching zones.
func TestMonitorLoops(t *testing.T) {
	get := func(loops []monitorLoop, name string) monitorLoop {
		for _, l := range loops {
			if l.Name == name {
				return l
			}
		}
		t.Fatalf("loop %q missing", name)
		return monitorLoop{}
	}

	t.Run("zone-only with ntfy on: zonealert + autoexec", func(t *testing.T) {
		ls := monitorLoops(true, true)
		if !get(ls, "autoexec").On || !get(ls, "zonealert").On {
			t.Error("autoexec and zonealert should both be live")
		}
		for _, n := range []string{"confluence", "structalert", "macrowarn"} {
			if get(ls, n).On {
				t.Errorf("%s must be off under MONITOR_ZONE_ONLY=1", n)
			}
		}
	})

	t.Run("muted under zone-only: autoexec alone", func(t *testing.T) {
		ls := monitorLoops(true, false)
		on := 0
		for _, l := range ls {
			if l.On {
				on++
				if l.Name != "autoexec" {
					t.Errorf("%s should not be live when muted", l.Name)
				}
			}
		}
		if on != 1 {
			t.Errorf("live loops = %d, want exactly 1 (autoexec)", on)
		}
		if r := get(ls, "zonealert").Reason; !strings.Contains(r, "NTFY_TOPIC") {
			t.Errorf("zonealert reason should name the cause, got %q", r)
		}
	})

	t.Run("full mode with ntfy on: everything live", func(t *testing.T) {
		for _, l := range monitorLoops(false, true) {
			if !l.On {
				t.Errorf("%s should be live", l.Name)
			}
		}
	})

	t.Run("autoexec survives muting in every mode", func(t *testing.T) {
		for _, zo := range []bool{true, false} {
			for _, nt := range []bool{true, false} {
				if !get(monitorLoops(zo, nt), "autoexec").On {
					t.Errorf("autoexec off at zoneOnly=%v ntfy=%v — it has no ntfy dependency", zo, nt)
				}
			}
		}
	})
}

func TestHumanSince(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{16 * time.Hour, "16h 0m"},
		{50 * time.Hour, "2d 2h"},
		{-time.Second, ""},
	} {
		if got := humanSince(tc.d); got != tc.want {
			t.Errorf("humanSince(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
