package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/autotrade"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/risk"
)

// A key in one dictionary and not the other is exactly the bug the server
// side i18n exists to prevent: that page would show the key, or fall back to
// the other language — the mixing the old client-side scheme allowed.
func TestI18nKeysPaired(t *testing.T) {
	dicts, err := loadDicts(i18nFS)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range langs {
		for _, b := range langs {
			if a == b {
				continue
			}
			var missing []string
			for k := range dicts[a] {
				if _, ok := dicts[b][k]; !ok {
					missing = append(missing, k)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("in %s.json but not %s.json: %v", a, b, missing)
			}
		}
	}
	for _, l := range langs {
		for k, v := range dicts[l] {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s.json: %q is empty", l, k)
			}
		}
	}
}

// Every literal {{t "key"}} in a template must exist. (Keys built at run
// time, like {{t .Key}} over the shell's nav, are covered by
// TestShellKeysExist.)
func TestTemplatesUseKnownKeys(t *testing.T) {
	dicts, err := loadDicts(i18nFS)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\{\{\s*t\s+(?:\\)?"([^"\\]+)(?:\\)?"`)
	files, _ := fs.Glob(assets, "templates/*.html")
	for _, f := range files {
		raw, _ := fs.ReadFile(assets, f)
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			if _, ok := dicts[langs[0]][m[1]]; !ok {
				t.Errorf("%s uses unknown key %q", f, m[1])
			}
		}
	}
}

func TestShellKeysExist(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	for _, s := range sections {
		if _, ok := dicts[langs[0]]["nav."+s.key]; !ok {
			t.Errorf("no key nav.%s", s.key)
		}
		for _, tb := range s.tabs {
			if _, ok := dicts[langs[0]][tb.key]; !ok {
				t.Errorf("no key %s", tb.key)
			}
		}
	}
}

// Both template sets parse, and the same page renders different words.
func TestPagesParsePerLanguage(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range langs {
		if pages[l].Lookup("topnav") == nil {
			t.Errorf("%s: topnav not defined", l)
		}
	}
}

