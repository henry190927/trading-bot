package main

import (
	"html/template"
	"strings"
	"testing"
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
