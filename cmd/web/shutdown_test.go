package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// http.Server.Shutdown waits for every handler to return, and a healthy SSE
// stream never returns on its own. Without closeSubscribers registered as an
// OnShutdown hook, Shutdown below runs into its deadline with the stream
// still open — the deploy would then wait out compose's stop_grace_period and
// get SIGKILLed.
func TestShutdownEndsOpenSSEStreams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newTestHub()
	h.syms = nil // no exchange websocket in a test
	s := &server{hub: h}
	r := gin.New()
	r.GET("/api/stream/tickers", s.handleStreamTickers)

	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: time.Second}
	srv.RegisterOnShutdown(h.closeSubscribers)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+lis.Addr().String()+"/api/stream/tickers", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	waitSubscribers(t, h, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v — an open SSE stream kept the server from draining", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Shutdown took %v; the stream should end at once", el)
	}

	// The client sees the stream end rather than hang.
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client stream still open after Shutdown")
	}
	<-served

	h.mu.Lock()
	n, running := len(h.subs), h.running
	h.mu.Unlock()
	if n != 0 || running {
		t.Fatalf("after shutdown: %d subscribers, loops running=%v", n, running)
	}
}

// A release func that runs after closeSubscribers must not close the channel
// a second time (which would panic) or restart anything.
func TestReleaseAfterCloseSubscribersIsSafe(t *testing.T) {
	h := newTestHub()
	h.syms = nil
	ch, _, release := h.subscribe()
	h.closeSubscribers()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel still open after closeSubscribers")
	}
	release() // must not panic
	h.mu.Lock()
	running := h.running
	h.mu.Unlock()
	if running {
		t.Fatal("loops restarted by a late release")
	}
}

func TestStartGRPCUnconfiguredIsAlreadyDone(t *testing.T) {
	t.Setenv("GRPC_BIND", "")
	select {
	case <-startGRPC(t.Context(), newTestHub()):
	default:
		t.Fatal("startGRPC without GRPC_BIND returned an open channel; main would wait on it")
	}
}

func waitSubscribers(t *testing.T, h *tickerHub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.subs)
		h.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber never attached (want %d)", want)
}
