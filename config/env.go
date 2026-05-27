package config

import (
	"os"
	"strings"
)

// LoadDotEnv reads a .env file (if present) and exports KEY=VALUE pairs to
// the process environment. Existing env vars take precedence — the file is
// only used to fill in unset keys.
//
// Lookup order: ./.env, ./trading/.env. Call once at program start.
//
// Format:
//   KEY=value           # comment
//   KEY="quoted value"
//   # full-line comment
//
// Empty lines and lines without '=' are ignored.
func LoadDotEnv() {
	for _, path := range []string{".env", "trading/.env"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			i := strings.Index(line, "=")
			if i <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:i])
			val := strings.TrimSpace(line[i+1:])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				_ = os.Setenv(key, val)
			}
		}
		return
	}
}
