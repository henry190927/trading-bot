// Package monitorcfg is the monitor's runtime config: settings the web panel
// may change while the monitor is running, re-read by the monitor on every
// use instead of at startup.
//
// It exists to keep the web service out of .env. The mute toggle used to
// work by rewriting .env (stashing NTFY_TOPIC) and restarting the monitor,
// which meant the most exposed process in the system could rewrite the file
// holding the exchange keys. .env now holds secrets only and the web never
// writes it; anything the panel changes lives here.
//
// Same pattern as zone-config.json and autotrade.json: a small JSON file in
// the data directory, hot-reloaded by its reader.
package monitorcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the whole file. A missing file is the zero value: push on.
type Config struct {
	// PushMuted silences ntfy pushes without stopping anything: loops keep
	// running and evaluating, and zone events are still recorded (marked as
	// not pushed). Unlike clearing NTFY_TOPIC, it needs no restart.
	PushMuted bool `json:"push_muted"`
}

// Path is the config file (env MONITOR_CONFIG or default).
func Path() string {
	if p := strings.TrimSpace(os.Getenv("MONITOR_CONFIG")); p != "" {
		return p
	}
	return "/opt/trading/monitor-config.json"
}

// Read loads the config. A missing file is not an error. A file that does
// not parse returns the zero value AND the error: the zero value means push
// stays on, which is the safe direction — the bracket guard's naked-position
// alarm rides the same channel, and a corrupt file must not silence it.
func Read(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("monitorcfg: decode %s: %w", path, err)
	}
	return c, nil
}

// Write replaces the file atomically (temp file + rename in the same
// directory), so the monitor never reads a half-written config.
func Write(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".monitor-config-*.tmp")
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
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
