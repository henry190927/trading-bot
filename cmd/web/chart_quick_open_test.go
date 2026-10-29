package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/henry190927/trading-bot/bingx"
	"github.com/henry190927/trading-bot/journal"
)

// quickOpen posts form to /api/chart/journal-open against a server whose BingX
// client is DryRun (writes never leave the process) and whose reads hit a stub
// that always fails — so equity and exposure are unknown, which is the state
// the risk gate degrades to rather than blocks on.
func quickOpen(t *testing.T, client *bingx.Client, form url.Values) (int, map[string]any, []journal.Trade) {
	t.Helper()
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	t.Setenv("BINGX_MAX_MARGIN_USDT", "50")
	gin.SetMode(gin.TestMode)
	dicts, _ := loadDicts(i18nFS)
	s := &server{dicts: dicts, client: client}
	r := gin.New()
	r.POST("/api/chart/journal-open", s.handleAPIChartJournalOpen)

	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/chart/journal-open", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	trades, err := journal.ReadAll("")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	return w.Code, body, trades
}

func dryRunClient(t *testing.T) *bingx.Client {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stub", http.StatusServiceUnavailable)
	}))
	t.Cleanup(stub.Close)
	return &bingx.Client{APIKey: "k", APISecret: "s", Host: stub.URL, HTTP: stub.Client(), DryRun: true}
}

func baseForm() url.Values {
	return url.Values{
		"symbol": {"BTC"}, "side": {"long"}, "entry": {"80000"},
		"stop": {"79000"}, "tp1": {"81000"}, "tp2": {"82000"},
		"margin_usdt": {"20"}, "leverage": {"10"},
	}
}

func TestQuickOpenJournalOnlyPlacesNothing(t *testing.T) {
	code, body, trades := quickOpen(t, dryRunClient(t), baseForm())
	if code != http.StatusOK || body["orders"] != nil {
		t.Fatalf("code %d body %v", code, body)
	}
	if len(trades) != 1 || trades[0].EntryOrderID != "" || trades[0].StopAuto {
		t.Fatalf("journal-only row carried order state: %+v", trades)
	}
}

func TestQuickOpenRefusesBeforeWriting(t *testing.T) {
	cases := map[string]func(url.Values){
		"no stop":         func(f url.Values) { f.Del("stop") },
		"no margin":       func(f url.Values) { f.Del("margin_usdt") },
		"no leverage":     func(f url.Values) { f.Del("leverage") },
		"margin over cap": func(f url.Values) { f.Set("margin_usdt", "60") },
		"stop above long": func(f url.Values) { f.Set("stop", "80500") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := baseForm()
			f.Set("open_position", "on")
			mutate(f)
			code, body, trades := quickOpen(t, dryRunClient(t), f)
			if code != http.StatusBadRequest {
				t.Fatalf("code %d body %v", code, body)
			}
			if len(trades) != 0 {
				t.Fatalf("a refused request wrote %d row(s)", len(trades))
			}
		})
	}
}

func TestQuickOpenPlacesEntryWithBundledStop(t *testing.T) {
	f := baseForm()
	f.Set("open_position", "on")
	code, body, trades := quickOpen(t, dryRunClient(t), f)
	if code != http.StatusOK {
		t.Fatalf("code %d body %v", code, body)
	}
	orders, _ := body["orders"].([]any)
	if len(orders) == 0 || orders[0].(map[string]any)["status"] != "ok" {
		t.Fatalf("entry leg not ok: %v", body)
	}
	tr := trades[0]
	if !strings.HasPrefix(tr.EntryOrderID, "DRY-RUN") {
		t.Errorf("entry order id = %q", tr.EntryOrderID)
	}
	if tr.StopOrderID != "ATTACHED:"+tr.EntryOrderID || !tr.StopAuto {
		t.Errorf("stop not bundled: StopOrderID=%q StopAuto=%v", tr.StopOrderID, tr.StopAuto)
	}
	if tr.TP2OrderID != "ATTACHED:"+tr.EntryOrderID {
		t.Errorf("tp2 not bundled: %q", tr.TP2OrderID)
	}
	if !tr.TP1Auto {
		t.Error("TP1Auto not persisted, the sweep would never place the partial")
	}
	sm, _ := body["summary"].(map[string]any)
	if sm == nil || sm["qty"] != 0.0025 || sm["stopBundled"] != true || sm["tp2Bundled"] != true || sm["tp1Pending"] != true || sm["orderId"] != tr.EntryOrderID {
		t.Errorf("summary = %v", sm)
	}
}

// A refused entry must not leave StopAuto behind: bracket.Decide would honour
// it against any later position on the same symbol and side.
func TestQuickOpenFailedEntryClearsAutoFlags(t *testing.T) {
	f := baseForm()
	f.Set("open_position", "on")
	code, body, trades := quickOpen(t, &bingx.Client{}, f) // no key: entry is skipped
	if code != http.StatusOK {
		t.Fatalf("code %d body %v", code, body)
	}
	orders, _ := body["orders"].([]any)
	if len(orders) != 1 || orders[0].(map[string]any)["status"] == "ok" {
		t.Fatalf("orders = %v", orders)
	}
	if tr := trades[0]; tr.StopAuto || tr.TP1Auto || tr.TP2Auto || tr.EntryOrderID != "" {
		t.Errorf("failed entry left intent behind: %+v", tr)
	}
}

