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

// The chat drops the one-shot output sections of the shared Quant prompt
// (flash-lite followed them and wrote [EN]/[ZH-TW] essays) and keeps the
// strategy knowledge.
func TestSystemPromptDropsTheOneShotFormat(t *testing.T) {
	p := SystemPrompt("zh")
	for _, gone := range []string{"[/EN]", "[ZH-TW]\n", "MANDATORY structure", "three flavors"} {
		if strings.Contains(p, gone) {
			t.Errorf("chat prompt still contains %q", gone)
		}
	}
	for _, kept := range []string{"# Persona & tone", "# Strategy context", "# Backtest-known facts", "# Discipline rules", "# Copilot chat mode"} {
		if !strings.Contains(p, kept) {
			t.Errorf("chat prompt lost %q", kept)
		}
	}
}
