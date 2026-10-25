package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/journal"
)

// Both new pieces of markup render to the end of the page in both languages
// — a template error part-way through still returns 200 with a truncated
// body, so "it rendered" has to mean "</html> is there". PREVIEW_DIR, when
// set, keeps the HTML for a visual check.
func TestPositionGoneMarkupRenders(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 0, 59, 48, 0, time.UTC)
	tr := journal.Trade{ID: 93, Symbol: "SUI", Side: "long", TF: "15m", Entry: 0.920912, Stop: 0.909, TP2: 0.948,
		OpenedAt: at, FilledAt: at.Add(38 * time.Minute), Leverage: 100}
	ec := exchangeClose{ExitPrice: 0.9249308968058968, Qty: 1628, EntryQty: 1628, PnL: 6.5623, Fees: -1.052739,
		Fills: 1, OrderIDs: []string{"2114230036026585088"}, Outcome: "manual", ClosedAt: at.Add(12*time.Hour + 38*time.Minute)}
	for _, lang := range langs {
		var buf bytes.Buffer
		err := pages[lang].ExecuteTemplate(&buf, "journal_close.html", gin.H{
			"Shell": buildShell("/journal/93/close", "/journal/93/close?from=exchange", lang),
			"Trade": tr, "Ver": journal.Version(tr), "Outcome": "manual", "ExitPrice": "0.9249309",
			"Notes": "from exchange", "Now": "2026-10-25T14:00",
			"Exchange": gin.H{"Close": ec, "Short": false, "Clipped": false},
		})
		body := buf.String()
		if err != nil || !strings.Contains(body, "</html>") || !strings.Contains(body, `class="ui-banner ok jc-ex"`) ||
			!strings.Contains(body, `<option value="manual" selected>`) || !strings.Contains(body, "1628") {
			t.Fatalf("%s close form: err=%v, complete=%v", lang, err, strings.Contains(body, "</html>"))
		}
		keep(t, lang+"_close.html", body)

		buf.Reset()
		card := openTradeCard{Trade: tr, MarkPrice: 0.93, PositionGone: true, Track: newRTrack(tr, 0.93)}
		if err := pages[lang].ExecuteTemplate(&buf, "positionsBody", gin.H{"OpenTrades": []openTradeCard{card}}); err != nil {
			t.Fatalf("%s positions: %v", lang, err)
		}
		if body := buf.String(); !strings.Contains(body, `href="/journal/93/close?from=exchange"`) || !strings.Contains(body, "pos-gone") {
			t.Fatalf("%s positions card has no close-from-exchange button", lang)
		}
		keep(t, lang+"_card.html", buf.String())
	}
}

func keep(t *testing.T, name, body string) {
	if dir := os.Getenv("PREVIEW_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
