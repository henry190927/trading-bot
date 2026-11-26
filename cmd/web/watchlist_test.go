package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func watchRouter(t *testing.T) (*gin.Engine, *server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchlist.json")
	t.Setenv("WATCHLIST_PATH", path)
	gin.SetMode(gin.TestMode)
	s := &server{venues: testCatalog(t)}
	r := gin.New()
	r.GET("/api/watchlist", s.handleWatchlistGet)
	r.POST("/api/watchlist", s.handleWatchlistPost)
	r.GET("/api/instruments", s.handleInstruments)
	return r, s, path
}

type watchResp struct {
	Items []symItem `json:"items"`
	Error string    `json:"error"`
}

func watchDo(t *testing.T, r *gin.Engine, action, sym string) (int, watchResp) {
	t.Helper()
	body := fmt.Sprintf(`{"symbol":%q,"action":%q}`, sym, action)
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/watchlist", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out watchResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body %q", action, sym, w.Body.String())
	}
	return w.Code, out
}

func keysOf(items []symItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

func TestWatchlistAddRemove(t *testing.T) {
	r, s, _ := watchRouter(t)

	code, got := watchDo(t, r, "add", "BINGX:ncsktsla2usd-usdt")
	if code != 200 || len(got.Items) != 1 {
		t.Fatalf("add TSLA: %d %+v", code, got)
	}
	if it := got.Items[0]; it.Key != "bingx:NCSKTSLA2USD-USDT" || it.Display != "TSLA" || it.Tradable {
		t.Errorf("TSLA stored as %+v", it)
	}
	// A list symbol is stored under its short name, however it is spelled.
	if _, got = watchDo(t, r, "add", "bingx:BTC-USDT"); strings.Join(keysOf(got.Items), ",") != "bingx:NCSKTSLA2USD-USDT,BTC" {
		t.Errorf("after BTC: %v", keysOf(got.Items))
	}
	// Adding the same symbol under another spelling is a no-op.
	if _, got = watchDo(t, r, "add", "NCSKTSLA2USD-USDT"); len(got.Items) != 2 {
		t.Errorf("duplicate added: %v", keysOf(got.Items))
	}
	if code, got = watchDo(t, r, "add", "TSLA"); code != 400 || !strings.Contains(got.Error, "unknown symbol") {
		t.Errorf("display name accepted: %d %+v", code, got)
	}
	if code, _ = watchDo(t, r, "flip", "BTC"); code != 400 {
		t.Errorf("bad action: %d", code)
	}

	// Only off-list entries become extra ticker rows.
	if tk := s.watchTickerKeys(); len(tk) != 1 || tk[0].Key != "bingx:NCSKTSLA2USD-USDT" {
		t.Errorf("watchTickerKeys = %+v", tk)
	}

	if _, got = watchDo(t, r, "remove", "bingx:NCSKTSLA2USD-USDT"); strings.Join(keysOf(got.Items), ",") != "BTC" {
		t.Errorf("after remove: %v", keysOf(got.Items))
	}
	if _, got = watchDo(t, r, "remove", "ETH"); len(got.Items) != 1 { // not on it: no-op
		t.Errorf("remove absent: %v", keysOf(got.Items))
	}
}

// A stored key that no longer resolves (delisted) is shown as stale and can
// still be removed; the cap holds.
func TestWatchlistStaleAndCap(t *testing.T) {
	r, _, path := watchRouter(t)
	if err := os.WriteFile(path, []byte(`{"items":["bingx:GONE-USDT","BTC"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/api/watchlist", nil))
	var got watchResp
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 2 || !got.Items[0].Stale || got.Items[1].Stale {
		t.Fatalf("stale not flagged: %+v", got.Items)
	}
	if _, got = watchDo(t, r, "remove", "bingx:GONE-USDT"); strings.Join(keysOf(got.Items), ",") != "BTC" {
		t.Errorf("stale not removable: %v", keysOf(got.Items))
	}

	var full []string
	for i := range maxWatch {
		full = append(full, fmt.Sprintf("K%d", i))
	}
	b, _ := json.Marshal(watchFile{Items: full})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, got := watchDo(t, r, "add", "BTC"); code != 400 || !strings.Contains(got.Error, "full") {
		t.Errorf("cap not enforced: %d %+v", code, got)
	}
}

func TestInstrumentsSearch(t *testing.T) {
	r, _, _ := watchRouter(t)
	get := func(q string) []symItem {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/api/instruments?q="+q, nil))
		var out watchResp
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Items
	}
	if it := get("tsla"); len(it) != 1 || it[0].Key != "bingx:NCSKTSLA2USD-USDT" || it[0].Tradable || it[0].Class != "us-stock" {
		t.Errorf("tsla: %+v", it)
	}
	if it := get("btc"); len(it) != 1 || it[0].Key != "BTC" || !it[0].Tradable {
		t.Errorf("btc: %+v", it)
	}
	if it := get("zzzz"); it == nil || len(it) != 0 {
		t.Errorf("no match must be an empty list, got %+v", it)
	}

	// Without a catalog the search still finds list symbols.
	bare := &server{}
	rr := gin.New()
	rr.GET("/api/instruments", bare.handleInstruments)
	w := httptest.NewRecorder()
	rr.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/api/instruments?q=et", nil))
	var out watchResp
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Items) != 1 || out.Items[0].Key != "ETH" {
		t.Errorf("no-catalog search: %+v", out.Items)
	}
}
