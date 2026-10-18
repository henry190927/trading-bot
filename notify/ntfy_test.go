package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNtfyMuted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	muted := true
	n := NewNtfy(srv.URL, "topic")
	n.Muted = func() bool { return muted }

	if err := n.Push(context.Background(), "t", "b", ""); err != nil {
		t.Fatalf("muted push: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("muted push reached the server %d time(s)", got)
	}

	muted = false // flipped at runtime, no new Ntfy
	if err := n.Push(context.Background(), "t", "b", ""); err != nil {
		t.Fatalf("unmuted push: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("unmuted push: %d server hits, want 1", got)
	}
}
