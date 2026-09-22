package autostrat

import (
	"os"
	"regexp"
	"testing"
)

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

var caseRe = regexp.MustCompile(`case "([a-z-]+)":`)

func casesIn(src string) []string {
	var out []string
	for _, m := range caseRe.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}
