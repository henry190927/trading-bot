package main

import (
	"bytes"
	"html"
	"html/template"
	"strings"
	"testing"
)

// The /ops position row for a trail fire, executed (not just parsed) — the
// TP cell, the trailed tag and the own-risk tag only exist at execute time.
// Fixture rows, not real data: what a real closed htf-trend fire looks like on
// this page can only be checked on the VPS after the first one settles.
func TestAtFireRowTrail(t *testing.T) {
	tpl := template.Must(template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html"))
	render := func(r atFireRow) string {
		t.Helper()
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "atFireRow", r); err != nil {
			t.Fatalf("atFireRow failed to execute: %v", err)
		}
		return html.UnescapeString(buf.String())
	}

	open := render(atFireRow{Symbol: "BTC", Strategy: "htf-trend", Side: "long", Status: "open", Open: true,
		Entry: 64000, Stop: 61000, Trail: true, CurStop: 62500, Trailed: true, UnrealR: 0.8, RiskPct: 0.25})
	for _, want := range []string{"live.trail", "@62,500", "+0.80", "0.25%"} {
		if !strings.Contains(open, want) {
			t.Errorf("open trail row missing %q:\n%s", want, open)
		}
	}
	if strings.Contains(open, "at.trailed\"") || strings.Contains(open, ">at.trailed<") {
		t.Errorf("an OPEN position must not carry the trailed-exit tag:\n%s", open)
	}

	closed := render(atFireRow{Symbol: "BTC", Strategy: "htf-trend", Side: "long", Status: "stop", Resolved: true,
		Entry: 64000, Stop: 61000, Trail: true, Trailed: true, NetR: 1.7})
	for _, want := range []string{"live.trail", "at.trailed", "+1.70"} {
		if !strings.Contains(closed, want) {
			t.Errorf("closed trail row missing %q:\n%s", want, closed)
		}
	}
	if strings.Contains(closed, "@") {
		t.Errorf("a closed trail row must not show a current stop:\n%s", closed)
	}

	// A fixed bracket renders its TP as before and no trail markup.
	fixed := render(atFireRow{Symbol: "ETH", Strategy: "engine", Side: "short", Status: "tp", Resolved: true, Entry: 3000, Stop: 3030, TP: 2940, NetR: 2})
	if !strings.Contains(fixed, "2,940") || strings.Contains(fixed, "live.trail") || strings.Contains(fixed, "at.trailed") {
		t.Errorf("fixed-bracket row changed:\n%s", fixed)
	}
}
