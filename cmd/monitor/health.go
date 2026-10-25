package main

// HTTP health endpoints for running the monitor under Kubernetes.
//
// The monitor has no HTTP of its own and its image is FROM scratch (no shell),
// so a liveness probe cannot `exec cat` a file. It already keeps a per-loop
// heartbeat in memory (package heartbeat); this file hands that to three
// readers over HTTP:
//
//	/livez    kubelet's liveness probe. Fails ONLY when a critical loop is
//	          stuck — the one case a restart can actually fix.
//	/healthz  every loop's state as JSON, for people and /ops. Never fails.
//	/metrics  per-loop tick age in Prometheus text format, so a SLOWER
//	          threshold on the non-critical loops can alert without restarting.
//
// Two tiers on purpose: restarting the whole daemon because a background
// cache refresh is late would also interrupt the naked-position guard.
//
// Off unless MONITOR_HEALTH_ADDR is set. On the VPS the container runs with
// host networking, so a listener here would be a new port on the host; the
// k8s manifest sets it, compose does not.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henry190927/trading-bot/heartbeat"
)

// criticalLoops are the loops whose stall justifies a restart: the
// naked-position guard (money is unprotected while it is stuck) and the
// auto-executor (orders are missed or left half-placed once live).
var criticalLoops = []string{"bracket", "autoexec"}

// criticalStalled returns, sorted, the names in critical whose loop is stuck.
//
// ═══════════════════════════ 這段你來寫 ═══════════════════════════
// Contract (each rule is a case in health_test.go):
//
//  1. "Stuck" is exactly heartbeat.Loop.Stalled(now) — reuse it, do not
//     re-derive the threshold here.
//  2. Only names in critical count. A stalled non-critical loop is NOT
//     returned (it is the /metrics tier's job).
//  3. A critical loop that is Off (Running == false) is NOT returned:
//     it was switched off by config, and a restart cannot fix config —
//     failing liveness for it would crash-loop the whole daemon.
//  4. A critical name missing from b.Loops is NOT returned, for the same
//     reason (a restart will not make it appear). /healthz shows it.
//  5. Idle loops are NOT exempt: an idle loop still ticks every cycle, so
//     an idle loop that stopped ticking is stuck too. (Stalled already
//     ignores Idle — rule 1 covers this if you do not add an exception.)
//  6. The result is sorted, so the 503 body is stable; nil or empty when
//     nothing is stuck.
//
// Hint: about 8 lines. Look up each critical name in b.Loops.
// ════════════════════════════════════════════════════════════════════
func criticalStalled(b heartbeat.Beat, critical []string, now time.Time) []string {
	res := make([]string, 0, len(critical))
	for _, c := range critical {
		// exists is checked on purpose: a missing name (rule 4) would also
		// fall through via the zero Loop{} (Running=false), but saying it
		// explicitly beats relying on that.
		if l, exists := b.Loops[c]; exists && l.Stalled(now) {
			res = append(res, c)
		}
	}
	sort.Strings(res)
	return res
}

// startHealthServer serves the three endpoints on addr until ctx ends. A
// listen failure is logged and returns: losing the health port must never
// take the monitor down with it.
func startHealthServer(ctx context.Context, addr string, reg *heartbeat.Registry) {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		stuck := criticalStalled(reg.Snapshot(), criticalLoops, time.Now())
		w.Header().Set("Content-Type", "application/json")
		if len(stuck) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": len(stuck) == 0, "stalled": stuck})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthReport(reg.Snapshot(), time.Now()))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(promText(reg.Snapshot(), time.Now())))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "health: %v (monitor keeps running without health endpoints)\n", err)
		}
	}()
}

// loopReport is one loop as /healthz shows it.
type loopReport struct {
	heartbeat.Loop
	AgeSec   float64 `json:"age_sec"`
	Stalled  bool    `json:"stalled"`
	Critical bool    `json:"critical"`
}

// healthReport is every loop's state plus the critical names that never
// registered at all (rule 4: not a liveness failure, but worth seeing).
func healthReport(b heartbeat.Beat, now time.Time) map[string]any {
	crit := map[string]bool{}
	for _, n := range criticalLoops {
		crit[n] = true
	}
	loops := map[string]loopReport{}
	for name, l := range b.Loops {
		loops[name] = loopReport{Loop: l, AgeSec: tickAge(l, now), Stalled: l.Stalled(now), Critical: crit[name]}
	}
	var missing []string
	for _, n := range criticalLoops {
		if _, ok := b.Loops[n]; !ok {
			missing = append(missing, n)
		}
	}
	return map[string]any{"started_at": b.StartedAt, "loops": loops, "critical_missing": missing}
}

// promText renders three gauges per loop. Hand-written rather than pulling in
// the Prometheus client: the format is three lines of text per metric.
func promText(b heartbeat.Beat, now time.Time) string {
	var sb strings.Builder
	gauge := func(name, help string, val func(heartbeat.Loop) float64) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, n := range b.Names() {
			fmt.Fprintf(&sb, "%s{loop=%q} %g\n", name, n, val(b.Loops[n]))
		}
	}
	gauge("monitor_loop_tick_age_seconds", "Seconds since the loop last started a cycle.",
		func(l heartbeat.Loop) float64 { return tickAge(l, now) })
	gauge("monitor_loop_running", "1 if the loop's goroutine is running.",
		func(l heartbeat.Loop) float64 { return b2f(l.Running) })
	gauge("monitor_loop_stalled", "1 if the loop is running but past its stall threshold.",
		func(l heartbeat.Loop) float64 { return b2f(l.Stalled(now)) })
	return sb.String()
}

func tickAge(l heartbeat.Loop, now time.Time) float64 {
	if l.LastTick.IsZero() {
		return -1
	}
	return now.Sub(l.LastTick).Seconds()
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
