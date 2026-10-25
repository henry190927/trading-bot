package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
)

// Thresholds these cases rely on (heartbeat.Loop.StaleAfter = max(3 × every, 2min)):
//
//	bracket   every 30s → max(90s, 2m)  = 2m
//	autoexec  every 60s → max(180s, 2m) = 3m
//	oisample  every 60s → 3m
//
// Stalled is strictly "older than", so exactly-at-threshold is NOT stalled.
func TestCriticalStalled(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	loop := func(every int, last time.Duration) heartbeat.Loop {
		return heartbeat.Loop{Running: true, EverySec: every, LastTick: ago(last)}
	}
	critical := []string{"bracket", "autoexec"}

	cases := []struct {
		name  string
		loops map[string]heartbeat.Loop
		want  []string
	}{
		{"all fresh", map[string]heartbeat.Loop{
			"bracket": loop(30, 10*time.Second), "autoexec": loop(60, 30*time.Second),
		}, nil},
		{"bracket stuck 3m (threshold 2m)", map[string]heartbeat.Loop{
			"bracket": loop(30, 3*time.Minute), "autoexec": loop(60, 30*time.Second),
		}, []string{"bracket"}},
		{"exactly at threshold is not stuck", map[string]heartbeat.Loop{
			"bracket": loop(30, 2*time.Minute), "autoexec": loop(60, 30*time.Second),
		}, nil},
		{"autoexec 2m30s is under its 3m threshold", map[string]heartbeat.Loop{
			"bracket": loop(30, 10*time.Second), "autoexec": loop(60, 150*time.Second),
		}, nil},
		{"both stuck, sorted", map[string]heartbeat.Loop{
			"bracket": loop(30, 5*time.Minute), "autoexec": loop(60, 4*time.Minute),
		}, []string{"autoexec", "bracket"}},
		{"rule 2: a stuck NON-critical loop is not a liveness failure", map[string]heartbeat.Loop{
			"bracket": loop(30, 10*time.Second), "autoexec": loop(60, 30*time.Second),
			"oisample": loop(60, 10*time.Minute),
		}, nil},
		{"rule 3: critical loop switched Off by config", map[string]heartbeat.Loop{
			"bracket":  {Running: false, EverySec: 30, LastTick: ago(time.Hour), Reason: "BINGX_API_KEY/SECRET 未設 → 沒啟動"},
			"autoexec": loop(60, 30*time.Second),
		}, nil},
		{"rule 4: critical loop missing from the beat", map[string]heartbeat.Loop{
			"autoexec": loop(60, 30*time.Second),
		}, nil},
		{"rule 5: idle is not an exemption", map[string]heartbeat.Loop{
			"bracket":  {Running: true, Idle: true, EverySec: 30, LastTick: ago(3 * time.Minute)},
			"autoexec": loop(60, 30*time.Second),
		}, []string{"bracket"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := criticalStalled(heartbeat.Beat{Loops: c.loops}, critical, now)
			if len(got) == 0 && len(c.want) == 0 {
				return // nil and empty are both "nothing stuck"
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("criticalStalled = %v, want %v", got, c.want)
			}
		})
	}
}

// The other two endpoints do not depend on criticalStalled.
func TestPromTextAndHealthReport(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	b := heartbeat.Beat{Loops: map[string]heartbeat.Loop{
		"bracket":  {Running: true, EverySec: 30, LastTick: now.Add(-3 * time.Minute)},
		"oisample": {Running: false, Reason: "OI_SAMPLE_SEC=0 → 沒啟動"},
	}}
	text := promText(b, now)
	for _, want := range []string{
		`monitor_loop_tick_age_seconds{loop="bracket"} 180`,
		`monitor_loop_stalled{loop="bracket"} 1`,
		`monitor_loop_running{loop="oisample"} 0`,
		`monitor_loop_tick_age_seconds{loop="oisample"} -1`,
		"# TYPE monitor_loop_stalled gauge",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q\n%s", want, text)
		}
	}
	rep := healthReport(b, now)
	if m, _ := rep["critical_missing"].([]string); !reflect.DeepEqual(m, []string{"autoexec"}) {
		t.Errorf("critical_missing = %v, want [autoexec]", rep["critical_missing"])
	}
}

// A freshly started registry is healthy, and the server answers all three paths.
func TestHealthServerServes(t *testing.T) {
	reg := heartbeat.New()
	reg.Start("bracket", 30*time.Second, "")
	reg.Start("autoexec", time.Minute, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewUnstartedServer(nil)
	addr := srv.Listener.Addr().String()
	srv.Listener.Close()
	startHealthServer(ctx, addr, reg)

	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ { // the listener starts in a goroutine
		if resp, err = http.Get("http://" + addr + "/livez"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("/livez on a fresh registry = %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{"/healthz", "/metrics"} {
		r, err := http.Get("http://" + addr + p)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Errorf("%s = %v %v", p, r, err)
			continue
		}
		r.Body.Close()
	}
}
