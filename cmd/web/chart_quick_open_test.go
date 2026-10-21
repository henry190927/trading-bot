package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

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
