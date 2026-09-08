package main

import (
	"os"
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

	t.Run("zone-only with ntfy on: zonealert + autoexec + macrowarn", func(t *testing.T) {
		ls := monitorLoops(true, true, true, true, true, "alert")
		for _, n := range []string{"autoexec", "zonealert", "macrowarn"} {
			if !get(ls, n).On {
				t.Errorf("%s should be live under MONITOR_ZONE_ONLY=1", n)
			}
		}
		for _, n := range []string{"confluence", "structalert"} {
			if get(ls, n).On {
				t.Errorf("%s must be off under MONITOR_ZONE_ONLY=1", n)
			}
		}
	})

	// The regression this whole change exists for: macrowarn used to be
	// started below the zone-only early-return in cmd/monitor/main.go, so
	// turning off confluence noise also turned off the NFP/FOMC pre-blackout
	// heads-up — discovered the morning of an NFP with nothing scheduled to
	// warn. Push is its ONLY gate now, in both modes.
	t.Run("macrowarn is gated by push alone, never by zone-only", func(t *testing.T) {
		for _, zo := range []bool{true, false} {
			if !get(monitorLoops(zo, true, true, true, true, "alert"), "macrowarn").On {
				t.Errorf("macrowarn off at zoneOnly=%v with push on", zo)
			}
			mw := get(monitorLoops(zo, false, true, true, true, "alert"), "macrowarn")
			if mw.On {
				t.Errorf("macrowarn on at zoneOnly=%v with push muted", zo)
			}
			if !strings.Contains(mw.Reason, "NTFY_TOPIC") {
				t.Errorf("zoneOnly=%v: muted reason should name push, got %q", zo, mw.Reason)
			}
		}
	})

	t.Run("muted under zone-only: autoexec and bracket", func(t *testing.T) {
		ls := monitorLoops(true, false, true, true, true, "alert")
		// These two are the loops that do NOT need push to be useful:
		// autoexec places paper orders, and bracket logs naked positions and
		// still attaches a stop for a StopAuto trade. Every other loop's only
		// output IS a push, so muting takes them down.
		wantOn := map[string]bool{"autoexec": true, "bracket": true}
		on := map[string]bool{}
		for _, l := range ls {
			if l.On {
				on[l.Name] = true
				if !wantOn[l.Name] {
					t.Errorf("%s should not be live when muted", l.Name)
				}
			}
		}
		for name := range wantOn {
			if !on[name] {
				t.Errorf("%s should stay live when muted (reason %q)", name, get(ls, name).Reason)
			}
		}
		if r := get(ls, "zonealert").Reason; !strings.Contains(r, "NTFY_TOPIC") {
			t.Errorf("zonealert reason should name the cause, got %q", r)
		}
	})

	t.Run("full mode with ntfy on: everything live", func(t *testing.T) {
		for _, l := range monitorLoops(false, true, true, true, true, "alert") {
			if !l.On {
				t.Errorf("%s should be live", l.Name)
			}
		}
	})

	t.Run("autoexec survives muting in every mode", func(t *testing.T) {
		for _, zo := range []bool{true, false} {
			for _, nt := range []bool{true, false} {
				if !get(monitorLoops(zo, nt, true, true, true, "alert"), "autoexec").On {
					t.Errorf("autoexec off at zoneOnly=%v ntfy=%v — it has no ntfy dependency", zo, nt)
				}
			}
		}
	})
}

