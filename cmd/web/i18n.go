// Server-side i18n and the page shell.
//
// One language per page, decided on the server from the `lang` cookie. This
// replaces the client-side scheme where every string shipped in both
// languages (`.i18n-en`/`.i18n-zh` span pairs, `data-i18n` filled by JS) and
// the browser hid one: that put both languages in the HTML, flashed on load,
// and let a string with no translation silently show the other language.
//
// The templates are parsed once per language, each set with its own `t`
// function bound in, so a template writes {{t "nav.today"}} and never has to
// thread the language through. TestI18nKeysPaired fails the build when a key
// exists in one dictionary and not the other, and TestTemplatesUseKnownKeys
// when a template asks for a key neither has.
//
// Migration: the shell (rail, top bar, section tabs) is fully server-side
// now. Page bodies move over page by page; until a page does, its legacy
// strings follow the same language through <html data-lang>, which the
// shell sets from the cookie.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
)

//go:embed i18n/*.json
var i18nFS embed.FS

// langs is every supported language; the first is the default.
var langs = []string{"zh", "en"}

const langCookie = "lang"

// loadDicts reads i18n/<lang>.json for every language.
func loadDicts(fsys fs.FS) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, l := range langs {
		raw, err := fs.ReadFile(fsys, "i18n/"+l+".json")
		if err != nil {
			return nil, err
		}
		d := map[string]string{}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("i18n/%s.json: %w", l, err)
		}
		out[l] = d
	}
	return out, nil
}

// parsePages parses the templates once per language with that language's
// `t` bound in. A missing key renders as the key itself — visible, and the
// tests keep it from shipping.
func parsePages(fsys fs.FS, dicts map[string]map[string]string) (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	for _, l := range langs {
		d := dicts[l]
		funcs := templateFuncs()
		funcs["t"] = func(key string) string {
			if v, ok := d[key]; ok {
				return v
			}
			return key
		}
		t, err := template.New("").Funcs(funcs).ParseFS(fsys, "templates/*.html")
		if err != nil {
			return nil, err
		}
		out[l] = t
	}
	return out, nil
}

// langOf is the request's language: the cookie when it names a supported
// language, else the default.
func langOf(c *gin.Context) string {
	if v, err := c.Cookie(langCookie); err == nil {
		for _, l := range langs {
			if v == l {
				return l
			}
		}
	}
	return langs[0]
}

// page renders a full page in the request's language with the shell's data
// added under "Shell". Every HTML page goes through here.
func (s *server) page(c *gin.Context, code int, name string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	lang := langOf(c)
	data["Shell"] = buildShell(c.Request.URL.Path, c.Request.URL.RequestURI(), lang)
	c.Render(code, render.HTML{Template: s.pages[lang], Name: name, Data: data})
}

// handleLang — GET /lang/:code?next=/path. Sets the cookie and goes back.
// A plain link, not a JS toggle: the whole page re-renders in one language.
func (s *server) handleLang(c *gin.Context) {
	code := c.Param("code")
	ok := false
	for _, l := range langs {
		ok = ok || code == l
	}
	if !ok {
		c.String(http.StatusBadRequest, "unsupported language")
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(langCookie, code, 365*24*3600, "/", "", false, true)
	c.Redirect(http.StatusSeeOther, safeNext(c.Query("next")))
}

// safeNext keeps a redirect target on this site: a path, not "//host" or a
// full URL, which would make /lang an open redirect.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// ── Shell: rail + top bar + section tabs ─────────────────────────────

type shellNav struct {
	Key, Href, Icon string
	On              bool
}

type shellTab struct {
	Key, Href string
	On        bool
}

type shellData struct {
	Lang      string // zh | en
	HTMLLang  string // zh-Hant | en
	OtherLang string // the language the toggle switches to
	Here      string // request URI, for the language switch to come back to
	Nav       []shellNav
	Tabs      []shellTab
}

// section groups pages under one rail item. A page is in the section whose
// tab prefix matches it most specifically, so /ops/autotrade lands in
// portfolio even though /ops is settings.
type section struct {
	key, href, icon string
	tabs            []tab
}

type tab struct {
	key, href string
	match     []string // path prefixes that light this tab; href is implied
}

var sections = []section{
	{"today", "/today", "today", []tab{
		{"tab.today", "/today", []string{"/"}},
		{"tab.live", "/ops/scan", nil},
	}},
	{"chart", "/chart", "chart", []tab{
		{"tab.chart", "/chart", nil},
		{"tab.validate", "/validate", nil},
	}},
	{"portfolio", "/journal", "portfolio", []tab{
		{"tab.trades", "/journal", nil},
		{"tab.setups", "/setups", nil},
		{"tab.auto", "/ops/autotrade", nil},
		{"tab.verify", "/ops/verify", nil},
	}},
	{"research", "/calendar", "research", []tab{
		{"tab.calendar", "/calendar", nil},
		{"tab.scan", "/scan", nil},
		{"tab.tips", "/tips", nil},
	}},
	{"settings", "/ops", "settings", []tab{
		{"tab.settings", "/ops", nil},
		{"tab.alerts", "/alerts", nil},
	}},
}

// prefixMatch reports whether path is p or under it. "/" matches only "/".
func prefixMatch(path, p string) bool {
	if p == "/" {
		return path == "/"
	}
	return path == p || strings.HasPrefix(path, p+"/")
}

// locate finds the section and tab for a path by the longest matching
// prefix; (-1, -1) when nothing matches.
func locate(path string) (si, ti int) {
	si, ti = -1, -1
	best := -1
	for i, s := range sections {
		for j, t := range s.tabs {
			for _, p := range append([]string{t.href}, t.match...) {
				if prefixMatch(path, p) && len(p) > best {
					si, ti, best = i, j, len(p)
				}
			}
		}
	}
	return si, ti
}

func buildShell(path, here, lang string) shellData {
	sh := shellData{Lang: lang, HTMLLang: "en", OtherLang: "zh", Here: here}
	if lang == "zh" {
		sh.HTMLLang, sh.OtherLang = "zh-Hant", "en"
	}
	si, ti := locate(path)
	for i, s := range sections {
		sh.Nav = append(sh.Nav, shellNav{Key: "nav." + s.key, Href: s.href, Icon: s.icon, On: i == si})
	}
	if si >= 0 {
		for j, t := range sections[si].tabs {
			sh.Tabs = append(sh.Tabs, shellTab{Key: t.key, Href: t.href, On: j == ti})
		}
	}
	return sh
}