// Closing #92-shaped: TP1 50% recorded at 82, remainder stopped at 79.2.
// Hand-traced: exit = 0.5*82 + 0.5*79.2 = 80.6; R = (80.6-80.424)/1.224.
func TestClosePostBlendsTP1Leg(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 21, 16, 2, 0, 0, time.UTC)
	if err := journal.WriteAll("", []journal.Trade{{
		ID: 92, OpenedAt: at, Symbol: "HYPE", Side: "long", Entry: 80.424, Stop: 79.2, TP1: 82,
		FilledAt: at, TP1FillPrice: 82, TP1FillPct: 50, TP1FilledAt: at,
	}}); err != nil {
		t.Fatal(err)
	}
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dicts: dicts, pages: pages}
	r := gin.New()
	r.GET("/journal/:id/close", s.handleJournalCloseForm)
	r.POST("/journal/:id/close", s.handleJournalClosePost)

	// The form shows the leg and asks for the remainder only.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/journal/92/close", nil))
	if body := w.Body.String(); !strings.Contains(body, `name="blend_tp1"`) || strings.Contains(body, "partials-rows") {
		t.Fatalf("close form: blend flag present=%v, manual partials present=%v",
			strings.Contains(body, `name="blend_tp1"`), strings.Contains(body, "partials-rows"))
	}

	cur, _ := journal.ReadAll("")
	form := url.Values{"outcome": {"stop"}, "exit_price": {"79.2"}, "blend_tp1": {"on"}, "closed_at": {"2026-10-22 01:00"}, "ver": {journal.Version(cur[0])}}
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/journal/92/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("close POST = %d: %s", w.Code, w.Body.String())
	}
	got, _ := journal.ReadAll("")
	g := got[0]
	if math.Abs(g.ExitPrice-80.6) > 1e-9 {
		t.Errorf("exit_price = %v, want the blend 80.6", g.ExitPrice)
	}
	// r_realized is written with 4 decimals.
	if want := (80.6 - 80.424) / 1.224; math.Abs(g.RRealized-want) > 5e-5 {
		t.Errorf("R = %v, want %v", g.RRealized, want)
	}
	if !strings.Contains(g.CloseNotes, "82") || !strings.Contains(g.CloseNotes, "79.2") {
		t.Errorf("notes do not spell out the legs: %q", g.CloseNotes)
	}
	// R survives a recompute from the stored exit (what every edit does).
	if rr := journal.RealizedR(g, g.ExitPrice); math.Abs(rr-g.RRealized) > 5e-5 {
		t.Errorf("recompute drifted: %v vs %v", rr, g.RRealized)
	}
}

