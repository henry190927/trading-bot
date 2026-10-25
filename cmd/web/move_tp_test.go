package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/journal"
)

func TestPickTPs(t *testing.T) {
	one := []liveStop{{ID: "A", Price: 0.948}}
	two := []liveStop{{ID: "A", Price: 0.935}, {ID: "B", Price: 0.948}}
	cases := []struct {
		name    string
		current []liveStop
		replace string
		want    string // fmt of the old list, or "refused"
	}{
		{"nothing resting: place only", nil, "", "[]"},
		{"one resting: that one", one, "", "[{A 0.948}]"},
		{"two resting, none named: refuse to guess", two, "", "refused"},
		{"two resting, B named", two, "B", "[{B 0.948}]"},
		{"named order is not resting", one, "Z", "refused"},
	}
	for _, c := range cases {
		old, ok := pickTPs(c.current, c.replace)
		got := "refused"
		if ok {
			got = fmt.Sprint(old)
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// The card shows the exchange's take-profits and carries them on the button
// as id|price pairs the modal reads.
func TestMoveTPButtonRenders(t *testing.T) {
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	tr := journal.Trade{ID: 94, Symbol: "SUI", Side: "long", Entry: 0.92, Stop: 0.909, TP2: 0.948, OpenedAt: at, FilledAt: at}
	card := openTradeCard{Trade: tr, MarkPrice: 0.93, Track: newRTrack(tr, 0.93), LiveStopKnown: true,
		LiveStops: []liveStop{{ID: "S", Price: 0.915}}, LiveTPs: []liveStop{{ID: "A", Price: 0.935}, {ID: "B", Price: 0.948}}}
	for _, lang := range langs {
		var buf bytes.Buffer
		if err := pages[lang].ExecuteTemplate(&buf, "positionsBody", gin.H{"OpenTrades": []openTradeCard{card}}); err != nil {
			t.Fatal(err)
		}
		body := buf.String()
		if !strings.Contains(body, `class="ui-btn ghost sm js-move-tp"`) || !strings.Contains(body, `data-tps="A|0.935;B|0.948"`) || !strings.Contains(body, `data-planned="0.948"`) {
			t.Errorf("%s: move-tp button missing or malformed", lang)
		}
	}
}
