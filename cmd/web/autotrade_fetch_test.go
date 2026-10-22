package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/henry190927/trading-bot/bingx"
)

// klineStub fails the first `fails` requests (alternating a 503 and an empty
// list), then answers with two bars.
func klineStub(t *testing.T, fails int32) (*bingx.Client, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := atomic.AddInt32(&n, 1)
		switch {
		case i <= fails && i%2 == 1:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case i <= fails:
			_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":[` +
				`{"open":"1","close":"2","high":"2","low":"1","volume":"1","time":1792580400000},` +
				`{"open":"2","close":"3","high":"3","low":"2","volume":"1","time":1792584000000}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &bingx.Client{Host: srv.URL, HTTP: srv.Client()}, &n
}

func TestKlinesWithRetryRecovers(t *testing.T) {
	c, n := klineStub(t, 2) // 503, then empty, then data
	cs, err := klinesWithRetry(t.Context(), c, "BTC", "1h", 500)
	if err != nil || len(cs) != 2 || *n != 3 {
		t.Fatalf("got %d candles, err %v, after %d calls; want 2 candles after 3", len(cs), err, *n)
	}
}

func TestKlinesWithRetryReportsFailure(t *testing.T) {
	c, n := klineStub(t, 99)
	cs, err := klinesWithRetry(t.Context(), c, "BTC", "1h", 500)
	if err == nil || cs != nil || *n != 3 {
		t.Fatalf("got %d candles, err %v, after %d calls; want an error after 3", len(cs), err, *n)
	}
}
