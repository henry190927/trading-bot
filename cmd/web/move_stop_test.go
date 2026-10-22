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

// stubExchange answers the three READS move-stop makes with #92's shape:
// a 9.3256 HYPE long whose only protective order is a 79.2 stop (id 111).
// The client is DryRun, so the place and cancel never leave the process.
func stubExchange(t *testing.T) *bingx.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user/positions"):
			_, _ = w.Write([]byte(`{"code":0,"data":[{"symbol":"HYPE-USDT","positionSide":"LONG","positionAmt":"9.3256","avgPrice":"80.424","leverage":100}]}`))
		case strings.HasSuffix(r.URL.Path, "/trade/openOrders"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"orders":[` +
				`{"symbol":"HYPE-USDT","orderId":111,"type":"STOP_MARKET","side":"SELL","positionSide":"LONG","stopPrice":"79.2","origQty":"9.3256"},` +
				`{"symbol":"HYPE-USDT","orderId":222,"type":"LIMIT","side":"SELL","positionSide":"LONG","price":"82.5","origQty":"9.3256"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/quote/premiumIndex"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"symbol":"HYPE-USDT","markPrice":"81.2","indexPrice":"81.2","lastFundingRate":"0.0001"}}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &bingx.Client{APIKey: "k", APISecret: "s", Host: srv.URL, HTTP: srv.Client(), DryRun: true}
}

func TestMoveStopEndpoint(t *testing.T) {
	t.Setenv("JOURNAL_PATH", filepath.Join(t.TempDir(), "journal.csv"))
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 21, 13, 35, 0, 0, time.UTC)
	if err := journal.WriteAll("", []journal.Trade{{
		ID: 92, OpenedAt: at, Symbol: "HYPE", Side: "long", Entry: 80.424, Stop: 79.2, TP1: 82, TP2: 82.5,
		FilledAt: at, StopOrderID: "ATTACHED:E", TP1FillPrice: 82, TP1FillPct: 50, TP1FilledAt: at,
	}}); err != nil {
		t.Fatal(err)
	}
	dicts, _ := loadDicts(i18nFS)
	s := &server{dicts: dicts, client: stubExchange(t)}
	r := gin.New()
	r.POST("/api/trade/:id/move-stop", s.handleMoveStop)
	call := func(form url.Values) (int, map[string]any) {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/trade/92/move-stop", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// Preview: nothing sent, the current stop is found (the LIMIT is not a stop).
	code, out := call(url.Values{"stop": {"80.09"}})
	if code != http.StatusOK || out["stage"] != "preview" {
		t.Fatalf("preview = %d %v", code, out)
	}
	cur, _ := out["current"].([]any)
	if len(cur) != 1 || cur[0].(map[string]any)["id"] != "111" {
		t.Fatalf("current stops = %v, want only 111", cur)
	}
	// Hand-traced: locked 0.5*1.576/1.224 = 0.64379; remainder 0.5*(80.09-80.424)/1.224 = -0.13644.
	if got := out["r_if_stopped"].(float64); math.Abs(got-(0.5*1.576/1.224+0.5*(80.09-80.424)/1.224)) > 1e-9 {
		t.Errorf("r_if_stopped = %v", got)
	}

	// A stop the mark (81.2) has already passed is refused before anything is sent.
	if code, out := call(url.Values{"stop": {"81.5"}, "confirm": {"1"}}); code != http.StatusBadRequest || out["stage"] != "refused" {
		t.Errorf("stop above mark: %d %v", code, out)
	}

	code, out = call(url.Values{"stop": {"80.09"}, "confirm": {"1"}})
	if code != http.StatusOK || !strings.HasPrefix(out["new_stop_id"].(string), "DRY-RUN") {
		t.Fatalf("confirm = %d %v", code, out)
	}
	if c, _ := out["cancelled"].([]any); len(c) != 1 || c[0] != "111" {
		t.Errorf("cancelled = %v, want [111]", out["cancelled"])
	}
	got, _ := journal.ReadAll("")
	if got[0].Stop != 79.2 {
		t.Errorf("journal stop = %v — the R baseline must not move", got[0].Stop)
	}
	if got[0].StopOrderID != out["new_stop_id"] {
		t.Errorf("stop_order_id = %q, want the new order", got[0].StopOrderID)
	}
}
