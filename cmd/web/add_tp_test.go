package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/journal"
)

// Entry 1628 units (SUI, 0 lot decimals), live position 1628 unless said.
func TestAddTPSize(t *testing.T) {
	cases := []struct {
		name             string
		entry, live, pct float64
		lot              int
		qty              float64
		fault            string
	}{
		{"30% of 1628 floors to 488", 1628, 1628, 30, 0, 488, ""}, // 488.4 → 488
		{"after a 50% leg, 30% is still of the original", 1628, 814, 30, 0, 488, ""},
		{"one that takes all that is left", 1628, 488, 30, 0, 488, "atp.err.wholePos"},
		{"0%", 1628, 1628, 0, 0, 0, "atp.err.pct"},
		{"100%", 1628, 1628, 100, 0, 0, "atp.err.pct"},
		{"unknown entry size", 0, 1628, 30, 0, 0, "atp.err.noEntryQty"},
		{"rounds to nothing", 3, 3, 10, 0, 0, "atp.err.tooSmall"}, // 0.3 → 0
		{"BTC-style lot", 0.0153, 0.0153, 30, 4, 0.0045, ""},      // 0.00459 → 0.0045
	}
	for _, c := range cases {
		qty, fault := addTPSize(c.entry, c.live, c.pct, c.lot)
		if !sameQty(qty, c.qty) || fault != c.fault {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, qty, fault, c.qty, c.fault)
		}
	}
}

func sameQty(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// The card lists each resting take-profit with its size and a cancel ×, and
// offers "add take-profit".
func TestAddTPCardRenders(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	tr := journal.Trade{ID: 95, Symbol: "SUI", Side: "long", Entry: 0.92, Stop: 0.909, TP2: 0.948, OpenedAt: at, FilledAt: at,
		Exits: []journal.Exit{{Price: 0.935, Pct: 30, OrderID: "x"}}}
	card := openTradeCard{Trade: tr, MarkPrice: 0.93, Track: newRTrack(tr, 0.93), LiveStopKnown: true,
		LiveStops: []liveStop{{ID: "S", Price: 0.915}}, LiveTPs: []liveStop{{ID: "B", Price: 0.948, Qty: 1140}}}
	for _, lang := range langs {
		var buf bytes.Buffer
		if err := pages[lang].ExecuteTemplate(&buf, "positionsBody", gin.H{"OpenTrades": []openTradeCard{card}}); err != nil {
			t.Fatal(err)
		}
		body := buf.String()
		for _, want := range []string{`js-add-tp`, `class="pos-x js-cancel-tp" data-id="95" data-order="B"`, `×1140`, `30% @ 0.935`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: card lacks %q", lang, want)
			}
		}
	}
}