// The gate this card originally missed: the goroutine is up (NTFY_TOPIC set)
// but zone-config's master switch short-circuits its body every cycle.
func TestMonitorLoopsZoneChannelGate(t *testing.T) {
	find := func(loops []monitorLoop, name string) monitorLoop {
		for _, l := range loops {
			if l.Name == name {
				return l
			}
		}
		t.Fatalf("loop %q missing", name)
		return monitorLoop{}
	}
	za := find(monitorLoops(true, true, false, true, true, "alert"), "zonealert")
	if za.On {
		t.Error("zonealert must read OFF when the zone channel master is disabled")
	}
	if !strings.Contains(za.Reason, "enabled=false") {
		t.Errorf("reason should name the zone-config gate, got %q", za.Reason)
	}
	if !find(monitorLoops(true, true, false, true, true, "alert"), "autoexec").On {
		t.Error("autoexec is independent of the zone channel and must stay ON")
	}
	if !find(monitorLoops(true, true, true, true, true, "alert"), "zonealert").On {
		t.Error("both gates open → zonealert ON")
	}
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

// zoneOnlyActive decides whether /ops refuses to write monitor config and
// bounce a live daemon for settings the running process cannot consume. Its
// only input is .env, so drive it through TRADING_ENV_PATH.
func TestZoneOnlyActive(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.env"
	t.Setenv("TRADING_ENV_PATH", path)

	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"set to 1", "A=1\nMONITOR_ZONE_ONLY=1\nB=2\n", true},
		{"set to 0", "MONITOR_ZONE_ONLY=0\n", false},
		{"blank value", "MONITOR_ZONE_ONLY=\n", false},
		{"key absent entirely", "A=1\n", false},
		// A trailing-space value must not read as enabled by accident.
		{"whitespace around 1", "MONITOR_ZONE_ONLY= 1 \n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := zoneOnlyActive(); got != tc.want {
				t.Errorf("zoneOnlyActive() = %v, want %v for %q", got, tc.want, tc.content)
			}
		})
	}

	// An unreadable .env must not claim the gate is active — that would block
	// config edits on a filesystem hiccup.
	t.Setenv("TRADING_ENV_PATH", dir+"/does-not-exist")
	if zoneOnlyActive() {
		t.Error("missing .env should read false, not true")
	}
}

// The bracket guard's gating differs from every other loop on this card, and
// the card's whole value is not lying about what is running. It is NOT gated
// by MONITOR_ZONE_ONLY (same reasoning as macrowarn) and NOT gated by ntfy —
// with push muted it still logs, and in place mode it still attaches the stop,
// which is degraded, not off.
func TestMonitorLoopsBracketGating(t *testing.T) {
	get := func(loops []monitorLoop, name string) monitorLoop {
		for _, l := range loops {
			if l.Name == name {
				return l
			}
		}
		t.Fatalf("loop %q missing from the card — an unlisted loop is a card that lies by omission", name)
		return monitorLoop{}
	}

	t.Run("on regardless of zone-only and ntfy", func(t *testing.T) {
		for _, zo := range []bool{true, false} {
			for _, nt := range []bool{true, false} {
				bg := get(monitorLoops(zo, nt, true, true, true, "alert"), "bracket")
				if !bg.On {
					t.Errorf("zoneOnly=%v ntfy=%v: bracket reported off (%s)", zo, nt, bg.Reason)
				}
			}
		}
	})

	t.Run("muted push is a caveat, not an off switch", func(t *testing.T) {
		bg := get(monitorLoops(true, false, true, true, true, "alert"), "bracket")
		if !bg.On {
			t.Fatal("bracket off with ntfy muted; want on with a caveat")
		}
		if !strings.Contains(bg.Reason, "NTFY") {
			t.Errorf("reason %q does not mention the muted push", bg.Reason)
		}
	})

	t.Run("MONITOR_BRACKET=0 is off", func(t *testing.T) {
		bg := get(monitorLoops(false, true, true, false, true, "alert"), "bracket")
		if bg.On {
			t.Error("bracket reported on with MONITOR_BRACKET=0")
		}
		if !strings.Contains(bg.Reason, "MONITOR_BRACKET") {
			t.Errorf("reason %q does not name the flag that disabled it", bg.Reason)
		}
	})

	t.Run("a missing API key is off, and says so", func(t *testing.T) {
		bg := get(monitorLoops(false, true, true, true, false, "alert"), "bracket")
		if bg.On {
			t.Error("bracket reported on with no API key")
		}
		if !strings.Contains(bg.Reason, "BINGX_API_KEY") {
			t.Errorf("reason %q does not name the missing credential", bg.Reason)
		}
	})

	t.Run("mode is visible, because alert and place do different things", func(t *testing.T) {
		alert := get(monitorLoops(false, true, true, true, true, "alert"), "bracket")
		place := get(monitorLoops(false, true, true, true, true, "place"), "bracket")
		if !strings.Contains(alert.Reason, "alert") {
			t.Errorf("alert-mode reason %q does not say so", alert.Reason)
		}
		if !strings.Contains(place.Reason, "place") {
			t.Errorf("place-mode reason %q does not say so", place.Reason)
		}
		if alert.Reason == place.Reason {
			t.Error("alert and place render identically — the card cannot show which one is live")
		}
	})
}
