package main

import (
	"bytes"
	"html"
	"html/template"
	"strings"
	"testing"

	"github.com/henry190927/trading-bot/autotrade"
)

// A FuncMap entry whose name collides with a text/template builtin replaces it
// for EVERY template, and the failure mode is invisible: Gin has already
// written the 200 header when Execute fails, so the page truncates at the
// offending node and the browser shows a half-rendered document.
//
// This bit on 2026-09-05. A custom `or func(a, b string) string` (string
// defaults, `{{or .TF "—"}}`) shadowed the builtin, so
// verify_exchange.html's `{{if or (eq .Type "STOP_MARKET") (eq .Type "STOP")}}`
// passed two bools into it — with two live resting orders on the exchange and
// no way to confirm them.
func TestTemplateFuncsDoNotShadowBuiltins(t *testing.T) {
	// The builtins a template author will reach for without thinking. Any of
	// these in our FuncMap is a landmine, not a convenience.
	builtins := []string{
		"and", "or", "not", "eq", "ne", "lt", "le", "gt", "ge",
		"index", "slice", "len", "print", "printf", "println",
		"call", "html", "js", "urlquery",
	}
	fns := templateFuncs()
	for _, name := range builtins {
		if _, shadowed := fns[name]; shadowed {
			t.Errorf("templateFuncs() overrides the builtin %q — pick a distinct name (e.g. %q)", name, "firstNonEmpty")
		}
	}
}

// The two shapes the deleted custom `or` was serving, proven to work on the
// builtin, so nobody re-adds it "because the default doesn't do this".
func TestBuiltinOrCoversBothUses(t *testing.T) {
	run := func(src string, data any) (string, error) {
		tpl, err := template.New("x").Funcs(templateFuncs()).Parse(src)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		err = tpl.Execute(&sb, data)
		return sb.String(), err
	}

	for _, tc := range []struct {
		name, src string
		data      any
		want      string
	}{
		{"string default, empty", `{{or .TF "—"}}`, map[string]any{"TF": ""}, "—"},
		{"string default, present", `{{or .TF "—"}}`, map[string]any{"TF": "1h"}, "1h"},
		{"boolean or, true arm", `{{if or (eq .T "STOP_MARKET") (eq .T "STOP")}}RED{{else}}BLUE{{end}}`, map[string]any{"T": "STOP"}, "RED"},
		{"boolean or, false arm", `{{if or (eq .T "STOP_MARKET") (eq .T "STOP")}}RED{{else}}BLUE{{end}}`, map[string]any{"T": "LIMIT"}, "BLUE"},
		{"strings in boolean position", `{{if or (or .A .B) (or .C .D)}}HAS{{else}}NONE{{end}}`, map[string]any{"A": "", "B": "x", "C": "", "D": ""}, "HAS"},
		{"all empty", `{{if or (or .A .B) (or .C .D)}}HAS{{else}}NONE{{end}}`, map[string]any{"A": "", "B": "", "C": "", "D": ""}, "NONE"},
	} {
		got, err := run(tc.src, tc.data)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every shipped template must at least PARSE and execute against a plausible
// dot without erroring on the funcs — the cheapest guard against another
// render-time-only failure. Uses the real FuncMap.
func TestVerifyExchangeOrderTypeCellRenders(t *testing.T) {
	src := `{{range .Orders}}{{if or (eq .Type "STOP_MARKET") (eq .Type "STOP")}}R{{else}}B{{end}}{{end}}`
	tpl := template.Must(template.New("x").Funcs(templateFuncs()).Parse(src))
	var sb strings.Builder
	err := tpl.Execute(&sb, map[string]any{
		"Orders": []struct{ Type string }{{"LIMIT"}, {"STOP_MARKET"}, {"TAKE_PROFIT_MARKET"}, {"STOP"}},
	})
	if err != nil {
		t.Fatalf("the cell that truncated /ops/verify still fails: %v", err)
	}
	if sb.String() != "BRBR" {
		t.Errorf("got %q, want %q", sb.String(), "BRBR")
	}
}

// Every template must PARSE with the real func map, exactly as main() does it.
//
// main() wraps this in template.Must, so a template that references an
// undefined function or has an unbalanced {{if}}/{{end}} does not fail a
// build and does not fail a test — it panics the service on startup, i.e.
// during `make deploy-web`, after the binary is already in place. There was no
// coverage for that until 2026-09-10, when adding a {{if .SessionWarn}} block
// to validate_result.html meant the only way to find out was to deploy.
func TestTemplatesParse(t *testing.T) {
	tpl, err := template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html")
	if err != nil {
		t.Fatalf("templates do not parse — this would panic main() on startup: %v", err)
	}
	// Guard against the glob silently matching nothing, which would make the
	// check above pass while proving nothing.
	for _, name := range []string{
		"validate_result.html", "autotrade.html", "journal_new.html",
	} {
		if tpl.Lookup(name) == nil {
			t.Errorf("%s not parsed — did the embed glob or the filename change?", name)
		}
	}
}

// TestBdTableExecutes renders the breakdown row template with real Groups.
// TestTemplatesParse only proves the templates PARSE; {{.RPerTrade}} is a
// method call resolved at EXECUTE time, as is every field promoted through
// Group's embedded Summary. A typo in either renders a blank cell (or errors)
// on a page that parsed cleanly.
func TestBdTableExecutes(t *testing.T) {
	tpl := template.Must(template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html"))
	groups := []autotrade.Group{
		// 2 tp / 1 stop, +2.5R over 3 settled → 67%, +0.83 per trade.
		{Key: "engine", Summary: autotrade.Summary{Total: 3, Filled: 3, TP: 2, Stop: 1, NetR: 2.5, WinRate: 2.0 / 3.0}},
		// Nothing settled: the win-rate and R/trade cells must be dashes, not 0%.
		{Key: "range-edge", Summary: autotrade.Summary{Total: 1, NoFill: 1}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "bdTable", groups); err != nil {
		t.Fatalf("bdTable failed to execute: %v", err)
	}
	// html/template escapes "+" to "&#43;" in a text node, so compare against
	// what the reader actually sees rather than the format string.
	out := html.UnescapeString(buf.String())
	for _, want := range []string{"engine", "range-edge", "+2.50", "67%", "+0.83"} {
		if !strings.Contains(out, want) {
			t.Errorf("bdTable output missing %q\n%s", want, out)
		}
	}
	// A group with nothing settled must print dashes, not a fabricated 0% /
	// +0.00 per trade — 0% reads as "it loses every time", which is a different
	// claim from "it has not resolved a trade yet".
	rows := strings.Split(out, "<tr>")
	var unsettled string
	for _, r := range rows {
		if strings.Contains(r, "range-edge") {
			unsettled = r
		}
	}
	if unsettled == "" {
		t.Fatalf("no range-edge row in output\n%s", out)
	}
	if strings.Contains(unsettled, "%") {
		t.Errorf("unsettled group rendered a win-rate:\n%s", unsettled)
	}
	if strings.Count(unsettled, "—") != 3 { // tp/stop, win-rate, R/trade
		t.Errorf("unsettled group should dash 3 cells, got %d:\n%s",
			strings.Count(unsettled, "—"), unsettled)
	}
}
