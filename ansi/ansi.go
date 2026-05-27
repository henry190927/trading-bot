// Package ansi provides minimal ANSI color helpers shared by CLI commands.
// Colors are auto-disabled when stdout isn't a TTY or NO_COLOR is set, so
// piped output stays clean for grepping and file redirection.
package ansi

import (
	"os"
	"strings"
)

const (
	Reset  = "\x1b[0m"
	Bold   = "\x1b[1m"
	Dim    = "\x1b[2m"
	Red    = "\x1b[31m"
	Green  = "\x1b[32m"
	Yellow = "\x1b[33m"
	Cyan   = "\x1b[36m"
	BoldR  = "\x1b[1;31m"
	BoldG  = "\x1b[1;32m"
	BoldY  = "\x1b[1;33m"
	BoldC  = "\x1b[1;36m"
)

// Enabled is determined once at process start: true when stdout is a TTY
// and NO_COLOR is unset.
var Enabled = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}()

// Wrap s with the ANSI code if colors are enabled; otherwise return s as-is.
func Wrap(s, code string) string {
	if !Enabled || s == "" {
		return s
	}
	return code + s + Reset
}

// VisibleLen returns the rune count of s ignoring ANSI escape sequences,
// so callers can pad colored cells correctly in tables.
func VisibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// PadR right-pads s with spaces so its visible length reaches total.
func PadR(s string, total int) string {
	pad := total - VisibleLen(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}
