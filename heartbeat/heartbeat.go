// Package heartbeat lets a long-running daemon report, from the inside, which
// of its loops are running and when each one last started a cycle.
//
// It exists to replace inference with observation. /ops used to decide which
// monitor loops were "on" by re-reading .env and re-deriving cmd/monitor's
// gating logic — two copies of the same rules, kept in sync by hand. When they
// drifted, the panel reported a loop as ON for a full day while it was
// short-circuiting every cycle. Here the daemon says what it is doing and a
// reader only judges how fresh that statement is.
//
// The transport is a JSON file in the shared data directory, rewritten
// atomically. That assumes the writer and the reader share a host (and a
// clock); if they ever stop sharing one, this becomes an HTTP endpoint.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// WriteEvery is how often the daemon rewrites the file. MaxAge is how old the
// file may get before a reader treats the daemon as down: four missed writes,
// so a single slow write never reads as an outage.
const (
	WriteEvery = 15 * time.Second
	MaxAge     = 60 * time.Second
)

// minStaleAfter is the floor on a loop's stall threshold. Three intervals is
// the rule, but a 25s loop whose cycle legitimately includes a slow exchange
// call would flap at 75s.
const minStaleAfter = 2 * time.Minute

// Path is the heartbeat file (env MONITOR_HEARTBEAT or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("MONITOR_HEARTBEAT")); p != "" {
		return p
	}
	return "/opt/trading/monitor-heartbeat.json"
}

// Loop is one goroutine's self-report.
//
// Running means the goroutine is alive. Idle means it is alive but its config
// tells it to do nothing this cycle — the state the old inferred panel could
// not represent. A loop that never started has Running false and a Reason.
type Loop struct {
	Running  bool      `json:"running"`
	Idle     bool      `json:"idle,omitempty"`
	EverySec int       `json:"every_sec,omitempty"`
	LastTick time.Time `json:"last_tick,omitzero"` // omitzero, not omitempty: a zero time.Time is a struct, which omitempty never drops
	Reason   string    `json:"reason,omitempty"`
}

// StaleAfter is how long a running loop may go without a tick before a
// reader calls it stalled. Zero means the loop has no cadence to judge.
func (l Loop) StaleAfter() time.Duration {
	if l.EverySec <= 0 {
		return 0
	}
	d := 3 * time.Duration(l.EverySec) * time.Second
	if d < minStaleAfter {
		d = minStaleAfter
	}
	return d
}

// Stalled reports a loop whose goroutine is alive but has not started a
// cycle in StaleAfter — typically blocked inside a network call. systemd
// cannot see this: the process is still active.
func (l Loop) Stalled(now time.Time) bool {
	after := l.StaleAfter()
	return l.Running && after > 0 && now.Sub(l.LastTick) > after
}

// Beat is the whole file.
type Beat struct {
	PID       int             `json:"pid"`
	StartedAt time.Time       `json:"started_at"`
	WrittenAt time.Time       `json:"written_at"`
	Loops     map[string]Loop `json:"loops"`
}

// Alive reports whether the daemon wrote recently enough to be trusted.
func (b Beat) Alive(now time.Time) bool {
	return !b.WrittenAt.IsZero() && now.Sub(b.WrittenAt) <= MaxAge
}

// Names returns the loop names in a stable order for display.
func (b Beat) Names() []string {
	names := make([]string, 0, len(b.Loops))
	for n := range b.Loops {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Registry is the daemon-side state. Every method is safe to call from any
// goroutine.
type Registry struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	loops   map[string]Loop
}

// New returns an empty registry stamped with the process start time.
func New() *Registry {
	r := &Registry{now: time.Now, loops: map[string]Loop{}}
	r.started = r.now()
	return r
}

// Start marks a loop as running with the given cadence. Starting counts as a
// tick, so a loop is not reported stalled before its first cycle comes due.
func (r *Registry) Start(name string, every time.Duration, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loops[name] = Loop{
		Running:  true,
		EverySec: int(every / time.Second),
		LastTick: r.now(),
		Reason:   reason,
	}
}

// Tick records the start of a cycle.
func (r *Registry) Tick(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.loops[name]
	l.LastTick = r.now()
	r.loops[name] = l
}

// SetIdle records whether the loop is skipping its work this cycle, and why.
// An empty reason on the way back to active keeps the reason given at Start.
func (r *Registry) SetIdle(name string, idle bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.loops[name]
	l.Idle = idle
	if reason != "" {
		l.Reason = reason
	}
	r.loops[name] = l
}

// Off records a loop that did not start, or has exited, and why.
func (r *Registry) Off(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.loops[name]
	l.Running = false
	l.Idle = false
	l.Reason = reason
	r.loops[name] = l
}

// Snapshot copies the current state into a Beat.
func (r *Registry) Snapshot() Beat {
	r.mu.Lock()
	defer r.mu.Unlock()
	loops := make(map[string]Loop, len(r.loops))
	for k, v := range r.loops {
		loops[k] = v
	}
	return Beat{
		PID:       os.Getpid(),
		StartedAt: r.started,
		WrittenAt: r.now(),
		Loops:     loops,
	}
}

// Run writes a snapshot immediately and then every WriteEvery until ctx ends.
// A failed write is logged, never fatal: losing the heartbeat must not take
// the daemon down with it, and a reader will see the file go stale.
func (r *Registry) Run(ctx context.Context, path string, logf func(string, ...any)) {
	write := func() {
		if err := Write(path, r.Snapshot()); err != nil {
			logf("heartbeat: write %s: %v", path, err)
		}
	}
	write()
	tick := time.NewTicker(WriteEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			write()
		}
	}
}

// Write replaces path atomically: the JSON goes to a temp file in the same
// directory, which is then renamed over the target. rename(2) within one
// filesystem is atomic, so a reader sees the old file or the new one, never a
// half-written one.
func Write(path string, b Beat) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".heartbeat-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename has succeeded
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close() // the write error is the one worth returning
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the web service reads it as the same
	// user today, but a container running as another UID would not.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Read loads a heartbeat file. A missing file is an error the caller should
// show as "no heartbeat", not as "down": the daemon may predate this package.
func Read(path string) (Beat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Beat{}, err
	}
	var b Beat
	if err := json.Unmarshal(data, &b); err != nil {
		return Beat{}, fmt.Errorf("heartbeat: decode %s: %w", path, err)
	}
	return b, nil
}
