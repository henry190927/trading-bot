package main

import (
	"strings"
	"testing"
	"time"

	"github.com/henry190927/trading-bot/protect"
	"github.com/henry190927/trading-bot/session"
)

// The English dictionary entry must say exactly what session.StopWarn.English
// says, so the CLI, the daemon and an English page never drift apart.
func TestSessionWarnEnglishMatchesPackage(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	s := &server{dicts: dicts}
	for _, w := range []session.StopWarn{
		{StopPct: 0.2, MedianPct: 3.75, Samples: 12, UntilOpen: 3 * time.Hour},
		{StopPct: 0.2, MedianPct: 3.75, Samples: 12},
	} {
		if got, want := sessionWarnText(s.translator("en"), w), w.English(); got != want {
			t.Errorf("en dictionary drifted:\n got %q\nwant %q", got, want)
		}
		if zh := sessionWarnText(s.translator("zh"), w); strings.Contains(zh, "stop is") {
			t.Errorf("zh rendered English: %q", zh)
		}
	}
}

// Every protect fault code has both translations, and the English entry is
// the exact sentence cmd/protect prints.
func TestProtectFaultsTranslated(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	for code, format := range protect.EnglishFormat {
		for _, l := range langs {
			if _, ok := dicts[l]["protect."+code]; !ok {
				t.Errorf("%s.json: no key protect.%s", l, code)
			}
		}
		if got := dicts["en"]["protect."+code]; got != format {
			t.Errorf("en protect.%s = %q, want the CLI wording %q", code, got, format)
		}
	}
	for _, code := range []string{"ghost-stop", "naked", "naked-no-stop", "stale"} {
		for _, l := range langs {
			if _, ok := dicts[l]["verify.warn."+code]; !ok {
				t.Errorf("%s.json: no key verify.warn.%s", l, code)
			}
		}
	}
}
