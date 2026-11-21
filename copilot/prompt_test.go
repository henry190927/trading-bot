package copilot

import (
	"strings"
	"testing"
)

// The chat panel's renderer (cmd/web/static/copilot-render.js) looks for
// these exact markers; the prompt must keep asking for them.
func TestSystemPromptAsksForTheRenderedFormat(t *testing.T) {
	for lang, want := range map[string][]string{
		"zh": {"結論:", "```levels", "止損:", "注意:", "壓力 / 支撐 / 樞紐區 / 現價"},
		"en": {"Verdict:", "```levels", "Stop:", "Note:", "resistance / support / zone / price"},
	} {
		p := SystemPrompt(lang)
		for _, w := range want {
			if !strings.Contains(p, w) {
				t.Errorf("%s prompt missing %q", lang, w)
			}
		}
	}
}