func TestEditPostTP1Leg(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 21, 16, 9, 50, 0, time.UTC)
	seed := func() {
		if err := journal.WriteAll("", []journal.Trade{{
			ID: 92, OpenedAt: at, AnalyzedAt: at, Symbol: "HYPE", Side: "long", TF: "1h", Entry: 80.424, Stop: 79.2, TP1: 82, TP2: 82.5,
			FilledAt: at, TP1OrderID: "T", TP1FillPrice: 82, TP1FillPct: 50, TP1FilledAt: at,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dicts: dicts, pages: pages}
	r := gin.New()
	r.GET("/journal/:id/edit", s.handleJournalEditForm)
	r.POST("/journal/:id/edit", s.handleJournalEditPost)
	post := func(extra url.Values) (int, journal.Trade) {
		f := url.Values{"symbol": {"HYPE"}, "side": {"long"}, "tf": {"1h"}, "entry": {"80.424"}, "stop": {"79.2"},
			"tp1": {"82"}, "tp2": {"82.5"}, "opened_at": {"2026-10-22T00:09"}, "filled_at": {"2026-10-22T00:09"}}
		cur, _ := journal.ReadAll("")
		f.Set("ver", journal.Version(cur[0]))
		for k, v := range extra {
			f[k] = v
		}
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/journal/92/edit", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		got, _ := journal.ReadAll("")
		return w.Code, got[0]
	}

	seed()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/journal/92/edit", nil))
	if body := w.Body.String(); !strings.Contains(body, `name="tp1_fill_price"`) || strings.Contains(body, "partials-rows") {
		t.Fatalf("edit form: leg fields=%v, manual partials=%v", strings.Contains(body, `name="tp1_fill_price"`), strings.Contains(body, "partials-rows"))
	}
	if code, g := post(nil); code != http.StatusSeeOther || g.TP1FillPrice != 82 || g.TP1FillPct != 50 {
		t.Errorf("save without leg fields: %d, leg %v/%v — must be kept", code, g.TP1FillPrice, g.TP1FillPct)
	}
	if _, g := post(url.Values{"tp1_fill_price": {"82.1"}, "tp1_fill_pct": {"40"}}); g.TP1FillPrice != 82.1 || g.TP1FillPct != 40 || !g.TP1FilledAt.Equal(at) {
		t.Errorf("correction = %+v", g)
	}
	if code, g := post(url.Values{"tp1_fill_price": {"82"}, "tp1_fill_pct": {"150"}}); code == http.StatusSeeOther || g.TP1FillPct != 40 {
		t.Errorf("pct 150 accepted: %d %v", code, g.TP1FillPct)
	}
	if _, g := post(url.Values{"tp1_fill_price": {""}, "tp1_fill_pct": {""}}); g.HasTP1Leg() || !g.TP1FilledAt.IsZero() {
		t.Errorf("clearing left %+v", g)
	}
}

// #92, 2026-10-22: an edit page opened before the close was saved after it.
// Edit reads a blank closed_at as "reopen", so the stale tab silently undid
// the close. With the row version, that save is refused and the close stays.
func TestStaleEditCannotReopenAClose(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 21, 13, 35, 0, 0, time.UTC)
	if err := journal.WriteAll("", []journal.Trade{{
		ID: 92, OpenedAt: at, AnalyzedAt: at, Symbol: "HYPE", Side: "long", TF: "1h", Entry: 80.424, Stop: 79.2, TP1: 82,
		FilledAt: at, TP1FillPrice: 82, TP1FillPct: 50, TP1FilledAt: at,
	}}); err != nil {
		t.Fatal(err)
	}
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dicts: dicts, pages: pages}
	r := gin.New()
	r.POST("/journal/:id/edit", s.handleJournalEditPost)
	r.POST("/journal/:id/close", s.handleJournalClosePost)
	post := func(path string, f url.Values) int {
		req := httptest.NewRequestWithContext(t.Context(), "POST", path, strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	before, _ := journal.ReadAll("")
	staleVer := journal.Version(before[0]) // the edit tab is opened now

	if code := post("/journal/92/close", url.Values{"outcome": {"stop"}, "exit_price": {"80.0753"}, "blend_tp1": {"on"}, "ver": {staleVer}}); code != http.StatusSeeOther {
		t.Fatalf("close = %d", code)
	}
	// The old edit tab is saved: blank closed_at, as it was rendered.
	code := post("/journal/92/edit", url.Values{"symbol": {"HYPE"}, "side": {"long"}, "tf": {"1h"}, "entry": {"80.424"}, "stop": {"79.2"},
		"tp1": {"82"}, "opened_at": {"2026-10-21T21:35"}, "ver": {staleVer}})
	if code != http.StatusConflict {
		t.Errorf("stale edit = %d, want 409", code)
	}
	got, _ := journal.ReadAll("")
	if got[0].IsOpen() || got[0].Outcome != "stop" {
		t.Errorf("the close was undone: %+v", got[0])
	}
}

// The edit page is not a close page: a result/exit typed into an open trade
// with no close time used to vanish silently, and closing a TP1-leg trade
// here would take the exit literally.
func TestEditRefusesSilentCloseAttempts(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 21, 13, 35, 0, 0, time.UTC)
	if err := journal.WriteAll("", []journal.Trade{{
		ID: 92, OpenedAt: at, AnalyzedAt: at, Symbol: "HYPE", Side: "long", TF: "1h", Entry: 80.424, Stop: 79.2, TP1: 82,
		FilledAt: at, TP1FillPrice: 82, TP1FillPct: 50, TP1FilledAt: at,
	}}); err != nil {
		t.Fatal(err)
	}
	dicts, _ := loadDicts(i18nFS)
	pages, err := parsePages(assets, dicts)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dicts: dicts, pages: pages}
	r := gin.New()
	r.POST("/journal/:id/edit", s.handleJournalEditPost)
	for name, extra := range map[string]url.Values{
		"result without close time": {"outcome": {"stop"}, "exit_price": {"80.0753"}},
		"close with a TP1 leg":      {"outcome": {"stop"}, "exit_price": {"80.0753"}, "closed_at": {"2026-10-22T09:56"}},
	} {
		cur, _ := journal.ReadAll("")
		f := url.Values{"symbol": {"HYPE"}, "side": {"long"}, "tf": {"1h"}, "entry": {"80.424"}, "stop": {"79.2"},
			"tp1": {"82"}, "opened_at": {"2026-10-21T21:35"}, "filled_at": {"2026-10-21T21:35"}, "ver": {journal.Version(cur[0])}}
		for k, v := range extra {
			f[k] = v
		}
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/journal/92/edit", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusSeeOther {
			t.Errorf("%s: saved (303), want the form back with an error", name)
		}
		if after, _ := journal.ReadAll(""); journal.Version(after[0]) != journal.Version(cur[0]) {
			t.Errorf("%s: row changed: %+v", name, after[0])
		}
	}
}
