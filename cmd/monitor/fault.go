package main

// Fault injection for the K8s liveness experiment (k8s/README.md).
//
//	MONITOR_FAULT_STALL=autoexec:90s
//
// makes the named loop block for good the first time it ticks 90s or more
// after process start, while the process and its HTTP server stay up — the
// "alive but stuck" failure /livez exists to catch. Unset (always, outside
// that experiment) it does nothing.

import (
	"context"
	"log"
	"os"
	"strings"
	"time"
)

var processStart = time.Now()

// injectStall blocks the calling loop until ctx ends if MONITOR_FAULT_STALL
// names it and its delay has passed. Blocking on ctx (not forever) keeps a
// SIGTERM shutdown clean.
func injectStall(ctx context.Context, loop string) {
	spec := strings.TrimSpace(os.Getenv("MONITOR_FAULT_STALL"))
	name, after, ok := strings.Cut(spec, ":")
	if spec == "" || name != loop {
		return
	}
	delay := time.Duration(0)
	if ok {
		d, err := time.ParseDuration(after)
		if err != nil {
			return
		}
		delay = d
	}
	if time.Since(processStart) < delay {
		return
	}
	log.Printf("FAULT INJECTION: %s loop now blocked (MONITOR_FAULT_STALL=%s)", loop, spec)
	<-ctx.Done()
}