func TestLocate(t *testing.T) {
	for _, tc := range []struct {
		path      string
		sec, tabK string
	}{
		{"/", "today", "tab.today"},
		{"/today", "today", "tab.today"},
		{"/ops/scan", "today", "tab.live"},          // longer than /ops (settings)
		{"/ops/autotrade", "portfolio", "tab.auto"}, // same
		{"/ops", "settings", "tab.settings"},
		{"/journal/42/edit", "portfolio", "tab.trades"},
		{"/scan", "research", "tab.scan"},
		{"/chart", "chart", "tab.chart"},
		{"/alerts", "settings", "tab.alerts"},
	} {
		si, ti := locate(tc.path)
		if si < 0 {
			t.Errorf("%s: no section", tc.path)
			continue
		}
		if got := sections[si].key; got != tc.sec {
			t.Errorf("%s: section %s, want %s", tc.path, got, tc.sec)
		}
		if got := sections[si].tabs[ti].key; got != tc.tabK {
			t.Errorf("%s: tab %s, want %s", tc.path, got, tc.tabK)
		}
	}
	if si, _ := locate("/nope"); si != -1 {
		t.Errorf("/nope matched section %d", si)
	}
	// "/" must not swallow every path.
	if si, _ := locate("/opsx"); si != -1 {
		t.Errorf("/opsx matched a section")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/chart?symbol=ETH":     "/chart?symbol=ETH",
		"/":                     "/",
		"":                      "/",
		"//evil.example":        "/",
		"https://evil.example/": "/",
		"/\\evil.example":       "/",
		"chart":                 "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLangCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &server{}
	r := gin.New()
	r.GET("/lang/:code", s.handleLang)
	r.GET("/who", func(c *gin.Context) { c.String(200, langOf(c)) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/lang/en?next=/chart", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/chart" {
		t.Fatalf("switch: %d %q", w.Code, w.Header().Get("Location"))
	}
	ck := w.Result().Cookies()
	if len(ck) != 1 || ck[0].Value != "en" {
		t.Fatalf("cookie = %+v", ck)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/who", nil)
	req.AddCookie(ck[0])
	r.ServeHTTP(w, req)
	if w.Body.String() != "en" {
		t.Errorf("langOf with en cookie = %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), "GET", "/who", nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "fr"})
	r.ServeHTTP(w, req)
	if w.Body.String() != langs[0] {
		t.Errorf("unsupported cookie should fall back to %s, got %q", langs[0], w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/lang/fr", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("/lang/fr = %d, want 400", w.Code)
	}
}

// Keys built at run time — {{t (outcomeKey .)}}, {{t (printf "side.%s" ...)}},
// {{t .LeanKey}} — are invisible to TestTemplatesUseKnownKeys, so enumerate
// every value they can produce.
func TestDynamicKeysExist(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	keys := []string{"out.pending", "out.active", "out.win", "out.other", "side.long", "side.short", "side.neutral", "side.auto"} // auto: an autotrade rule that takes either direction
	for _, o := range journal.CloseOutcomes {
		keys = append(keys, "out."+o)
	}
	for _, s := range []Setup{
		{Event: "BOS-up"}, {Event: "BOS-down"}, {Event: "CHoCH-up"}, {Event: "CHoCH-down"},
		{InZone: true, Dir: "long"}, {InZone: true, Dir: "short"},
		{Trend: "HH-HL uptrend"}, {Trend: "LH-LL downtrend"}, {},
	} {
		keys = append(keys, leanKey(s))
	}
	for _, c := range append(knownStrategies, "unknown-future") {
		keys = append(keys, strategyKey(c))
	}
	for _, st := range []autotrade.OutcomeStatus{autotrade.OutPending, autotrade.OutNoFill, autotrade.OutOpen, autotrade.OutTP, autotrade.OutStop} {
		keys = append(keys, "at.st."+string(st))
	}
	for _, o := range []string{"", "win", "loss", "expired", "skip", "no-fill", "weird"} {
		keys = append(keys, setupOutcomeKey(o))
	}
	for _, l := range langs {
		for _, k := range keys {
			if _, ok := dicts[l][k]; !ok {
				t.Errorf("%s.json: no key %q", l, k)
			}
		}
	}
}

// Every validator factor name the chart can show has a translation. Reads
// validator/*.go directly, so a new Factor{"..."} without a dictionary entry
// fails here rather than showing up untranslated on the chart.
func TestFactorNamesTranslated(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	files, _ := filepath.Glob("../../validator/*.go")
	if len(files) == 0 {
		t.Fatal("no validator sources found")
	}
	re := regexp.MustCompile(`Factor\{"([^"]+)"`)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			n++
			for _, l := range langs {
				if _, ok := dicts[l]["factor."+m[1]]; !ok {
					t.Errorf("%s.json: no translation for factor %q (%s)", l, m[1], filepath.Base(f))
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("found no Factor{...} literals — the pattern no longer matches validator")
	}
}

// Every risk message code has a translation in each language, quoting the
// same number of values as risk.EnglishFormat — a translation that drops or
// adds a verb would print "%!" or lose a number in the refusal.
func TestRiskMessagesTranslated(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	verbs := regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]+)?[dfsvgx]`)
	for code, en := range risk.EnglishFormat {
		want := len(verbs.FindAllString(en, -1))
		for _, l := range langs {
			f, ok := dicts[l]["risk."+code]
			if !ok {
				t.Errorf("%s.json: no risk.%s", l, code)
				continue
			}
			if got := len(verbs.FindAllString(f, -1)); got != want {
				t.Errorf("%s.json risk.%s quotes %d values, EnglishFormat %d", l, code, got, want)
			}
		}
	}
}
