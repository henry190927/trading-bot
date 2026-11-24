package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
	"github.com/henry190927/trading-bot/market"
	"github.com/henry190927/trading-bot/venue"
)

// The safety property of the symbol catalog (docs/symbol_catalog_design.md
// §3): a symbol that is in the catalog but not on the curated list can be
// LOOKED AT and never TRADED. Every handler that takes a symbol from the
// request and writes the journal, the setups log or an exchange order is
// listed here and hit with such a symbol. If one of them is ever switched to
// resolveViewable, this fails.
//
// Handlers keyed by trade id (move-stop, move-tp, add-tp, cancel-tp, close,
// unwind) read the symbol from a journal row; the journal rows are covered
// here through /journal/open and /journal/:id/edit.

type catalogVenue struct{ list []venue.Instrument }

func (v catalogVenue) Venue() string { return "bingx" }
func (v catalogVenue) Instruments(context.Context) ([]venue.Instrument, error) {
	return v.list, nil
}
func (v catalogVenue) Klines(context.Context, string, market.Timeframe, int) ([]market.Candle, error) {
	return nil, nil
}
func (v catalogVenue) Timeframes() []market.Timeframe { return nil }

// testCatalog holds TSLA (catalog only) and BTC (on the list).
func testCatalog(t *testing.T) *venue.Registry {
	t.Helper()
	reg := venue.NewRegistry("", nil)
	reg.Register(catalogVenue{list: []venue.Instrument{
		{Venue: "bingx", Code: "NCSKTSLA2USD-USDT", Display: "TSLA", Class: venue.ClassUSStock, Quote: "USDT"},
		{Venue: "bingx", Code: "BTC-USDT", Display: "BTC", Class: venue.ClassCrypto, Quote: "USDT", Tradable: true},
	}})
	if err := reg.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestResolveViewable(t *testing.T) {
	s := &server{venues: testCatalog(t)}
	cases := []struct {
		in       string
		key      string
		display  string
		tradable bool
	}{
		{"BTC", "BTC", "BTC", true},
		{"btc", "BTC", "BTC", true},
		{"bingx:BTC-USDT", "BTC", "BTC", true}, // a list symbol asked for by ID gets its short Key
		{"BTC-USDT", "BTC", "BTC", true},
		{"bingx:NCSKTSLA2USD-USDT", "bingx:NCSKTSLA2USD-USDT", "TSLA", false},
		{"BINGX:NCSKTSLA2USD-USDT", "bingx:NCSKTSLA2USD-USDT", "TSLA", false},
		{"NCSKTSLA2USD-USDT", "bingx:NCSKTSLA2USD-USDT", "TSLA", false},
	}
	for _, c := range cases {
		v, err := s.resolveViewable(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if v.Key != c.key || v.Display != c.display || v.Tradable != c.tradable {
			t.Errorf("%q: got key=%q display=%q tradable=%v", c.in, v.Key, v.Display, v.Tradable)
		}
	}
	if v, _ := s.resolveViewable("NCSKTSLA2USD-USDT"); v.Sym != "NCSKTSLA2USD-USDT" {
		t.Errorf("TSLA fetch symbol = %q", v.Sym)
	}
	for _, bad := range []string{"TSLA", "bingx:NOPE-USDT", "okx:BTC-USDT", ""} {
		if _, err := s.resolveViewable(bad); err == nil {
			t.Errorf("%q resolved", bad)
		}
	}
	// No catalog: exactly the list, as before.
	bare := &server{}
	if _, err := bare.resolveViewable("bingx:NCSKTSLA2USD-USDT"); err == nil {
		t.Error("catalog symbol resolved without a catalog")
	}
	if v, err := bare.resolveViewable("ETH"); err != nil || v.Key != "ETH" {
		t.Errorf("ETH without catalog: %+v %v", v, err)
	}
}

func TestTradingPathsRejectCatalogOnlySymbol(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "journal.csv")
	t.Setenv("JOURNAL_PATH", jpath)
	t.Setenv("SETUPS_PATH", filepath.Join(dir, "setups.jsonl"))
	t.Setenv("BINGX_MAX_MARGIN_USDT", "50")
	gin.SetMode(gin.TestMode)

	// A BingX stand-in that records every request. Reads fail (503); any
	// request at all to a trade endpoint is a failure of the test.
	var mu sync.Mutex
	var hits []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		http.Error(w, "stub", http.StatusServiceUnavailable)
	}))
	t.Cleanup(stub.Close)
	client := &bingx.Client{APIKey: "k", APISecret: "s", Host: stub.URL, HTTP: stub.Client(), DryRun: true}

	// One existing (list) row, for the edit path.
	row := journal.Trade{ID: 1, OpenedAt: time.Now().Add(-time.Hour), Symbol: "BTC", Side: "long", Entry: 100, Stop: 90, TF: "1h"}
	if err := journal.WriteAll(jpath, []journal.Trade{row}); err != nil {
		t.Fatal(err)
	}
	rows, _ := journal.ReadAll("")
	ver := journal.Version(rows[0])

	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dicts: dicts, pages: pages, client: client, venues: testCatalog(t)}

	r := gin.New()
	r.POST("/journal/open", s.handleJournalOpen)
	r.POST("/journal/:id/edit", s.handleJournalEditPost)
	r.POST("/api/chart/journal-open", s.handleAPIChartJournalOpen)
	r.POST("/api/chart/validate", s.handleAPIChartValidate)
	r.POST("/api/chart/setup", s.handleAPIChartSetupRecord)
	r.POST("/scan/record", s.handleScanRecord)
	r.POST("/ops/protect", s.handleOpsProtect)
	r.POST("/ops/entry", s.handleOpsEntry)
	r.POST("/ops/cancel", s.handleOpsCancel)

	trade := url.Values{
		"side": {"long"}, "entry": {"100"}, "stop": {"90"}, "tp2": {"130"}, "tp": {"130"},
		"tf": {"1h"}, "margin_usdt": {"10"}, "leverage": {"5"}, "open_position": {"on"},
		"confirm": {"1"}, "order_id": {"123"}, "anchor": {"test"},
	}
	form := func(sym string, extra url.Values) url.Values {
		f := url.Values{}
		for k, v := range trade {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		f.Set("symbol", sym)
		return f
	}

	for _, sym := range []string{"bingx:NCSKTSLA2USD-USDT", "NCSKTSLA2USD-USDT"} {
		// The premise: the symbol IS viewable. Otherwise a rejection below
		// would prove nothing.
		if _, err := s.resolveViewable(sym); err != nil {
			t.Fatalf("premise: %s not viewable: %v", sym, err)
		}
		reqs := []struct {
			path, ctype, body string
		}{
			{"/journal/open", "form", form(sym, nil).Encode()},
			{"/journal/1/edit", "form", form(sym, url.Values{"ver": {ver}}).Encode()},
			{"/api/chart/journal-open", "form", form(sym, nil).Encode()},
			{"/api/chart/validate", "form", form(sym, nil).Encode()},
			{"/api/chart/setup", "json", `{"symbol":"` + sym + `","tf":"1h","dir":"long","entry":100,"stop":90,"target":130}`},
			{"/scan/record", "form", form(sym, nil).Encode()},
			{"/ops/protect", "form", form(sym, nil).Encode()},
			{"/ops/entry", "form", form(sym, nil).Encode()},
			{"/ops/cancel", "form", form(sym, nil).Encode()},
		}
		for _, rq := range reqs {
			req := httptest.NewRequestWithContext(t.Context(), "POST", rq.path, strings.NewReader(rq.body))
			if rq.ctype == "json" {
				req.Header.Set("Content-Type", "application/json")
			} else {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			body := w.Body.String()
			// Rejection shows up as a 4xx JSON error, a form re-rendered with
			// the error, or /ops/scan?err=1 — so the status alone is not the
			// check. It must be the RESOLVER that refused: with this stub
			// every exchange read fails, so a handler that resolved the
			// symbol and failed later would also write and send nothing, and
			// "nothing written" alone would pass it. /scan/record carries no
			// message; the setups-log check below covers it.
			if rq.path != "/scan/record" && !strings.Contains(body, "unknown symbol") {
				t.Errorf("%s %s: not refused by the symbol resolver: %d %.300s", rq.path, sym, w.Code, body)
			}
			if w.Code/100 == 2 && strings.Contains(body, `"ok":true`) {
				t.Errorf("%s %s: accepted: %d %s", rq.path, sym, w.Code, body)
			}
			if loc := w.Header().Get("Location"); loc != "" && !strings.Contains(loc, "err=") {
				t.Errorf("%s %s: redirected to %s", rq.path, sym, loc)
			}
		}
	}

	rows, err = journal.ReadAll("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Symbol != "BTC" {
		t.Errorf("journal changed: %+v", rows)
	}
	if su, _ := readSetups(); len(su) != 0 {
		t.Errorf("setups written: %+v", su)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range hits {
		if strings.Contains(h, "/trade/") || strings.Contains(h, "leverage") {
			t.Errorf("exchange write attempted: %s", h)
		}
	}
	if len(hits) != 0 {
		t.Logf("exchange reads made (fine, but unexpected before symbol resolution): %v", hits)
	}
}
